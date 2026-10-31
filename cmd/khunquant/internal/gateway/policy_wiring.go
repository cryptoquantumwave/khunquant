package gateway

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// policyRuntime is the policy engine's wiring for one generation of services: the
// table the verdicts are written to and the read-only mandate lookup. In shadow the
// engine records what it would have decided for each tool call and the existing
// checks still decide every call; in live (enforcing) it also refuses, through the
// tool hook, what the existing checks allowed and it refuses.
type policyRuntime struct {
	// mode is what is armed: shadow, or live (the configured mode can be lowered).
	mode  string
	store *policy.Store
	// rec is the asynchronous recorder tool calls hand their records to.
	rec *policy.Recorder
	// mandates is the DCA plan store handle mandates live in, late-bound because it
	// is opened by buildApproval, which runs after perception (whose engine runs
	// the store's housekeeper). Nil: nothing is covered.
	mandates atomic.Pointer[dca.Store]
	// now is the gateway clock; nil is time.Now.
	now func() time.Time
}

func (rt *policyRuntime) clock() time.Time {
	if rt != nil && rt.now != nil {
		return rt.now()
	}
	//simclock:wall the default when no clock is injected
	return time.Now()
}

// policyLive is the runtime the stats route reads: the current generation's, nil
// while none is armed. Package state because the route is registered before the
// runtime exists and outlives none of them.
var policyLive atomic.Pointer[policyRuntime]

// buildPolicy arms the shadow recorder, or returns nil and disarms it. It is armed
// only while proactive is effectively on AND proactive.policy.mode is shadow or
// live: with the mode off, or proactive off, no request is built, the engine is never
// called and no row is written. A database that cannot be opened is logged and leaves
// it off. Live (Effective already lowers it to shadow without a valid owner) also arms
// the enforcing tool hook, unless trading_risk does not validate (a share cap: no
// portfolio source is wired, so enforcing it would refuse every buy), in which case
// it stays shadow and says so. Called on every build and reload, like the kill flag
// it reads; the engine, and with it the order-rate memory, is new on each.
func buildPolicy(d assemblyDeps) *policyRuntime {
	tools.SetPolicyLive(nil)
	tools.SetPolicyShadow(nil)
	policyLive.Store(nil)
	eff := d.Cfg.Proactive.Effective()
	if !eff.Enabled || (eff.PolicyMode != config.ProactiveModeShadow && eff.PolicyMode != config.ProactiveModeLive) {
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("policy", "The policy engine's shadow record is off: the shared proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := policy.NewStore(ctx, db)
	if err != nil {
		logger.WarnCF("policy", "The policy engine's shadow record is off: its table could not be opened",
			map[string]any{"error": err.Error()})
		return nil
	}
	rt := &policyRuntime{store: store, mode: config.ProactiveModeShadow, now: d.nowFn()}

	var killed func() bool
	if d.Control != nil && d.Control.state != nil {
		killed = d.Control.state.Killed
	}
	cfg := d.Cfg
	eng := tools.NewPolicyEngine(func() *config.Config { return cfg }, killed, rt, nil)
	eng.SetClock(d.nowFn())
	// The owner's tighten-only autonomy dial, read from the profile on every decision.
	readDial := autonomyReader(d)
	eng.SetOwnerDial(func(ctx context.Context, t policy.Tier) (string, error) { return readDial(ctx, t.String()) })
	var bus events.Bus
	if b := d.Shared.eventBus(); b != nil {
		bus = b
	}
	rt.rec = policy.NewRecorder(eng, store, bus, 0)
	rt.rec.SetClock(d.nowFn())
	tools.SetPolicyShadow(rt.rec)
	if eff.PolicyMode == config.ProactiveModeLive {
		if err := config.ValidateTradingRisk(d.Cfg.TradingRisk); err != nil {
			logger.ErrorCF("policy", "proactive.policy.mode is live but trading_risk does not validate, so the policy engine only records (shadow)",
				map[string]any{"error": err.Error()})
		} else {
			tools.SetPolicyLive(eng)
			rt.mode = config.ProactiveModeLive
			logger.InfoCF("policy", "proactive.policy.mode is live: the policy engine refuses what the existing checks allow and it forbids", nil)
		}
	}
	policyLive.Store(rt)
	return rt
}

// Covering is the engine's read-only mandate lookup: an active, unexpired mandate
// on the plan an execute_dca_order call names. Safe on nil.
func (rt *policyRuntime) Covering(ctx context.Context, r policy.ActionRequest) (string, bool) {
	if rt == nil || r.PlanID <= 0 {
		return "", false
	}
	st := rt.mandates.Load()
	if st == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	m, ok, err := st.ActiveMandateForPlan(ctx, r.PlanID, rt.clock())
	if err != nil || !ok {
		return "", false
	}
	return m.ID, true
}

// attachMandates hands the lookup the mandate store once the approval side has
// opened it. Safe on nil.
func (rt *policyRuntime) attachMandates(ap *approvalRuntime) {
	if rt != nil && ap != nil && ap.mandateStore != nil {
		rt.mandates.Store(ap.mandateStore)
	}
}

// policyStopTimeout bounds how long stop waits for the recorder to drain.
const policyStopTimeout = 2 * time.Second

// stop disarms the recorder, drains and stops it (bounded), and lets go of the
// mandate store, all before the approval side closes the store and the shared
// database closes. Safe on nil.
func (rt *policyRuntime) stop() {
	tools.SetPolicyLive(nil)
	tools.SetPolicyShadow(nil)
	if rt == nil {
		return
	}
	policyLive.CompareAndSwap(rt, nil)
	rt.rec.Close(policyStopTimeout)
	rt.mandates.Store(nil)
}

// policyStatsResponse is the policy part of GET /api/proactive/stats: counts only.
// It is present only while the engine is armed; the dropped counter is that of the
// current generation, so a config reload starts it again from zero.
type policyStatsResponse struct {
	// Mode is what is armed: shadow (records) or live (enforces).
	Mode string `json:"mode"`
	// DecisionsToday counts the verdicts recorded since local midnight, by mode
	// (act, ask, notify_only, deny).
	DecisionsToday map[string]int `json:"decisions_today"`
	// Dropped is how many records the recorder dropped because its queue was full.
	Dropped int64 `json:"dropped"`
}

// stats snapshots the counters. Safe on a nil runtime (nil).
func (rt *policyRuntime) stats(now time.Time) *policyStatsResponse {
	if rt == nil {
		return nil
	}
	out := &policyStatsResponse{Mode: rt.mode, DecisionsToday: map[string]int{}, Dropped: rt.rec.Dropped()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if counts, err := rt.store.CountsSince(ctx, midnight); err == nil {
		for m, n := range counts {
			out.DecisionsToday[string(m)] = n
		}
	}
	return out
}

// withPolicyStats adds the policy engine's counters to the stats route.
func withPolicyStats() statsPart {
	return func(r *busStatsResponse) {
		rt := policyLive.Load()
		r.Policy = rt.stats(rt.clock())
	}
}
