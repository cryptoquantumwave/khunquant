package quiesce

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// script is a quiescer that answers from a list, then keeps answering the last item.
type script struct {
	name  string
	seq   []bool
	calls atomic.Int32
}

func (s *script) Name() string { return s.name }
func (s *script) Idle() bool {
	i := int(s.calls.Add(1)) - 1
	return s.seq[min(i, len(s.seq)-1)]
}

func TestRegistry_SettleNeedsTwoCleanPolls(t *testing.T) {
	r := NewRegistry()
	q := &script{name: "x", seq: []bool{true, false, true, true}}
	r.Register(q)
	rep, err := r.SettleReport(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// idle (clean 1), busy (reset), idle (clean 1), idle (clean 2): four polls.
	if q.calls.Load() != 4 || rep.Polls != 4 {
		t.Fatalf("polls: quiescer %d, report %d, want 4", q.calls.Load(), rep.Polls)
	}
}

func TestRegistry_SettleTimeoutNamesBusy(t *testing.T) {
	r := NewRegistry()
	r.Register(Func("x", func() bool { return false }))
	r.Register(Func("ok", func() bool { return true }))
	start := time.Now()
	err := r.Settle(context.Background(), 30*time.Millisecond)
	var be *BusyError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want *BusyError", err)
	}
	if len(be.Names) != 1 || be.Names[0] != "x" {
		t.Fatalf("names = %v, want [x]", be.Names)
	}
	if be.Cause != nil || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("cause %v after %v: the cap, not a context, should end it", be.Cause, time.Since(start))
	}
	if got := be.Error(); !strings.HasPrefix(got, "not settled after ") || !strings.HasSuffix(got, ": x") {
		t.Fatalf("message %q", got)
	}
}

func TestRegistry_SettleContextEndKeepsCause(t *testing.T) {
	r := NewRegistry()
	r.Register(Func("x", func() bool { return false }))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := r.Settle(ctx, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline in the chain", err)
	}
}

func TestRegistry_RegisterReplacesByName(t *testing.T) {
	r := NewRegistry()
	r.Register(Func("a", func() bool { return false }))
	r.Register(Func("b", func() bool { return true }))
	r.Register(Func("a", func() bool { return true }))
	got := r.Snapshot()
	if len(got) != 2 || got[0] != (Status{"a", true}) || got[1] != (Status{"b", true}) {
		t.Fatalf("snapshot = %+v, want a and b once each, both idle, sorted", got)
	}
	if err := r.Settle(context.Background(), time.Second); err != nil {
		t.Fatalf("the replaced quiescer should not keep it busy: %v", err)
	}
}

// A timeout after a clean poll names nothing: the names of an earlier busy poll are stale.
func TestRegistry_TimeoutAfterCleanPollCarriesNoStaleNames(t *testing.T) {
	r := NewRegistry()
	// busy, then idle for good: the settle needs two clean polls, but the context ends
	// after the first.
	q := &script{name: "x", seq: []bool{false, true}}
	r.Register(q)
	ctx, cancel := context.WithCancel(context.Background())
	r.Register(Func("cancel-on-clean", func() bool {
		if q.calls.Load() >= 1 { // from the second poll on (it sorts before x)
			cancel()
		}
		return true
	}))
	rep, err := r.SettleReport(ctx, time.Minute)
	var be *BusyError
	if !errors.As(err, &be) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a BusyError wrapping the cancel", err)
	}
	if len(be.Names) != 0 || len(rep.Busy) != 0 {
		t.Fatalf("names %v / report %v after a clean last poll, want none", be.Names, rep.Busy)
	}
	if got := be.Error(); !strings.HasPrefix(got, "not settled after ") || strings.HasSuffix(got, ": ") || !strings.Contains(got, "no component") {
		t.Fatalf("message %q reads badly with no names", got)
	}
}

func TestRegistry_EmptySettles(t *testing.T) {
	if err := NewRegistry().Settle(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

// A component that turns busy right after a clean poll (the wake-up the gap exists for)
// resets the count.
func TestRegistry_BusyAfterCleanPollResetsCount(t *testing.T) {
	r := NewRegistry()
	q := &script{name: "x", seq: []bool{true, false, false, true, true}}
	r.Register(q)
	rep, err := r.SettleReport(context.Background(), time.Second)
	if err != nil || rep.Polls != 5 {
		t.Fatalf("polls = %d err = %v, want 5", rep.Polls, err)
	}
}
