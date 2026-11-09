package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// A plan tool's request makes the alert runtime busy at once (before its loop has taken
// it), through the debounce window and the pass, and idle only once the pass has run: a
// lab settle cannot pass between the tool's turn and the hand-over it asked for.
func TestDNSync_AlertRuntimeIsBusyUntilTheDebouncedPassRan(t *testing.T) {
	f, planID, _ := linkedDNPlan(t, alertsMode(config.ProactiveModeLive))
	a := f.services.Alerts
	waitUntil(t, "the alert runtime idle at rest", a.idle)
	before := a.dn.passes.Load()

	runDNTool(t, f, tools.NameUpdateDeltaNeutralPlan, map[string]any{"plan_id": float64(planID), "enabled": true})
	if a.idle() {
		t.Fatal("idle right after a plan tool asked for a pass")
	}
	waitUntil(t, "the alert runtime idle after the pass", a.idle)
	if n := a.dn.passes.Load() - before; n != 1 {
		t.Fatalf("idle after %d debounced passes, want 1: it went idle before the pass ran", n)
	}
}

// The sampler is busy from start until its first sample is taken, and from a tick until
// that sample is taken; between samples (the next one is a timer) it is idle.
func TestSampler_IdleOnlyBetweenSamples(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	collect := func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
		entered <- struct{}{}
		<-release
		return nil, errors.New("exchange down")
	}
	tick := make(chan time.Time)
	s := newTestSampler(newTestTracker(), collect)
	s.tracked = []string{"binance/main"}
	s.after = func(time.Duration) <-chan time.Time { return tick }
	s.start()
	t.Cleanup(s.Stop)
	if s.idle() {
		t.Fatal("idle before the first sample was taken")
	}
	<-entered
	if s.idle() {
		t.Fatal("idle while the first sample runs")
	}
	release <- struct{}{}
	waitUntil(t, "idle after the first sample", s.idle)

	tick <- samplerEpoch // taken by the sampler once the send returns
	<-entered
	if s.idle() {
		t.Fatal("idle while a timed sample runs")
	}
	release <- struct{}{}
	waitUntil(t, "idle after the timed sample", s.idle)
}
