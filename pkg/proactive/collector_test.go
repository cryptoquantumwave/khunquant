package proactive

import (
	"context"
	"sync"
	"testing"
)

func TestCollector_CapsAndCounts(t *testing.T) {
	c := NewCollector()
	for i := 0; i < MaxDecisionsPerTurn+3; i++ {
		kept := c.Add(Decision{Kind: DecideNotify})
		if kept != (i < MaxDecisionsPerTurn) {
			t.Fatalf("Add #%d kept=%v", i, kept)
		}
	}
	if got := len(c.Decisions()); got != MaxDecisionsPerTurn {
		t.Errorf("kept %d decisions, want %d", got, MaxDecisionsPerTurn)
	}
	if c.Dropped() != 3 {
		t.Errorf("Dropped = %d, want 3", c.Dropped())
	}
}

func TestCollector_DecisionsIsACopy(t *testing.T) {
	c := NewCollector()
	c.Add(Decision{Kind: DecideSilent})
	got := c.Decisions()
	got[0].Kind = DecideNotify
	if c.Decisions()[0].Kind != DecideSilent {
		t.Error("mutating the returned slice changed the collector")
	}
}

func TestCollector_ConcurrentAdd(t *testing.T) {
	c := NewCollector()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Add(Decision{Kind: DecideNotify})
		}()
	}
	wg.Wait()
	if len(c.Decisions()) != MaxDecisionsPerTurn || c.Dropped() != 50-MaxDecisionsPerTurn {
		t.Errorf("kept %d dropped %d", len(c.Decisions()), c.Dropped())
	}
}

func TestCollectorContext(t *testing.T) {
	if CollectorFrom(context.Background()) != nil {
		t.Error("a bare context has a collector")
	}
	c := NewCollector()
	if CollectorFrom(WithCollector(context.Background(), c)) != c {
		t.Error("collector did not round-trip through the context")
	}
}
