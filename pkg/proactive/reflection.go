package proactive

import (
	"context"
	"encoding/json"
	"sync"
)

// MaxRawReflectionItems bounds what a reflection turn may hand over; pkg/reflection
// keeps at most five valid ones.
const MaxRawReflectionItems = 20

// ReflectionItem is one change a reflection turn proposes, exactly as the model wrote
// it. Nothing in it is trusted: pkg/reflection validates every field in code.
type ReflectionItem struct {
	Type        string          `json:"type"`
	Target      string          `json:"target"`
	Change      json.RawMessage `json:"change"`
	Rationale   string          `json:"rationale"`
	EvidenceIDs []string        `json:"evidence_ids"`
}

// ReflectionCollector gathers the items a reflection turn declares through
// propose_reflection. The tool may be called once per turn (a prompt contract, not a
// provider guarantee); a second call is refused.
type ReflectionCollector struct {
	mu      sync.Mutex
	items   []ReflectionItem
	called  bool
	dropped int
}

// NewReflectionCollector returns an empty collector.
func NewReflectionCollector() *ReflectionCollector { return &ReflectionCollector{} }

// Record keeps items (at most MaxRawReflectionItems) and reports false when the tool
// was already called this turn.
func (c *ReflectionCollector) Record(items []ReflectionItem) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.called {
		return false
	}
	c.called = true
	if len(items) > MaxRawReflectionItems {
		c.dropped = len(items) - MaxRawReflectionItems
		items = items[:MaxRawReflectionItems]
	}
	c.items = append([]ReflectionItem(nil), items...)
	return true
}

// Result returns the items, whether the tool was called, and how many were cut.
func (c *ReflectionCollector) Result() (items []ReflectionItem, called bool, dropped int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ReflectionItem(nil), c.items...), c.called, c.dropped
}

type reflectionCollectorKey struct{}

// WithReflectionCollector returns a context carrying c.
func WithReflectionCollector(ctx context.Context, c *ReflectionCollector) context.Context {
	return context.WithValue(ctx, reflectionCollectorKey{}, c)
}

// ReflectionCollectorFrom returns the turn's collector, or nil.
func ReflectionCollectorFrom(ctx context.Context) *ReflectionCollector {
	c, _ := ctx.Value(reflectionCollectorKey{}).(*ReflectionCollector)
	return c
}
