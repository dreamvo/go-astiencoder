package astilibav

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astitools/time"

	"github.com/asticode/go-astiencoder"
	"github.com/asticode/go-astitools/stat"
	"github.com/asticode/go-astitools/sync"
	"github.com/asticode/go-astitools/worker"
	"github.com/asticode/goav/avutil"
)

var countRateEnforcer uint64

// RateEnforcer represents an object capable of enforcing rate when switching between multiple inputs
type RateEnforcer struct {
	*astiencoder.BaseNode
	buf              []*rateEnforcerItem
	d                *frameDispatcher
	e                astiencoder.EventEmitter
	m                *sync.Mutex
	n                astiencoder.Node
	p                *framePool
	period           time.Duration
	previousItem     *rateEnforcerItem
	q                *astisync.CtxQueue
	restamper        FrameRestamper
	slotsCount       int
	slots            []*rateEnforcerSlot
	statIncomingRate *astistat.IncrementStat
	statWorkRatio    *astistat.DurationRatioStat
	timeBase         avutil.Rational
}

type rateEnforcerSlot struct {
	i      *rateEnforcerItem
	n      astiencoder.Node
	ptsMax int64
	ptsMin int64
}

type rateEnforcerItem struct {
	d Descriptor
	f *avutil.Frame
	n astiencoder.Node
}

// RateEnforcerOptions represents rate enforcer options
type RateEnforcerOptions struct {
	Delay     int
	FrameRate avutil.Rational
	Node astiencoder.NodeOptions
	Restamper FrameRestamper
}

// NewRateEnforcer creates a new rate enforcer
func NewRateEnforcer(o RateEnforcerOptions, e astiencoder.EventEmitter, c astiencoder.CloseFuncAdder) (r *RateEnforcer) {
	// Extend node metadata
	count := atomic.AddUint64(&countRateEnforcer, uint64(1))
	o.Node.Metadata = o.Node.Metadata.Extend(fmt.Sprintf("rate_enforcer_%d", count), fmt.Sprintf("Rate Enforcer #%d", count), "Enforces rate")

	// Create rate enforcer
	r = &RateEnforcer{
		e:                e,
		m:                &sync.Mutex{},
		p:                newFramePool(c),
		period:           time.Duration(float64(1e9) / o.FrameRate.ToDouble()),
		q:                astisync.NewCtxQueue(),
		restamper:        o.Restamper,
		slots:            []*rateEnforcerSlot{nil},
		slotsCount:       int(math.Max(float64(o.Delay), 1)),
		statIncomingRate: astistat.NewIncrementStat(),
		statWorkRatio:    astistat.NewDurationRatioStat(),
		timeBase:         avutil.NewRational(o.FrameRate.Den(), o.FrameRate.Num()),
	}
	r.BaseNode = astiencoder.NewBaseNode(o.Node, astiencoder.NewEventGeneratorNode(r), e)
	r.d = newFrameDispatcher(r, e, c)
	r.addStats()
	return
}

func (r *RateEnforcer) addStats() {
	// Add incoming rate
	r.Stater().AddStat(astistat.StatMetadata{
		Description: "Number of frames coming in per second",
		Label:       "Incoming rate",
		Unit:        "fps",
	}, r.statIncomingRate)

	// Add work ratio
	r.Stater().AddStat(astistat.StatMetadata{
		Description: "Percentage of time spent doing some actual work",
		Label:       "Work ratio",
		Unit:        "%",
	}, r.statWorkRatio)

	// Add dispatcher stats
	r.d.addStats(r.Stater())

	// Add queue stats
	r.q.AddStats(r.Stater())
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
	r.BaseNode.Start(ctx, t, func(t *astiworker.Task) {
		// Handle context
		go r.q.HandleCtx(r.Context())

		// Make sure to wait for all dispatcher subprocesses to be done so that they are properly closed
		defer r.d.wait()

		// Make sure to stop the queue properly
		defer r.q.Stop()

		// Start tick
		r.startTick(r.Context())

		// Start queue
		r.q.Start(func(dp interface{}) {
			// Handle pause
			defer r.HandlePause()

			// Assert payload
			p := dp.(*FrameHandlerPayload)

			// Increment incoming rate
			r.statIncomingRate.Add(1)

			// Lock
			r.m.Lock()
			defer r.m.Unlock()

			// We update the last slot if:
			//   - there are no slots
			//   - the node of the last slot is different from the desired node AND the payload's node is the desired
			//   node. That way, if the desired node doesn't frames for some time, we fallback to the previous node
			//   instead of the previous item
			if r.slots[len(r.slots)-1] == nil || (r.n != r.slots[len(r.slots)-1].n && r.n == p.Node) {
				r.slots[len(r.slots)-1] = r.newRateEnforcerSlot(p)
			}

			// Create item
			i := r.newRateEnforcerItem(p)

			// Copy frame
			if ret := avutil.AvFrameRef(i.f, p.Frame); ret < 0 {
				emitAvError(r, r.e, ret, "avutil.AvFrameRef failed")
				return
			}

			// Append item
			r.buf = append(r.buf, i)
		})
	})
}

func (r *RateEnforcer) newRateEnforcerSlot(p *FrameHandlerPayload) *rateEnforcerSlot {
	return &rateEnforcerSlot{
		n:      r.n,
		ptsMax: p.Frame.Pts() + int64(r.timeBase.ToDouble()/p.Descriptor.TimeBase().ToDouble()),
		ptsMin: p.Frame.Pts(),
	}
}

func (r *RateEnforcer) newRateEnforcerItem(p *FrameHandlerPayload) *rateEnforcerItem {
	return &rateEnforcerItem{
		d: p.Descriptor,
		f: r.p.get(),
		n: p.Node,
	}
}

func (r *RateEnforcer) startTick(ctx context.Context) {
	go func() {
		nextAt := time.Now()
		for {
			func() {
				// Compute next at
				nextAt = nextAt.Add(r.period)

				// Sleep until next at
				if delta := nextAt.Sub(time.Now()); delta > 0 {
					astitime.Sleep(ctx, delta)
				}

				// Check context
				if ctx.Err() != nil {
					return
				}

				// Lock
				r.m.Lock()
				defer r.m.Unlock()

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
					return
				}

				// Distribute
				r.distribute()

				// Dispatch
				i, previous := r.current()
				if i != nil {
					// Restamp frame
					if r.restamper != nil {
						r.restamper.Restamp(i.f, true)
					}

					// Dispatch frame
					r.d.dispatch(i.f, i.d)
				}

				// Remove first slot
				if !previous {
					r.p.put(i.f)
				}
				r.slots = r.slots[1:]
			}()
		}
	}()
}

func (s *rateEnforcerSlot) next() *rateEnforcerSlot {
	return &rateEnforcerSlot{
		n:      s.n,
		ptsMin: s.ptsMax,
		ptsMax: s.ptsMax - s.ptsMin + s.ptsMax,
	}
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
				s.i = r.buf[idx]
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

func (r *RateEnforcer) current() (i *rateEnforcerItem, previous bool) {
	if r.slots[0] != nil && r.slots[0].i != nil {
		// Get item
		i = r.slots[0].i

		// Create previous item
		if r.previousItem == nil {
			// Create item
			r.previousItem = &rateEnforcerItem{
				d: i.d,
				f: r.p.get(),
			}
		} else {
			avutil.AvFrameUnref(r.previousItem.f)
		}

		// Copy frame
		if ret := avutil.AvFrameRef(r.previousItem.f, i.f); ret < 0 {
			emitAvError(r, r.e, ret, "avutil.AvFrameRef failed")
			r.p.put(r.previousItem.f)
			r.previousItem = nil
		}
	} else {
		i = r.previousItem
		previous = true
	}
	return
}

// HandleFrame implements the FrameHandler interface
func (r *RateEnforcer) HandleFrame(p *FrameHandlerPayload) {
	r.q.Send(p)
}
