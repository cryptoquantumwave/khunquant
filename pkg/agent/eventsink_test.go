package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const (
	sentinelPrompt = "SENTINEL_PROMPT_7f3a"
	sentinelResult = "SENTINEL_RESULT_91bc"
	sentinelTool   = "SENTINEL_TOOLARG_44de"
)

// sentinelLoop is a loop whose sub-turns answer with sentinelResult. The
// sub-turn's prompt is sentinelPrompt (see subTurnOnce).
func sentinelLoop(t *testing.T) *AgentLoop {
	t.Helper()
	prov := newRoutedProvider().on(sentinelPrompt, func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
		return textResponse(sentinelResult)
	})
	al, _ := newTurnTestLoop(t, prov, false)
	return al
}

func newParentTurn(id string) *turnState {
	return &turnState{
		ctx:            context.Background(),
		turnID:         id,
		pendingResults: make(chan *tools.ToolResult, 8),
		concurrencySem: make(chan struct{}, maxConcurrentSubTurns),
		session:        &ephemeralSessionStore{},
	}
}

// subTurnOnce runs one sub-turn whose prompt and result both hold secrets.
func subTurnOnce(t *testing.T, al *AgentLoop, async bool) {
	t.Helper()
	parent := newParentTurn("parent-sentinel")
	res, err := spawnSubTurn(context.Background(), al, parent, SubTurnConfig{
		Model:        "char-model",
		SystemPrompt: sentinelPrompt + " " + sentinelTool,
		Async:        async,
	})
	if err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	if res == nil || !strings.Contains(res.ForLLM, sentinelResult) {
		t.Fatalf("sub-turn result = %+v, want the sentinel answer", res)
	}
}

// busWatcher subscribes to every agent.subturn.* event of b.
func busWatcher(t *testing.T, b *events.EventBus) <-chan events.Event {
	t.Helper()
	sub, ch, err := b.Channel().KindPrefix("agent.subturn.").SubscribeChan(
		context.Background(), events.SubscribeOptions{Name: "test", Buffer: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return ch
}

func drain(ch <-chan events.Event, want int) []events.Event {
	var out []events.Event
	deadline := time.After(testWait)
	for len(out) < want {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestSubTurnEmitsToBus(t *testing.T) {
	al := sentinelLoop(t)
	b := events.NewBus()
	t.Cleanup(func() { _ = b.Close() })
	ch := busWatcher(t, b)
	al.SetEventBus(b)

	subTurnOnce(t, al, true)

	// Spawn, then the async result delivered to the parent, then end.
	got := drain(ch, 3)
	kinds := map[events.Kind]events.Event{}
	for _, e := range got {
		kinds[e.Kind] = e
	}
	for _, k := range []events.Kind{
		events.KindAgentSubTurnSpawn, events.KindAgentSubTurnResultDelivered, events.KindAgentSubTurnEnd,
	} {
		if _, ok := kinds[k]; !ok {
			t.Fatalf("no %s event on the bus; got %d event(s): %+v", k, len(got), got)
		}
	}

	spawn := kinds[events.KindAgentSubTurnSpawn]
	if spawn.Source.Component != "agent" || spawn.Scope.TurnID != "parent-sentinel" {
		t.Errorf("spawn event source/scope = %+v / %+v", spawn.Source, spawn.Scope)
	}
	if spawn.TrustLevel() != proactive.TrustSystem {
		t.Errorf("spawn event trust = %v, want system", spawn.TrustLevel())
	}
	end, ok := kinds[events.KindAgentSubTurnEnd].Payload.(subTurnEndPayload)
	if !ok || end.Status != SubTurnOK || !end.HasResult || end.ResultBytes == 0 {
		t.Errorf("end payload = %+v, want a clean end with a result", kinds[events.KindAgentSubTurnEnd].Payload)
	}

	stats := b.Stats()
	if stats.Published != 3 || stats.Delivered != 3 || stats.Dropped != 0 {
		t.Errorf("bus stats = %+v, want 3 published and delivered, none dropped", stats)
	}
}

// With no bus the loop publishes nothing and a sub-turn runs exactly as before.
func TestSubTurnNoBusDoesNothing(t *testing.T) {
	al := sentinelLoop(t)
	if _, ok := al.subTurnSink().(noopSink); !ok {
		t.Fatalf("a loop with no bus has sink %T, want the no-op", al.subTurnSink())
	}

	unused := events.NewBus() // never attached
	t.Cleanup(func() { _ = unused.Close() })
	subTurnOnce(t, al, true)
	if got := unused.Stats().Published; got != 0 {
		t.Fatalf("a bus nobody attached saw %d event(s)", got)
	}

	// Removing a bus, with nil or a typed nil, goes back to the no-op.
	b := events.NewBus()
	t.Cleanup(func() { _ = b.Close() })
	al.SetEventBus(b)
	al.SetEventBus(nil)
	subTurnOnce(t, al, true)
	var typedNil *events.EventBus
	al.SetEventBus(typedNil)
	if _, ok := al.subTurnSink().(noopSink); !ok {
		t.Fatalf("a typed-nil bus left sink %T, want the no-op", al.subTurnSink())
	}
	subTurnOnce(t, al, true)
	if got := b.Stats().Published; got != 0 {
		t.Fatalf("a detached bus saw %d event(s)", got)
	}
}

// Sub-turn events used to be printed to stdout. Nothing may reach it now, with
// a bus or without one.
func TestNoStdoutOnSubTurn(t *testing.T) { // not parallel: it swaps os.Stdout
	al := sentinelLoop(t)
	b := events.NewBus()
	t.Cleanup(func() { _ = b.Close() })
	ch := busWatcher(t, b)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- string(data)
	}()
	restore := sync.OnceFunc(func() { os.Stdout = orig })
	t.Cleanup(restore)

	subTurnOnce(t, al, true) // no bus
	al.SetEventBus(b)
	subTurnOnce(t, al, true) // with a bus
	deliverSubTurnResult(busEventSink{bus: b}, newParentTurn("p"), "child-x", &tools.ToolResult{ForLLM: sentinelResult})

	restore()
	_ = w.Close()
	out := <-captured
	_ = r.Close()

	_ = drain(ch, 3) // the bus did see the second sub-turn
	if b.Stats().Published == 0 {
		t.Fatal("the bus saw nothing; the test would pass for the wrong reason")
	}
	for _, banned := range []string{"Mock EventBus", "SubTurnSpawnEvent", "SubTurnEndEvent",
		"SubTurnResultDeliveredEvent", "SubTurnOrphanResultEvent", sentinelPrompt, sentinelResult, sentinelTool} {
		if strings.Contains(out, banned) {
			t.Errorf("stdout contains %q:\n%s", banned, out)
		}
	}
}

// Nothing a sub-turn reads or writes may be on the bus: not its prompt, not a
// tool argument, not its result, not the text of an error.
func TestSubTurnEventPayloadsCarryNoContent(t *testing.T) {
	al := sentinelLoop(t)
	b := events.NewBus()
	t.Cleanup(func() { _ = b.Close() })
	ch := busWatcher(t, b)
	al.SetEventBus(b)
	subTurnOnce(t, al, true)

	// An orphaned result and a failed sub-turn too.
	sink := busEventSink{bus: b}
	deliverSubTurnResult(sink, func() *turnState {
		p := newParentTurn("finished-parent")
		p.Finish(false)
		return p
	}(), "child-orphan", &tools.ToolResult{ForLLM: sentinelResult, ForUser: sentinelResult})
	sink.emit(SubTurnEndEvent{
		ParentID: "p", ChildID: "c", Status: subTurnStatus(errors.New(sentinelResult+" "+sentinelTool), false),
	})

	got := drain(ch, 5)
	if len(got) < 5 {
		t.Fatalf("got %d event(s), want at least 5", len(got))
	}
	want := map[events.Kind][]string{
		events.KindAgentSubTurnSpawn: {"agent_id?", "async", "child_id", "critical", "model", "parent_id", "timeout_ms", "tool_count"},
		events.KindAgentSubTurnEnd: {
			"child_id", "duration_ms", "has_result", "parent_id", "result_bytes?", "result_is_error?", "status",
		},
		events.KindAgentSubTurnResultDelivered: {"child_id", "parent_id"},
		events.KindAgentSubTurnOrphan:          {"child_id", "parent_id"},
	}
	seen := map[events.Kind]bool{}
	for _, e := range got {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{sentinelPrompt, sentinelResult, sentinelTool} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s event leaks %q: %s", e.Kind, secret, raw)
			}
		}
		if len(e.Attrs) != 0 {
			t.Errorf("%s event has attrs: %v", e.Kind, e.Attrs)
		}

		var decoded struct {
			Payload map[string]json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range decoded.Payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		allowed := map[string]bool{}
		for _, k := range want[e.Kind] {
			allowed[strings.TrimSuffix(k, "?")] = true
		}
		for _, k := range keys {
			if !allowed[k] {
				t.Errorf("%s payload has unexpected key %q (keys %v)", e.Kind, k, keys)
			}
		}
		for _, k := range want[e.Kind] {
			if !strings.HasSuffix(k, "?") && decoded.Payload[k] == nil {
				t.Errorf("%s payload lacks %q (keys %v)", e.Kind, k, keys)
			}
		}
		seen[e.Kind] = true
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("no %s event was checked", k)
		}
	}
}

func TestSubTurnStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err      error
		panicked bool
		want     SubTurnStatus
	}{
		{nil, false, SubTurnOK},
		{errors.New("boom"), false, SubTurnFailed},
		{context.Canceled, false, SubTurnCanceled},
		{context.DeadlineExceeded, false, SubTurnTimedOut},
		{errors.New("subturn panicked: x"), true, SubTurnPanicked},
	} {
		if got := subTurnStatus(tc.err, tc.panicked); got != tc.want {
			t.Errorf("subTurnStatus(%v, %v) = %q, want %q", tc.err, tc.panicked, got, tc.want)
		}
	}
}

// ====================== publishing never hurts the agent ======================

// finishesWithin fails the test if fn does not return within testWait.
func finishesWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatalf("%s did not finish: a subscriber is holding up the publisher", what)
	}
}

// A subscriber that never returns, behind a one-slot queue that asks to block:
// the worst case for a publisher. The non-blocking publish the agent uses must
// drop instead of waiting.
func TestSubTurnPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	al := sentinelLoop(t)
	b := events.NewBus()
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		_ = b.Close()
	})
	_, err := b.Channel().Subscribe(context.Background(), events.SubscribeOptions{
		Name: "stuck", Buffer: 1, Backpressure: events.Block,
	}, func(ctx context.Context, _ events.Event) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	al.SetEventBus(b)

	const n = 200
	finishesWithin(t, "publishing 200 events", func() {
		for i := 0; i < n; i++ {
			al.subTurnSink().emit(SubTurnSpawnEvent{ParentID: "p", ChildID: "c", Model: "m"})
		}
	})
	stats := b.Stats()
	if stats.Published != n {
		t.Errorf("published %d, want %d", stats.Published, n)
	}
	if stats.Dropped == 0 {
		t.Errorf("bus stats = %+v, want drops for a subscriber that never keeps up", stats)
	}

	// And a real sub-turn runs to its end with the subscriber still stuck.
	finishesWithin(t, "a sub-turn", func() { subTurnOnce(t, al, true) })
}

// A subscriber that panics, and a filter that panics: neither takes down or
// delays the sub-turn that published.
func TestSubTurnPublishSurvivesPanickingSubscriber(t *testing.T) {
	al := sentinelLoop(t)
	b := events.NewBus()
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.Channel().Subscribe(context.Background(), events.SubscribeOptions{
		Name: "panics", Buffer: 64,
	}, func(context.Context, events.Event) error {
		panic("subscriber blew up")
	}); err != nil {
		t.Fatal(err)
	}
	al.SetEventBus(b)

	subTurnOnce(t, al, true) // 3 events, each one panics its handler

	deadline := time.Now().Add(testWait)
	for {
		st := b.Stats()
		if len(st.SubscriberStats) == 1 && st.SubscriberStats[0].Panicked >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panicking subscriber's panics were not counted: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A panic in a filter runs on the publisher's own goroutine, so the sink
	// has to contain it.
	if _, err := b.Channel().Filter(func(events.Event) bool { panic("filter blew up") }).Subscribe(
		context.Background(), events.SubscribeOptions{Name: "bad-filter"},
		func(context.Context, events.Event) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	finishesWithin(t, "a sub-turn with a panicking filter on the bus", func() { subTurnOnce(t, al, true) })
}

// Publishing after the bus is closed (the gateway shut down while a sub-turn
// was still running) is a quiet no-op.
func TestSubTurnPublishAfterBusClosed(t *testing.T) {
	al := sentinelLoop(t)
	b := events.NewBus()
	al.SetEventBus(b)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	subTurnOnce(t, al, true)
	if st := b.Stats(); !st.Closed || st.Published != 0 {
		t.Fatalf("closed bus stats = %+v, want closed with nothing published", st)
	}
}

// SetEventBus may be called while sub-turns run (a gateway reload calls it
// again); -race checks the swap.
func TestSetEventBusIsSafeDuringSubTurns(t *testing.T) {
	al := sentinelLoop(t)
	b := events.NewBus()
	t.Cleanup(func() { _ = b.Close() })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				al.SetEventBus(b)
				al.SetEventBus(nil)
			}
		}
	}()
	for i := 0; i < 3; i++ {
		subTurnOnce(t, al, true)
	}
	close(stop)
	wg.Wait()
}
