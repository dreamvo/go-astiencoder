package astilibav

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/asticode/go-astiencoder"
	"github.com/asticode/go-astikit"
)

var countRateEnforcer uint64

// RateEnforcer represents an object capable of enforcing rate based on PTS
type RateEnforcer struct {
	*astiencoder.BaseNode
	adaptSlotsToIncomingFrames bool
	buf                        []*rateEnforcerItem
	c                          *astikit.Chan
	d                          *frameDispatcher
	descriptor                 Descriptor
	eh                         *astiencoder.EventHandler
	f                          RateEnforcerFiller
	m                          *sync.Mutex
	n                          astiencoder.Node
	outputCtx                  Context
	p                          *framePool
	period                     time.Duration
	restamper                  FrameRestamper
	slotsCount                 int
	slots                      []*rateEnforcerSlot
	statDelayAvg               *astikit.CounterAvgStat
	statFilledRate             *astikit.CounterRateStat
	statIncomingRate           *astikit.CounterRateStat
	statProcessedRate          *astikit.CounterRateStat
}

type rateEnforcerSlot struct {
	i      *rateEnforcerItem
	n      astiencoder.Node
	ptsMax int64
	ptsMin int64
}

type rateEnforcerItem struct {
	f *astiav.Frame
	n astiencoder.Node
}

// RateEnforcerOptions represents rate enforcer options
type RateEnforcerOptions struct {
	AdaptSlotsToIncomingFrames bool
	// Expressed in number of frames in the output framerate
	Delay  uint
	Filler RateEnforcerFiller
	Node   astiencoder.NodeOptions
	// Both FrameRate and TimeBase are mandatory
	OutputCtx Context
	Restamper FrameRestamper
}

// NewRateEnforcer creates a new rate enforcer
func NewRateEnforcer(o RateEnforcerOptions, eh *astiencoder.EventHandler, c *astikit.Closer, s *astiencoder.Stater) (r *RateEnforcer) {
	// Extend node metadata
	count := atomic.AddUint64(&countRateEnforcer, uint64(1))
	o.Node.Metadata = o.Node.Metadata.Extend(fmt.Sprintf("rate_enforcer_%d", count), fmt.Sprintf("Rate Enforcer #%d", count), "Enforces rate", "rate enforcer")

	// Create rate enforcer
	r = &RateEnforcer{
		adaptSlotsToIncomingFrames: o.AdaptSlotsToIncomingFrames,
		c:                          astikit.NewChan(astikit.ChanOptions{ProcessAll: true}),
		descriptor:                 o.OutputCtx.Descriptor(),
		eh:                         eh,
		f:                          o.Filler,
		m:                          &sync.Mutex{},
		outputCtx:                  o.OutputCtx,
		period:                     time.Duration(float64(1e9) / o.OutputCtx.FrameRate.ToDouble()),
		restamper:                  o.Restamper,
		slots:                      []*rateEnforcerSlot{nil},
		slotsCount:                 int(math.Max(float64(o.Delay), 1)),
		statDelayAvg:               astikit.NewCounterAvgStat(),
		statFilledRate:             astikit.NewCounterRateStat(),
		statIncomingRate:           astikit.NewCounterRateStat(),
		statProcessedRate:          astikit.NewCounterRateStat(),
	}

	// Create base node
	r.BaseNode = astiencoder.NewBaseNode(o.Node, c, eh, s, r, astiencoder.EventTypeToNodeEventName)

	// Create frame pool
	r.p = newFramePool(r)

	// Create frame dispatcher
	r.d = newFrameDispatcher(r, eh)

	// Create filler
	if r.f == nil {
		r.f = newPreviousRateEnforcerFiller(r, r.eh, r.p)
	}

	// Add stats
	r.addStats()
	return
}

func (r *RateEnforcer) addStats() {
	// Get stats
	ss := r.c.Stats()
	ss = append(ss, r.d.stats()...)
	ss = append(ss,
		astikit.StatOptions{
			Handler: r.statIncomingRate,
			Metadata: &astikit.StatMetadata{
				Description: "Number of frames coming in per second",
				Label:       "Incoming rate",
				Name:        StatNameIncomingRate,
				Unit:        "fps",
			},
		},
		astikit.StatOptions{
			Handler: r.statProcessedRate,
			Metadata: &astikit.StatMetadata{
				Description: "Number of frames processed per second",
				Label:       "Processed rate",
				Name:        StatNameProcessedRate,
				Unit:        "fps",
			},
		},
		astikit.StatOptions{
			Handler: r.statDelayAvg,
			Metadata: &astikit.StatMetadata{
				Description: "Average delay of frames coming in",
				Label:       "Average delay",
				Name:        StatNameAverageDelay,
				Unit:        "ns",
			},
		},
		astikit.StatOptions{
			Handler: r.statFilledRate,
			Metadata: &astikit.StatMetadata{
				Description: "Number of frames filled per second",
				Label:       "Filled rate",
				Name:        StatNameFilledRate,
				Unit:        "fps",
			},
		},
	)

	// Add stats
	r.BaseNode.AddStats(ss...)
}

// OutputCtx returns the output ctx
func (r *RateEnforcer) OutputCtx() Context {
	return r.outputCtx
}

// Switch switches the source
func (r *RateEnforcer) Switch(n astiencoder.Node) {
	r.m.Lock()
	defer r.m.Unlock()
	r.n = n
}

// Connect implements the FrameHandlerConnector interface
func (r *RateEnforcer) Connect(h FrameHandler) {
	// Add handler
	r.d.addHandler(h)

	// Connect nodes
	astiencoder.ConnectNodes(r, h)
}

// Disconnect implements the FrameHandlerConnector interface
func (r *RateEnforcer) Disconnect(h FrameHandler) {
	// Delete handler
	r.d.delHandler(h)

	// Disconnect nodes
	astiencoder.DisconnectNodes(r, h)
}

// Start starts the rate enforcer
func (r *RateEnforcer) Start(ctx context.Context, t astiencoder.CreateTaskFunc) {
	r.BaseNode.Start(ctx, t, func(t *astikit.Task) {
		// Make sure to stop the chan properly
		defer r.c.Stop()

		// Start tick
		startTickCtx := r.startTick(r.Context())

		// Start chan
		r.c.Start(r.Context())

		// Wait for start tick to be really over since it's not the blocking pattern
		// and is executed in a goroutine
		<-startTickCtx.Done()
	})
}

// HandleFrame implements the FrameHandler interface
func (r *RateEnforcer) HandleFrame(p FrameHandlerPayload) {
	// Everything executed outside the main loop should be protected from the closer
	r.DoWhenUnclosed(func() {
		// Increment incoming rate
		r.statIncomingRate.Add(1)

		// Copy frame
		f := r.p.get()
		if err := f.Ref(p.Frame); err != nil {
			emitError(r, r.eh, err, "refing frame")
			return
		}

		// Restamp
		f.SetPts(astiav.RescaleQ(f.Pts(), p.Descriptor.TimeBase(), r.outputCtx.TimeBase))

		// Add to chan
		r.c.Add(func() {
			// Everything executed outside the main loop should be protected from the closer
			r.DoWhenUnclosed(func() {
				// Handle pause
				defer r.HandlePause()

				// Make sure to close frame
				defer r.p.put(f)

				// Increment processed rate
				r.statProcessedRate.Add(1)

				// Lock
				r.m.Lock()
				defer r.m.Unlock()

				// Get last slot
				l := r.slots[len(r.slots)-1]

				// We update the last slot if:
				//   - it's empty
				//   - its node is different from the desired node AND the payload's node is the desired
				//   node. That way, if the desired node doesn't dispatch frames for some time, we fallback to the previous
				//   node instead of the previous item
				//   - it's in the past compared to current frame and developer wants to adapt slots to incoming frames
				if c1, c2 := l == nil || (r.n != l.n && r.n == p.Node), l != nil && l.n == p.Node && l.ptsMax < f.Pts(); c1 || c2 {
					// Update last slot
					if c1 || (c2 && r.adaptSlotsToIncomingFrames) {
						r.slots[len(r.slots)-1] = r.newRateEnforcerSlot(f)
					}

					// Emit event
					if c1 {
						r.eh.Emit(astiencoder.Event{
							Name:    EventNameRateEnforcerSwitchedIn,
							Payload: p.Node,
							Target:  r,
						})
					}
				}

				// Create item
				i := newRateEnforcerItem(nil, p.Node)

				// Copy frame
				i.f = r.p.get()
				if err := i.f.Ref(f); err != nil {
					emitError(r, r.eh, err, "refing frame")
					return
				}

				// Append item
				r.buf = append(r.buf, i)

				// Process delay stat
				if l != nil && l.n == i.n {
					r.statDelayAvg.Add(float64(time.Duration(astiav.RescaleQ(l.ptsMax-i.f.Pts(), r.outputCtx.TimeBase, nanosecondRational))))
				}
			})
		})
	})
}

func (r *RateEnforcer) newRateEnforcerSlot(f *astiav.Frame) *rateEnforcerSlot {
	return &rateEnforcerSlot{
		n:      r.n,
		ptsMax: f.Pts() + int64(1/(r.outputCtx.TimeBase.ToDouble()*r.outputCtx.FrameRate.ToDouble())),
		ptsMin: f.Pts(),
	}
}

func (s *rateEnforcerSlot) next() *rateEnforcerSlot {
	return &rateEnforcerSlot{
		n:      s.n,
		ptsMin: s.ptsMax,
		ptsMax: s.ptsMax - s.ptsMin + s.ptsMax,
	}
}

func newRateEnforcerItem(f *astiav.Frame, n astiencoder.Node) *rateEnforcerItem {
	return &rateEnforcerItem{
		f: f,
		n: n,
	}
}

func (r *RateEnforcer) startTick(parentCtx context.Context) (ctx context.Context) {
	// Create independant context that only captures when the following goroutine ends
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(context.Background())

	// Execute the rest in a go routine
	go func() {
		// Make sure to cancel local context
		defer cancel()

		// Loop
		nextAt := time.Now()
		var previousNode astiencoder.Node
		for {
			if stop := r.tickFunc(parentCtx, &nextAt, &previousNode); stop {
				return
			}
		}
	}()
	return
}

func (r *RateEnforcer) tickFunc(ctx context.Context, nextAt *time.Time, previousNode *astiencoder.Node) (stop bool) {
	// Compute next at
	*nextAt = nextAt.Add(r.period)

	// Sleep until next at
	if delta := time.Until(*nextAt); delta > 0 {
		astikit.Sleep(ctx, delta) //nolint:errcheck
	}

	// Check context
	if ctx.Err() != nil {
		return true
	}

	// Lock
	r.m.Lock()
	defer r.m.Unlock()

	// Make sure to remove first slot AFTER adding next slot, so that when there's only
	// one slot, we still can get the .next() slot
	removeFirstSlot := true
	defer func(b *bool) {
		if *b {
			r.slots = r.slots[1:]
		}
	}(&removeFirstSlot)

	// Make sure to add next slot
	defer func() {
		var s *rateEnforcerSlot
		if ps := r.slots[len(r.slots)-1]; ps != nil {
			s = ps.next()
		}
		r.slots = append(r.slots, s)
	}()

	// Not enough slots
	if len(r.slots) < r.slotsCount {
		removeFirstSlot = false
		return
	}

	// Distribute
	r.distribute()

	// Dispatch
	i, filled := r.current()
	if i != nil {
		// Restamp frame
		if r.restamper != nil {
			r.restamper.Restamp(i.f)
		}

		// Dispatch frame
		r.d.dispatch(i.f, r.descriptor)

		// Frame is coming from an actual node
		if i.n != nil {
			// New node has been dispatched
			if *previousNode != i.n {
				// Emit event
				r.eh.Emit(astiencoder.Event{
					Name:    EventNameRateEnforcerSwitchedOut,
					Payload: i.n,
					Target:  r,
				})

				// Update previous node
				*previousNode = i.n
			}
		}
	}

	// Frame has been filled
	if filled {
		r.statFilledRate.Add(1)
	} else {
		r.p.put(i.f)
	}
	return
}

func (r *RateEnforcer) distribute() {
	// Get useful nodes
	ns := make(map[astiencoder.Node]bool)
	for _, s := range r.slots {
		if s != nil && s.n != nil {
			ns[s.n] = true
		}
	}

	// Loop through slots
	for _, s := range r.slots {
		// Slot is empty or already has an item
		if s == nil || s.i != nil {
			continue
		}

		// Loop through buffer
		for idx := 0; idx < len(r.buf); idx++ {
			// Not the same node
			if r.buf[idx].n != s.n {
				// Node is useless
				if _, ok := ns[r.buf[idx].n]; !ok {
					r.p.put(r.buf[idx].f)
					r.buf = append(r.buf[:idx], r.buf[idx+1:]...)
					idx--
				}
				continue
			}

			// Add to slot or remove if pts is older
			if s.ptsMin <= r.buf[idx].f.Pts() && s.ptsMax > r.buf[idx].f.Pts() {
				if s.i == nil {
					s.i = r.buf[idx]
				} else {
					r.p.put(r.buf[idx].f)
				}
				r.buf = append(r.buf[:idx], r.buf[idx+1:]...)
				idx--
				continue
			} else if s.ptsMin > r.buf[idx].f.Pts() {
				r.p.put(r.buf[idx].f)
				r.buf = append(r.buf[:idx], r.buf[idx+1:]...)
				idx--
				continue
			}
		}
	}
}

func (r *RateEnforcer) current() (i *rateEnforcerItem, filled bool) {
	if r.slots[0] != nil && r.slots[0].i != nil {
		// Get item
		i = r.slots[0].i

		// No fill
		r.f.NoFill(i.f, i.n)
	} else {
		// Fill
		if f, n := r.f.Fill(); f != nil {
			i = newRateEnforcerItem(f, n)
		}

		// Update filled
		filled = true
	}
	return
}

type RateEnforcerFiller interface {
	Fill() (*astiav.Frame, astiencoder.Node)
	NoFill(*astiav.Frame, astiencoder.Node)
}

type previousRateEnforcerFiller struct {
	eh     *astiencoder.EventHandler
	f      *astiav.Frame
	n      astiencoder.Node
	p      *framePool
	target interface{}
}

func newPreviousRateEnforcerFiller(target interface{}, eh *astiencoder.EventHandler, p *framePool) *previousRateEnforcerFiller {
	return &previousRateEnforcerFiller{
		eh:     eh,
		p:      p,
		target: target,
	}
}

func (f *previousRateEnforcerFiller) Fill() (*astiav.Frame, astiencoder.Node) {
	return f.f, f.n
}

func (f *previousRateEnforcerFiller) NoFill(fm *astiav.Frame, n astiencoder.Node) {
	// Store
	f.n = n

	// Create frame
	if f.f == nil {
		f.f = f.p.get()
	} else {
		f.f.Unref()
	}

	// Copy frame
	if err := f.f.Ref(fm); err != nil {
		emitError(f.target, f.eh, err, "refing frame")
		f.p.put(f.f)
		f.f = nil
	}
}

type frameRateEnforcerFiller struct {
	f *astiav.Frame
}

func NewFrameRateEnforcerFiller(fn func(fm *astiav.Frame) error, c *astikit.Closer) (f *frameRateEnforcerFiller, err error) {
	// Alloc frame
	fm := astiav.AllocFrame()

	// Make sure to free frame
	defer func(err *error) {
		if *err != nil {
			fm.Free()
		} else {
			c.Add(fm.Free)
		}
	}(&err)

	// Adapt frame
	if fn != nil {
		if err = fn(fm); err != nil {
			err = fmt.Errorf("astilibav: adapting frame failed: %w", err)
			return
		}
	}

	// Create filler
	f = &frameRateEnforcerFiller{
		f: fm,
	}
	return
}

func (f *frameRateEnforcerFiller) Fill() (*astiav.Frame, astiencoder.Node) {
	return f.f, nil
}

func (f *frameRateEnforcerFiller) NoFill(fm *astiav.Frame, n astiencoder.Node) {}
