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

// policyRuntime is the policy engine's shadow wiring for one generation of
// services: the table the verdicts are written to and the read-only mandate
// lookup. The engine records what it would have decided for each tool call and
// enforces nothing; the existing checks still decide every call.
type policyRuntime struct {
	store *policy.Store
	// rec is the asynchronous recorder tool calls hand their records to.
	rec *policy.Recorder
	// mandates is the DCA plan store handle mandates live in, late-bound because it
	// is opened by buildApproval, which runs after perception (whose engine runs
	// the store's housekeeper). Nil: nothing is covered.
	mandates atomic.Pointer[dca.Store]
}

// buildPolicy arms the shadow recorder, or returns nil and disarms it. It is armed
// only while proactive is effectively on AND proactive.policy.mode is shadow or
// live (live behaves as shadow until the engine's tool hooks ship): with the mode
// off, or proactive off, no request is built, the engine is never called and no
// row is written. A database that cannot be opened is logged and leaves it off.
// Called on every build and reload, like the kill flag it reads.
func buildPolicy(d assemblyDeps) *policyRuntime {
	tools.SetPolicyShadow(nil)
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
	rt := &policyRuntime{store: store}

	var killed func() bool
	if d.Control != nil && d.Control.state != nil {
		killed = d.Control.state.Killed
	}
	cfg := d.Cfg
	eng := tools.NewPolicyEngine(func() *config.Config { return cfg }, killed, rt, nil)
	var bus events.Bus
	if b := d.Shared.eventBus(); b != nil {
		bus = b
	}
	rt.rec = policy.NewRecorder(eng, store, bus, 0)
	tools.SetPolicyShadow(rt.rec)
	if eff.PolicyMode == config.ProactiveModeLive {
		logger.InfoCF("policy", "proactive.policy.mode is live, which still only records: the engine enforces nothing in this release", nil)
	}
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
	m, ok, err := st.ActiveMandateForPlan(ctx, r.PlanID, time.Now())
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
	tools.SetPolicyShadow(nil)
	if rt == nil {
		return
	}
	rt.rec.Close(policyStopTimeout)
	rt.mandates.Store(nil)
}
