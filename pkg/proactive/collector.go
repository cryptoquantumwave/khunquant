package proactive

import (
	"context"
	"sync"
)

// MaxDecisionsPerTurn is how many decisions one proactive turn may declare.
// A turn that wants more has lost the plot, and the cap keeps a confused or
// manipulated model from turning one wake-up into a flood of messages.
const MaxDecisionsPerTurn = 8

// Collector gathers the decisions a proactive turn declares through
// respond_proactive. One is created per turn and travels in the turn's context;
// the tool calls of one model response run concurrently, so it is safe for
// concurrent use.
type Collector struct {
	mu        sync.Mutex
	decisions []Decision
	dropped   int
}

// NewCollector returns an empty collector.
func NewCollector() *Collector { return &Collector{} }

// Add records d and reports whether it was kept. Once MaxDecisionsPerTurn are
// held, further decisions are dropped and counted.
func (c *Collector) Add(d Decision) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.decisions) >= MaxDecisionsPerTurn {
		c.dropped++
		return false
	}
	c.decisions = append(c.decisions, d)
	return true
}

// Decisions returns a copy of the decisions kept so far, in the order declared.
func (c *Collector) Decisions() []Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Decision(nil), c.decisions...)
}

// Dropped is how many decisions were refused for being over the cap.
func (c *Collector) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

type collectorKey struct{}

// WithCollector returns a context carrying c.
func WithCollector(ctx context.Context, c *Collector) context.Context {
	return context.WithValue(ctx, collectorKey{}, c)
}

// CollectorFrom returns the turn's collector, or nil when the context has none.
func CollectorFrom(ctx context.Context) *Collector {
	c, _ := ctx.Value(collectorKey{}).(*Collector)
	return c
}
