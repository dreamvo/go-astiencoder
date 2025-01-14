package astilibav

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/asticode/go-astiencoder"
	"github.com/asticode/go-astikit"
)

var countFrameRateEmulator uint64

type frameRateEmulatorPTSReference struct {
	pts  int64
	time time.Time
}

type FrameRateEmulator struct {
	*astiencoder.BaseNode
	c                   *astikit.Chan
	d                   *frameDispatcher
	eh                  *astiencoder.EventHandler
	outputCtx           Context
	p                   *framePool
	ptsReference        frameRateEmulatorPTSReference
	r                   *rateEmulator
	statFramesProcessed uint64
	statFramesReceived  uint64
}

type PTSReference struct {
	PTS      int64
	Time     time.Time
	TimeBase astiav.Rational
}

type FrameRateEmulatorOptions struct {
	FlushOnStop  bool
	Node         astiencoder.NodeOptions
	OutputCtx    Context
	PTSReference PTSReference
}

func NewFrameRateEmulator(o FrameRateEmulatorOptions, eh *astiencoder.EventHandler, c *astikit.Closer, s *astiencoder.Stater) (r *FrameRateEmulator) {
	// Extend node metadata
	count := atomic.AddUint64(&countFrameRateEmulator, uint64(1))
	o.Node.Metadata = o.Node.Metadata.Extend(fmt.Sprintf("frame_rate_emulator_%d", count), fmt.Sprintf("Frame Rate Emulator #%d", count), "Emulates frame rate", "frame rate emulator")

	// Create frame rate emulator
	r = &FrameRateEmulator{
		c:         astikit.NewChan(astikit.ChanOptions{ProcessAll: true}),
		eh:        eh,
		outputCtx: o.OutputCtx,
		ptsReference: frameRateEmulatorPTSReference{
			pts:  astiav.RescaleQ(o.PTSReference.PTS, o.PTSReference.TimeBase, o.OutputCtx.TimeBase),
			time: o.PTSReference.Time,
		},
	}

	// Create base node
	r.BaseNode = astiencoder.NewBaseNode(o.Node, c, eh, s, r, astiencoder.EventTypeToNodeEventName)

	// Create frame pool
	r.p = newFramePool(r)

	// Create frame dispatcher
	r.d = newFrameDispatcher(r, eh)

	// Create rate emulator
	r.r = newRateEmulator(o.FlushOnStop, r.rateEmulatorAt, r.rateEmulatorBefore, r.rateEmulatorExec)

	// Add stat options
	r.addStatOptions()
	return
}

type FrameRateEmulatorStats struct {
	FramesAllocated uint64
	FramesDispached uint64
	FramesProcessed uint64
	FramesReceived  uint64
	WorkDuration    time.Duration
}

func (r *FrameRateEmulator) Stats() FrameRateEmulatorStats {
	return FrameRateEmulatorStats{
		FramesAllocated: r.p.stats().framesAllocated,
		FramesDispached: r.d.stats().framesDispatched,
		FramesProcessed: atomic.LoadUint64(&r.statFramesProcessed),
		FramesReceived:  atomic.LoadUint64(&r.statFramesReceived),
		WorkDuration:    r.c.Stats().WorkDuration,
	}
}

func (r *FrameRateEmulator) addStatOptions() {
	// Get stats
	ss := r.c.StatOptions()
	ss = append(ss, r.d.statOptions()...)
	ss = append(ss, r.p.statOptions()...)
	ss = append(ss,
		astikit.StatOptions{
			Metadata: &astikit.StatMetadata{
				Description: "Number of frames coming in per second",
				Label:       "Incoming rate",
				Name:        StatNameIncomingRate,
				Unit:        "fps",
			},
			Valuer: astikit.NewAtomicUint64RateStat(&r.statFramesReceived),
		},
		astikit.StatOptions{
			Metadata: &astikit.StatMetadata{
				Description: "Number of frames processed per second",
				Label:       "Processed rate",
				Name:        StatNameProcessedRate,
				Unit:        "fps",
			},
			Valuer: astikit.NewAtomicUint64RateStat(&r.statFramesProcessed),
		},
	)

	// Add stats
	r.BaseNode.AddStats(ss...)
}

func (r *FrameRateEmulator) SetFlushOnStop(flushOnStop bool) {
	r.r.setFlushOnStop(flushOnStop)
}

// OutputCtx returns the output ctx
func (r *FrameRateEmulator) OutputCtx() Context {
	return r.outputCtx
}

// Connect implements the FrameHandlerConnector interface
func (r *FrameRateEmulator) Connect(h FrameHandler) {
	// Add handler
	r.d.addHandler(h)

	// Connect nodes
	astiencoder.ConnectNodes(r, h)
}

// Disconnect implements the FrameHandlerConnector interface
func (r *FrameRateEmulator) Disconnect(h FrameHandler) {
	// Delete handler
	r.d.delHandler(h)

	// Disconnect nodes
	astiencoder.DisconnectNodes(r, h)
}

// Start starts the frame rate emulator
func (r *FrameRateEmulator) Start(ctx context.Context, t astiencoder.CreateTaskFunc) {
	r.BaseNode.Start(ctx, t, func(t *astikit.Task) {
		// Make sure to stop the chan properly
		defer r.c.Stop()

		// Prepare waiting group
		wg := &sync.WaitGroup{}
		wg.Add(1)

		// Run rate emulator in goroutine
		go func() {
			// Make sure to decrement waiting group
			defer wg.Done()

			// Make sure to stop rate emulator properly
			defer r.r.stop()

			// Start rate emulator
			r.r.start(r.Context())
		}()

		// Start chan
		r.c.Start(r.Context())

		// Wait for rate emulator
		wg.Wait()
	})
}

type frameRateEmulatorItem struct {
	d Descriptor
	f *astiav.Frame
}

// HandleFrame implements the FrameHandler interface
func (r *FrameRateEmulator) HandleFrame(p FrameHandlerPayload) {
	// Everything executed outside the main loop should be protected from the closer
	r.DoWhenUnclosed(func() {
		// Increment received frames
		atomic.AddUint64(&r.statFramesReceived, 1)

		// Copy frame
		f := r.p.get()
		if err := f.Ref(p.Frame); err != nil {
			emitError(r, r.eh, err, "refing frame")
			return
		}

		// Add to chan
		r.c.Add(func() {
			// Everything executed outside the main loop should be protected from the closer
			r.DoWhenUnclosed(func() {
				// Handle pause
				defer r.HandlePause()

				// Increment processed frames
				atomic.AddUint64(&r.statFramesProcessed, 1)

				// Add to rate emulator
				r.r.add(&frameRateEmulatorItem{
					d: p.Descriptor,
					f: f,
				})
			})
		})
	})
}

func (r *FrameRateEmulator) rateEmulatorAt(i interface{}) time.Time {
	return r.ptsReference.time.Add(time.Duration(astiav.RescaleQ(i.(*frameRateEmulatorItem).f.Pts()-r.ptsReference.pts, r.outputCtx.TimeBase, nanosecondRational)))
}

func (r *FrameRateEmulator) rateEmulatorBefore(a, b interface{}) bool {
	return a.(*frameRateEmulatorItem).f.Pts() < b.(*frameRateEmulatorItem).f.Pts()
}

func (r *FrameRateEmulator) rateEmulatorExec(i interface{}) {
	// Dispatch
	r.d.dispatch(i.(*frameRateEmulatorItem).f, i.(*frameRateEmulatorItem).d)

	// Close frame
	r.p.put(i.(*frameRateEmulatorItem).f)
}
