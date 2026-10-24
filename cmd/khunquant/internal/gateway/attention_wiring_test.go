package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/notify/notifytest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func withAttentionMode(mode string) func(*config.Config) {
	return func(cfg *config.Config) { cfg.Proactive.Attention.Mode = mode }
}

func compose(opts ...func(*config.Config)) func(*config.Config) {
	return func(cfg *config.Config) {
		for _, o := range opts {
			o(cfg)
		}
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func submitNotice(t *testing.T, ob *attention.Outbox, key string) attention.Decision {
	t.Helper()
	d, err := ob.Submit(context.Background(), attention.Item{Candidate: proactive.Candidate{
		TriggerID: "trig:" + key, DedupKey: key, Severity: proactive.SeverityNotice, Subject: "BTC/USDT", Title: "t " + key,
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return d
}

func TestAttention_OffBuildsNothing(t *testing.T) {
	f := newAssemblyFixture(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeOff)))
	if f.services.Attention != nil || f.services.attentionRun != nil {
		t.Fatal("attention built although its mode is off")
	}
}

func enabled(cfg *config.Config) { cfg.Proactive.Enabled = true }

func TestAttention_NothingBuiltWhenProactiveDisabled(t *testing.T) {
	// the shipped default (proactive disabled, attention shadow) and a live
	// request without proactive enabled: no governor, no migrations, no outbox.
	for name, mut := range map[string]func(*config.Config){
		"default":      nil,
		"live request": withAttentionMode(config.ProactiveModeLive),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mut)
			if f.services.Attention != nil || f.services.attentionRun != nil {
				t.Fatal("attention built while proactive is disabled")
			}
		})
	}
}

func TestAttention_ShadowIsBuiltAndNeverDelivers(t *testing.T) {
	for name, mut := range map[string]func(*config.Config){
		"enabled, no owner": compose(enabled, withAttentionMode(config.ProactiveModeShadow)),
		"owner configured":  compose(withOwner("1001"), withAttentionMode(config.ProactiveModeShadow)),
		"live downgraded":   compose(enabled, withAttentionMode(config.ProactiveModeLive)), // no owner: effective mode is shadow
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mut)
			ob := f.services.Attention
			if ob == nil {
				t.Fatal("outbox not built")
			}
			if f.services.attentionRun != nil {
				t.Fatal("a Run loop was started although nothing can be delivered")
			}
			d := submitNotice(t, ob, "k")
			if d.Disposition != proactive.DispLog || d.Reason != attention.ReasonShadow {
				t.Fatalf("decision = %s/%s, want log/shadow", d.Disposition, d.Reason)
			}
			if err := ob.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Live attention is only returned together with a Run loop that can deliver:
// without a notify service, an outbox would buffer decisions nothing ever
// flushes. (A service that exists but cannot deliver cannot happen with a live
// effective mode: both derive from the same config.)
func TestAttention_LiveWithoutDeliverableOwnerIsUnavailable(t *testing.T) {
	f := newAssemblyFixture(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive)))
	if f.services.Attention == nil || f.services.attentionRun == nil {
		t.Fatal("precondition: live attention with an owner must be running")
	}
	d := assemblyDeps{Cfg: f.initial, Shared: f.shared}
	if ob, run := buildAttention(d, nil); ob != nil || run != nil {
		t.Fatalf("buildAttention without a notify service = %v, %v", ob, run)
	}
}

func TestAttention_LiveRunsAndReloadDoesNotLeak(t *testing.T) {
	f := newAssemblyFixture(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive)))
	first, run1 := f.services.Attention, f.services.attentionRun
	if first == nil || run1 == nil {
		t.Fatalf("live attention not running: outbox %v runner %v", first, run1)
	}
	if isClosed(run1.done) {
		t.Fatal("the Run loop is not running")
	}
	// live mode decides for real: a Notice in active hours is notify or
	// digest (quiet hours), never log.
	if d := submitNotice(t, first, "live"); d.Disposition == proactive.DispLog {
		t.Fatalf("live outbox decided %s/%s", d.Disposition, d.Reason)
	}

	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	second, run2 := f.services.Attention, f.services.attentionRun
	if second == nil || second == first || run2 == nil || run2 == run1 {
		t.Fatal("outbox not rebuilt on reload")
	}
	if !isClosed(run1.done) {
		t.Fatal("the old Run goroutine outlived the reload")
	}
	if isClosed(run2.done) {
		t.Fatal("the new Run loop is not running")
	}

	// the shared database was not reopened for it.
	if n := countShared(f.shared, proactiveDBResource); n != 1 {
		t.Fatalf("shared database registered %d times", n)
	}

	// a reload that turns attention off tears it down.
	if err := f.reload(withAttentionMode(config.ProactiveModeOff)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.services.Attention != nil || f.services.attentionRun != nil || !isClosed(run2.done) {
		t.Fatal("attention not stopped when switched off")
	}

	// and back on, then stopped by stopServices.
	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	run3 := f.services.attentionRun
	if run3 == nil {
		t.Fatal("attention not rebuilt")
	}
	stopServices(f.services, 5*time.Second)
	if f.services.Attention != nil || f.services.attentionRun != nil || !isClosed(run3.done) {
		t.Fatal("stopServices left attention running")
	}
	f.services = nil
}

func TestAttentionDeliverer(t *testing.T) {
	ctx := context.Background()
	msg := attention.Message{
		InterventionID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Severity: proactive.SeverityInfo, Title: "Daily digest — 2 items",
		Body: "x", DedupKey: "digest:2026-03-11", Silent: true, Digest: true,
		Decisions: []attention.Decision{{Candidate: proactive.Candidate{DedupKey: "a"}}},
	}

	fake := &notifytest.Service{Enabled: true}
	ok, err := attentionDeliverer{svc: fake}.Deliver(ctx, msg)
	if !ok || err != nil {
		t.Fatalf("Deliver = %v, %v", ok, err)
	}
	got := fake.Delivered()
	if len(got) != 1 {
		t.Fatalf("%d notifications", len(got))
	}
	n := got[0]
	if n.Severity != proactive.SeverityInfo || !n.DisableNotification || n.Title != msg.Title || n.Body != "x" ||
		n.InterventionID != msg.InterventionID || n.DedupKey != msg.DedupKey || len(n.Actions) != 0 ||
		n.Candidate == nil || n.Candidate.DedupKey != "a" {
		t.Fatalf("notification = %+v", n)
	}

	// not delivered and failed are both failures.
	if ok, err := (attentionDeliverer{svc: &notifytest.Service{}}).Deliver(ctx, msg); ok || err == nil {
		t.Fatalf("disabled service: %v, %v", ok, err)
	}
	boom := errors.New("boom")
	if ok, err := (attentionDeliverer{svc: &notifytest.Service{Err: boom}}).Deliver(ctx, msg); ok || !errors.Is(err, boom) {
		t.Fatalf("erroring service: %v, %v", ok, err)
	}
	// a nil notify service is a disabled one.
	var none *notify.Service
	if ok, err := (attentionDeliverer{svc: none}).Deliver(ctx, msg); ok || err == nil {
		t.Fatalf("nil service: %v, %v", ok, err)
	}
	// a normal message is not silent.
	fake2 := &notifytest.Service{Enabled: true}
	_, _ = attentionDeliverer{svc: fake2}.Deliver(ctx, attention.Message{Title: "t"})
	if d := fake2.Delivered(); len(d) != 1 || d[0].DisableNotification || d[0].Candidate != nil {
		t.Fatalf("plain notification = %+v", d)
	}
}

func TestAttentionRunner_StopIsNilSafe(t *testing.T) {
	var r *attentionRunner
	r.stop()
}
