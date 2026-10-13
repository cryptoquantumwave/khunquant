package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const journalEngineFrame = "journal.(*Engine)"

// withJournalSource registers a definition source for one test.
func withJournalSource(t *testing.T, src func(assemblyDeps) []journal.Definition) {
	t.Helper()
	prev := journalSources
	journalSources = append(append([]func(assemblyDeps) []journal.Definition(nil), prev...), src)
	t.Cleanup(func() { journalSources = prev })
}

func workflowTables(t *testing.T, workspace string) int {
	t.Helper()
	db, err := sqlitex.Shared(workspace)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('workflows','workflow_steps')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With no definition registered, a running gateway gains no engine, no goroutine
// and no table, and the stats route has no journal section.
func TestJournal_NothingRegisteredBuildsNothing(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"default config":    nil,
		"perception live":   liveWithPerception(),
		"proactive enabled": withOwner("1001"),
	} {
		t.Run(name, func(t *testing.T) {
			if len(journalSources) != 0 {
				t.Fatalf("%d journal sources registered in production code; this change registers none", len(journalSources))
			}
			f := newAssemblyFixture(t, mutate)
			if f.services.journalLive == nil || f.services.journalLive.Load() != nil {
				t.Fatal("a workflow engine was built with no definition registered")
			}
			if n := goroutinesIn(journalEngineFrame); n != 0 {
				t.Fatalf("%d journal engine goroutines on a gateway with no workflow", n)
			}
			if n := workflowTables(t, f.workspace); n != 0 {
				t.Fatalf("%d journal tables were created on a gateway with no workflow", n)
			}
			body := f.expectStatus(statsPath, testMgmtToken, http.StatusOK)
			if strings.Contains(string(body), "journal") {
				t.Fatalf("the stats route mentions the journal while no engine runs: %s", body)
			}
		})
	}
}

// With a definition registered the engine is built, runs, shows on the stats route,
// publishes payload-free events, survives a reload and stops with the services.
func TestJournal_RegisteredDefinitionRunsAndIsStopped(t *testing.T) {
	const secret = "SECRET-WORKFLOW-OUTPUT-31"
	withJournalSource(t, func(assemblyDeps) []journal.Definition {
		return []journal.Definition{{Kind: "gw.test", Run: func(c *journal.Ctx, _ json.RawMessage) error {
			if err := c.Step("one", nil, func(context.Context, int) (any, error) { return secret, nil }); err != nil {
				return err
			}
			return c.Step("two", nil, func(context.Context, int) (any, error) { return 2, nil })
		}}}
	})
	f := newAssemblyFixture(t, nil)
	rt := f.services.journalLive.Load()
	if rt == nil {
		t.Fatal("no engine although a definition is registered")
	}
	if workflowTables(t, f.workspace) != 2 {
		t.Fatal("the journal tables were not created")
	}

	var mu sync.Mutex
	var got []events.Event
	if _, err := f.shared.Events.Channel().KindPrefix("journal.").Subscribe(context.Background(),
		events.SubscribeOptions{Name: "test", Buffer: 64},
		func(_ context.Context, ev events.Event) error {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := rt.engine.Start(ctx, "gw.test", map[string]string{"secret": secret})
	if err != nil {
		t.Fatal(err)
	}
	w, err := rt.engine.Wait(ctx, id)
	if err != nil || w.Status != journal.StatusSucceeded {
		t.Fatalf("workflow = %+v, %v", w, err)
	}

	var stats busStatsResponse
	body := f.expectStatus(statsPath, testMgmtToken, http.StatusOK)
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Journal == nil || stats.Journal.Started != 1 || stats.Journal.Succeeded != 1 || stats.Journal.StepsRun != 2 {
		t.Fatalf("journal stats = %+v (%s)", stats.Journal, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("the stats route leaks workflow data: %s", body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 6 || time.Now().After(deadline) { // started, 2x(step started, step succeeded), succeeded
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 6 {
		t.Fatalf("%d journal events on the bus, want 6", len(got))
	}
	kinds := map[events.Kind]int{}
	for _, ev := range got {
		kinds[ev.Kind]++
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), secret) {
			t.Errorf("event %s carries workflow data", raw)
		}
		if ev.Source.Component != "journal" {
			t.Errorf("event source = %+v", ev.Source)
		}
	}
	if kinds[events.KindJournalStep] != 4 || kinds[events.KindJournalWorkflow] != 2 {
		t.Errorf("event kinds = %v, want 4 journal.step and 2 journal.workflow", kinds)
	}

	// A reload stops this generation's engine (bounded) and builds the next one.
	if err := f.reload(nil); err != nil {
		t.Fatalf("reload: %v", err)
	}
	next := f.services.journalLive.Load()
	if next == nil || next == rt {
		t.Fatal("a reload did not build a fresh engine")
	}
	select {
	case <-rt.done:
	default:
		t.Fatal("the old generation's engine was still running after the reload")
	}
	if n := goroutinesIn("journal.(*Engine).Run("); n != 1 {
		t.Fatalf("%d engine Run goroutines after a reload, want 1", n)
	}

	svc := f.services
	stopServices(svc, 5*time.Second)
	f.services = nil // the fixture's cleanup must not stop them a second time
	if svc.journalLive.Load() != nil {
		t.Error("stopServices left an engine in the holder")
	}
	limit := time.Now().Add(5 * time.Second)
	for goroutinesIn("journal.(*Engine).Run(") != 0 && time.Now().Before(limit) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := goroutinesIn("journal.(*Engine).Run("); n != 0 {
		t.Errorf("%d engine goroutines after stopServices", n)
	}
}

// The adapter maps journal events onto the bus kinds the events package declares.
func TestJournalPublisher_KindsMatchTheEventsPackage(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	_, ch, err := bus.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Name: "t", Buffer: 8})
	if err != nil {
		t.Fatal(err)
	}
	p := journalPublisher{bus: bus}
	p.Publish(journal.Event{Scope: journal.ScopeStep, WorkflowID: "w", WorkflowKind: "k", Step: "s", Seq: 1, Attempt: 2, Status: journal.StepFailed, Duration: 1500 * time.Millisecond})
	p.Publish(journal.Event{Scope: journal.ScopeWorkflow, WorkflowID: "w", WorkflowKind: "k", Status: string(journal.StatusSucceeded)})
	step, wf := <-ch, <-ch
	if step.Kind != events.KindJournalStep || step.Severity != events.SeverityWarn || wf.Kind != events.KindJournalWorkflow {
		t.Errorf("kinds/severity = %s %s / %s", step.Kind, step.Severity, wf.Kind)
	}
	pl, ok := step.Payload.(journalEventPayload)
	if !ok || pl.Step != "s" || pl.Attempt != 2 || pl.DurationMS != 1500 || pl.WorkflowID != "w" {
		t.Errorf("payload = %#v", step.Payload)
	}
}
