package astilibav

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/asticode/go-astiencoder"
	"github.com/asticode/go-astikit"
)

var (
	countDemuxer       uint64
	nanosecondRational = astiav.NewRational(1, 1e9)
)

// Demuxer represents an object capable of demuxing packets out of an input
type Demuxer struct {
	*astiencoder.BaseNode
	d                     *pktDispatcher
	eh                    *astiencoder.EventHandler
	emulateRate           bool
	formatContext         *astiav.FormatContext
	interruptRet          *int
	l                     *demuxerLooper
	loop                  uint32
	p                     *pktPool
	readFrameErrorHandler DemuxerReadFrameErrorHandler
	ss                    map[int]*demuxerStream
	statIncomingRate      *astikit.CounterRateStat
}

type DemuxerReadFrameErrorHandler func(d *Demuxer, err error) (stop, handled bool)

type demuxerStream struct {
	ctx Context
	d   Descriptor
	e   *demuxerRateEmulator
	pd  *pktDurationer
	s   *astiav.Stream
}

func (d *demuxerStream) stream() *Stream {
	return &Stream{
		CodecParameters: d.s.CodecParameters(),
		Ctx:             d.ctx,
		ID:              d.s.ID(),
		Index:           d.s.Index(),
	}
}

// DemuxerOptions represents demuxer options
type DemuxerOptions struct {
	// String content of the demuxer as you would use in ffmpeg
	Dictionary *Dictionary
	// If true, the demuxer will sleep between packets for the exact duration of the packet
	EmulateRate bool
	// Max duration of continuous packets of the same stream the demuxer receives has to be
	// inferior to this value.
	// Defaults to 1s
	EmulateRateBufferDuration time.Duration
	// Exact input format
	Format *astiav.InputFormat
	// If true, at the end of the input the demuxer will seek to its beginning and start over
	// In this case the packets are restamped
	Loop bool
	// Basic node options
	Node astiencoder.NodeOptions
	// Context used to cancel probing
	ProbeCtx context.Context
	// Custom read frame error handler
	// If handled is false, default error handling will be executed
	ReadFrameErrorHandler DemuxerReadFrameErrorHandler
	// URL of the input
	URL string
}

// NewDemuxer creates a new demuxer
func NewDemuxer(o DemuxerOptions, eh *astiencoder.EventHandler, c *astikit.Closer, s *astiencoder.Stater) (d *Demuxer, err error) {
	// Extend node metadata
	count := atomic.AddUint64(&countDemuxer, uint64(1))
	o.Node.Metadata = o.Node.Metadata.Extend(fmt.Sprintf("demuxer_%d", count), fmt.Sprintf("Demuxer #%d", count), fmt.Sprintf("Demuxes %s", o.URL), "demuxer")

	// Create demuxer
	d = &Demuxer{
		eh:                    eh,
		emulateRate:           o.EmulateRate,
		readFrameErrorHandler: o.ReadFrameErrorHandler,
		ss:                    make(map[int]*demuxerStream),
		statIncomingRate:      astikit.NewCounterRateStat(),
	}

	// Create base node
	d.BaseNode = astiencoder.NewBaseNode(o.Node, c, eh, s, d, astiencoder.EventTypeToNodeEventName)

	// Update loop
	if o.Loop {
		d.loop = 1
	}

	// Create pkt pool
	d.p = newPktPool(d)

	// Create pkt dispatcher
	d.d = newPktDispatcher(d, eh, d.p)

	// Add stats
	d.addStats()

	// Dictionary
	var dict *astiav.Dictionary
	if o.Dictionary != nil {
		// Parse dict
		if dict, err = o.Dictionary.parse(); err != nil {
			err = fmt.Errorf("astilibav: parsing dict failed: %w", err)
			return
		}

		// Make sure the dictionary is freed
		defer dict.Free()
	}

	// Alloc format context
	d.formatContext = astiav.AllocFormatContext()

	// Make sure the format context is properly freed
	d.AddClose(d.formatContext.Free)

	// Set interrupt callback
	d.interruptRet = d.formatContext.SetInterruptCallback()

	// Handle probe cancellation
	if o.ProbeCtx != nil {
		// Create context
		probeCtx, probeCancel := context.WithCancel(o.ProbeCtx)

		// Handle interrupt
		*d.interruptRet = 0
		go func() {
			<-probeCtx.Done()
			if o.ProbeCtx.Err() != nil {
				*d.interruptRet = 1
			}
		}()

		// Make sure to cancel context so that go routine is closed
		defer probeCancel()
	}

	// Open input
	if err = d.formatContext.OpenInput(o.URL, o.Format, dict); err != nil {
		err = fmt.Errorf("astilibav: opening input failed: %w", err)
		return
	}

	// Make sure the input is properly closed
	d.AddClose(d.formatContext.CloseInput)

	// Check whether probe has been cancelled
	if o.ProbeCtx != nil && o.ProbeCtx.Err() != nil {
		err = fmt.Errorf("astilibav: probing has been cancelled: %w", o.ProbeCtx.Err())
		return
	}

	// Find stream information
	if err = d.formatContext.FindStreamInfo(nil); err != nil {
		err = fmt.Errorf("astilibav: finding stream info failed: %w", err)
		return
	}

	// Check whether probe has been cancelled
	if o.ProbeCtx != nil && o.ProbeCtx.Err() != nil {
		err = fmt.Errorf("astilibav: probing has been cancelled: %w", o.ProbeCtx.Err())
		return
	}

	// Loop through streams
	for _, s := range d.formatContext.Streams() {
		// Create demuxer stream
		ds := &demuxerStream{
			ctx: NewContextFromStream(s),
			s:   s,
		}
		ds.d = ds.ctx.Descriptor()

		// Create rate emulator
		ds.e = newDemuxerRateEmulator(o.EmulateRateBufferDuration, d.d, d.eh, d.p, ds)

		// Create pkt durationer
		ds.pd = newPktDurationer(ds.ctx)

		// Store stream
		d.ss[s.Index()] = ds
	}

	// Create looper
	d.l = newDemuxerLooper(d.ss)
	return
}

func (d *Demuxer) addStats() {
	// Get stats
	ss := d.d.stats()
	ss = append(ss, astikit.StatOptions{
		Handler: d.statIncomingRate,
		Metadata: &astikit.StatMetadata{
			Description: "Number of bits going in per second",
			Label:       "Incoming rate",
			Name:        StatNameIncomingRate,
			Unit:        "bps",
		},
	})

	// Add stats
	d.BaseNode.AddStats(ss...)
}

func (d *Demuxer) SetLoop(loop bool) {
	var v uint32
	if loop {
		v = 1
	}
	atomic.StoreUint32(&d.loop, v)
}

// Streams returns the streams ordered by index
func (d *Demuxer) Streams() (ss []*Stream) {
	// Get indexes
	var idxs []int
	for idx := range d.ss {
		idxs = append(idxs, idx)
	}

	// Sort indexes
	sort.Ints(idxs)

	// Loop through indexes
	for _, idx := range idxs {
		ss = append(ss, d.ss[idx].stream())
	}
	return
}

// Connect implements the PktHandlerConnector interface
func (d *Demuxer) Connect(h PktHandler) {
	// Add handler
	d.d.addHandler(h)

	// Connect nodes
	astiencoder.ConnectNodes(d, h)
}

// Disconnect implements the PktHandlerConnector interface
func (d *Demuxer) Disconnect(h PktHandler) {
	// Delete handler
	d.d.delHandler(h)

	// Disconnect nodes
	astiencoder.DisconnectNodes(d, h)
}

// ConnectForStream connects the demuxer to a PktHandler for a specific stream
func (d *Demuxer) ConnectForStream(h PktHandler, i *Stream) {
	// Add handler
	d.d.addHandler(newPktCond(i, h))

	// Connect nodes
	astiencoder.ConnectNodes(d, h)
}

// DisconnectForStream disconnects the demuxer from a PktHandler for a specific stream
func (d *Demuxer) DisconnectForStream(h PktHandler, i *Stream) {
	// Delete handler
	d.d.delHandler(newPktCond(i, h))

	// Disconnect nodes
	astiencoder.DisconnectNodes(d, h)
}

// Start starts the demuxer
func (d *Demuxer) Start(ctx context.Context, t astiencoder.CreateTaskFunc) {
	d.BaseNode.Start(ctx, t, func(t *astikit.Task) {
		// Handle interrupt callback
		*d.interruptRet = 0
		go func() {
			<-d.Context().Done()
			*d.interruptRet = 1
		}()

		// Emulate rate
		wg := &sync.WaitGroup{}
		if d.emulateRate {
			// Loop through streams
			for _, s := range d.ss {
				// Execute the rest in a goroutine
				wg.Add(1)
				go func(e *demuxerRateEmulator) {
					// Make sure to mark task as done
					defer wg.Done()

					// Make sure to stop rate emulator
					defer e.stop()

					// Start rate emulator
					e.start(d.Context())
				}(s.e)
			}
		}

		// Loop
		for {
			// Read frame
			if stop := d.readFrame(d.Context()); stop {
				break
			}

			// Handle pause
			d.HandlePause()

			// Check context
			if d.Context().Err() != nil {
				break
			}
		}

		// Wait for rate emulators
		wg.Wait()

		// Reset looper
		d.l.reset()
	})
}

func (d *Demuxer) readFrame(ctx context.Context) (stop bool) {
	// Get pkt from pool
	pkt := d.p.get()
	defer d.p.put(pkt)

	// Read frame
	if err := d.formatContext.ReadFrame(pkt); err != nil {
		if atomic.LoadUint32(&d.loop) > 0 && errors.Is(err, astiav.ErrEof) {
			// Let the looper know we're looping
			d.l.looping()

			// Seek to start
			if err = d.formatContext.SeekFrame(-1, d.formatContext.StartTime(), astiav.NewSeekFlags(astiav.SeekFlagBackward)); err != nil {
				emitError(d, d.eh, err, "seeking to frame")
				stop = true
			}
		} else {
			// Let the rate emulators know we've reached EOF
			if d.emulateRate && errors.Is(err, astiav.ErrEof) {
				// Loop through streams
				for _, s := range d.ss {
					s.e.eof()
				}
			}

			// Custom error handler
			if d.readFrameErrorHandler != nil {
				var handled bool
				if stop, handled = d.readFrameErrorHandler(d, err); handled {
					return
				}
			}

			// Default error handling
			if !errors.Is(err, astiav.ErrEof) {
				emitError(d, d.eh, err, "reading frame")
			}
			stop = true
		}
		return
	}

	// Increment incoming rate
	d.statIncomingRate.Add(float64(pkt.Size() * 8))

	// Get stream
	s, ok := d.ss[pkt.StreamIndex()]
	if !ok {
		return
	}

	// Handle pkt duration
	previousDuration := s.pd.handlePkt(pkt)

	// Handle loop
	d.l.handlePkt(pkt, &previousDuration)

	// Emulate rate
	if d.emulateRate {
		// Handle rate emulation
		s.e.handlePkt(ctx, pkt, previousDuration)
	} else {
		// Dispatch pkt
		d.d.dispatch(pkt, s.d)
	}
	return
}

type demuxerLooper struct {
	ss map[int]*demuxerLooperStream // Indexed by stream index
}

type demuxerLooperStream struct {
	duration         int64
	durationD        time.Duration
	lastDuration     int64
	restampDelta     int64
	restampRemainder time.Duration
	s                *demuxerStream
}

func newDemuxerLooperStream(s *demuxerStream) *demuxerLooperStream {
	return &demuxerLooperStream{
		s: s,
	}
}

func newDemuxerLooper(ss map[int]*demuxerStream) (l *demuxerLooper) {
	// Create looper
	l = &demuxerLooper{
		ss: make(map[int]*demuxerLooperStream),
	}

	// Loop through streams
	for idx, s := range ss {
		l.ss[idx] = newDemuxerLooperStream(s)
	}
	return
}

func (l *demuxerLooper) reset() {
	for _, s := range l.ss {
		s.duration = 0
		s.lastDuration = 0
		s.restampRemainder = 0
	}
}

func (l *demuxerLooper) handlePkt(pkt *astiav.Packet, previousDuration *int64) {
	// Get stream
	s, ok := l.ss[pkt.StreamIndex()]
	if !ok {
		return
	}

	// Update durations
	if *previousDuration > 0 {
		s.duration += *previousDuration
	} else if s.lastDuration > 0 {
		*previousDuration = s.lastDuration
		s.lastDuration = 0
	}

	// Restamp
	if s.restampDelta > 0 {
		d := pkt.Pts() - pkt.Dts()
		pkt.SetDts(pkt.Dts() + s.restampDelta)
		pkt.SetPts(pkt.Dts() + d)
	}
}

func (l *demuxerLooper) looping() {
	// Get max duration
	var maxDuration time.Duration
	for _, s := range l.ss {
		// Flush pkt durationer
		s.lastDuration = s.s.pd.flush()

		// Update duration
		s.duration += s.lastDuration
		s.durationD = time.Duration(astiav.RescaleQ(s.duration, s.s.ctx.TimeBase, nanosecondRational))

		// Get max duration
		if s.durationD > maxDuration {
			maxDuration = s.durationD
		}
	}

	// Loop through streams
	for _, s := range l.ss {
		// Get delta duration
		d := maxDuration - s.durationD + s.restampRemainder

		// Process delta
		restampDelta := s.duration
		if d > 0 {
			// Convert duration to timebase
			var i int64
			i, s.restampRemainder = durationToTimeBase(d, s.s.ctx.TimeBase)

			// Use delta
			if i > 0 {
				restampDelta += i
				s.lastDuration += i
			}
		}

		// Update restamp delta
		s.restampDelta += restampDelta

		// Reset duration
		s.duration = 0
	}
}

type demuxerRateEmulator struct {
	bufferDuration time.Duration
	c              *astikit.Chan
	cancel         context.CancelFunc
	ctx            context.Context
	d              *pktDispatcher
	eh             *astiencoder.EventHandler
	lastAt         time.Time
	m              *sync.Mutex // Locks ps
	p              *pktPool
	ps             map[*astiav.Packet]bool
	s              *demuxerStream
}

func newDemuxerRateEmulator(bufferDuration time.Duration, d *pktDispatcher, eh *astiencoder.EventHandler, p *pktPool, s *demuxerStream) (e *demuxerRateEmulator) {
	e = &demuxerRateEmulator{
		bufferDuration: bufferDuration,
		c:              astikit.NewChan(astikit.ChanOptions{}),
		d:              d,
		eh:             eh,
		m:              &sync.Mutex{},
		p:              p,
		ps:             make(map[*astiav.Packet]bool),
		s:              s,
	}
	if e.bufferDuration <= 0 {
		e.bufferDuration = time.Second
	}
	return
}

func (e *demuxerRateEmulator) start(ctx context.Context) {
	// Create context
	e.ctx, e.cancel = context.WithCancel(ctx)

	// Start chan
	e.c.Start(e.ctx)

	// Unref remaining pkts
	e.m.Lock()
	for pkt := range e.ps {
		e.p.put(pkt)
	}
	e.ps = map[*astiav.Packet]bool{}
	e.m.Unlock()

	// Reset chan
	e.c.Reset()
}

func (e *demuxerRateEmulator) stop() {
	// Cancel context
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}

	// Make sure to stop chan
	e.c.Stop()
}

func (e *demuxerRateEmulator) handlePkt(ctx context.Context, in *astiav.Packet, previousDuration int64) {
	// Copy pkt
	pkt := e.p.get()
	if err := pkt.Ref(in); err != nil {
		emitError(e, e.eh, err, "refing packet")
		return
	}

	// Store pkt
	e.m.Lock()
	e.ps[pkt] = true
	e.m.Unlock()

	// Get pkt at
	pktAt := e.lastAt
	if pktAt.IsZero() {
		pktAt = time.Now()
	}

	// Process previous duration
	if previousDuration > 0 {
		pktAt = pktAt.Add(time.Duration(astiav.RescaleQ(previousDuration, e.s.ctx.TimeBase, nanosecondRational)))
	}

	// Add to chan
	e.c.Add(func() {
		// Make sure to clean pkt
		defer func() {
			// Put pkt
			e.p.put(pkt)

			// Unstore pkt
			e.m.Lock()
			delete(e.ps, pkt)
			e.m.Unlock()
		}()

		// Sleep
		if delta := time.Until(pktAt); delta > 0 {
			astikit.Sleep(e.ctx, delta) //nolint:errcheck
		}

		// Check context error
		if err := e.ctx.Err(); err != nil {
			return
		}

		// Dispatch
		e.d.dispatch(pkt, e.s.d)
	})

	// Update last at
	e.lastAt = pktAt

	// Too many pkts are buffered, demuxer needs to wait
	if delta := time.Until(e.lastAt) - e.bufferDuration; delta > 0 {
		// Sleep
		astikit.Sleep(ctx, delta) //nolint:errcheck
	}
}

func (e *demuxerRateEmulator) eof() {
	e.c.Add(func() { e.stop() })
}
