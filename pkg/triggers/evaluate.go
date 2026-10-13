package triggers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// pending is what one trigger's evaluation of the current event would change, held
// back until attention has recorded its decision for every candidate the evaluation
// returned.
type pending struct {
	e   *entry
	ids []string // the candidates that must be decided first

	state      json.RawMessage
	stateDirty bool
	// disable: the evaluation was the tenth error in a row and the candidate in ids is
	// the notice. Only once the notice is decided is the spec switched off.
	disable bool
	// turnOff: the trigger asked to be switched off (State.TurnOff) and its effective
	// mode was live. It takes effect when the candidates are decided, like disable,
	// but is no failure: the error count is left alone.
	turnOff bool
}

// Evaluate routes d to the triggers that list the kind of its event, in spec ID
// order, and returns their candidates. It is the engine's Evaluator, once the
// gateway's adapter has unboxed the engine's Diff and View.
//
// The effective mode of each spec is min(global mode, spec mode), the global mode
// being read now: off is not evaluated, shadow marks its candidates Shadow (attention
// logs them and delivers nothing), live leaves them deliverable. The error it
// returns is always nil: a failing trigger costs its own result, never the event's.
//
// Nothing a trigger changed is final yet. The state it left, its error count and a
// switch-off are held back until Committed names the candidates attention has
// decided (a crash or a failed decision in between loses nothing: the trigger
// re-evaluates from its old state, and attention's dedup absorbs the repeat). A call
// to Evaluate discards whatever the previous event left uncommitted.
//
// Evaluate runs on the pipeline goroutine. It does the one thing it may wait for: a
// local read of a trigger's state the first time the trigger is evaluated.
func (r *Registry) Evaluate(ctx context.Context, v worldmodel.View, d worldmodel.Diff) ([]proactive.Candidate, error) {
	r.discardPending()
	// Route first: an event no trigger lists costs a map lookup, not a read of the
	// configuration.
	es := r.snap.Load().byKind[d.Event.Kind]
	if len(es) == 0 || v == nil {
		return nil, nil
	}
	g := globalRank(r.globalMode())
	if g == 0 {
		return nil, nil
	}
	var out []proactive.Candidate
	for _, e := range es {
		if ctx.Err() != nil {
			break
		}
		out = append(out, r.evaluateOne(ctx, e, g, v, d)...)
	}
	return out, nil
}

// evaluateOne runs one trigger and turns its outcome into candidates and a pending
// change.
func (r *Registry) evaluateOne(ctx context.Context, e *entry, g int, v worldmodel.View, d worldmodel.Diff) []proactive.Candidate {
	e.mu.Lock()
	spec := e.spec
	eff := effectiveRank(g, spec.Mode)
	loaded := e.stateLoaded
	e.mu.Unlock()
	if eff == 0 {
		return nil
	}
	if !loaded && !r.loadState(ctx, e) {
		return nil
	}

	e.mu.Lock()
	st := &State{Raw: bytes.Clone(e.state)}
	oldState := e.state
	e.mu.Unlock()
	r.c.evaluated.Add(1)

	cands, err := r.call(ctx, e, v, d, st)
	if err == nil {
		err = r.checkAll(spec.Kind, cands, st, oldState)
	}
	if err != nil {
		return r.failed(e, spec, eff, v, err)
	}

	p := &pending{e: e}
	if st.Dirty && !bytes.Equal(st.Raw, oldState) {
		p.state, p.stateDirty = st.Raw, true
	}
	p.turnOff = st.TurnOff && eff == 2
	out := make([]proactive.Candidate, 0, len(cands))
	ownerAlert := OwnerAlertSpec(spec.Kind, spec.Origin)
	for _, c := range cands {
		if c.OwnerRequested && !ownerAlert {
			// Only an alert the owner set may break quiet hours. The candidate is kept
			// (a refused one would fail the evaluation and count against the spec) but
			// loses the mark, so the worst a wrong mark costs is a digest entry.
			c.OwnerRequested = false
			r.c.ownerMarkCleared.Add(1)
			r.warn("mark:"+spec.ID, "A trigger returned a candidate marked as the owner's own alert, but its spec is not one; the mark was removed",
				map[string]any{"trigger_id": e.tid, "kind": safeLabel(spec.Kind)})
		}
		c.ID = r.newID()
		c.TriggerID = e.tid
		c.IntentID = spec.IntentID
		c.Trust = proactive.MinTrust(c.Trust, d.Event.TrustLevel())
		c.Shadow = c.Shadow || eff != 2
		if c.Shadow {
			r.noteShadow(spec.Kind)
		}
		if c.At.IsZero() {
			c.At = v.Now()
		}
		p.ids = append(p.ids, c.ID)
		out = append(out, c)
	}
	r.c.candidates.Add(uint64(len(out)))

	e.mu.Lock()
	hadErrors := e.errs > 0
	e.errs = 0 // a success ends the streak, whatever happens to its candidates
	needWrite := p.stateDirty || e.unsaved || e.persistedEr != 0
	e.mu.Unlock()
	if len(p.ids) > 0 || needWrite || hadErrors || p.turnOff {
		r.addPending(p)
	}
	return out
}

// loadState reads a trigger's saved state for the first time. A failure skips the
// trigger for this event and does not count against it: the table not being readable
// is not the trigger's fault.
func (r *Registry) loadState(ctx context.Context, e *entry) bool {
	raw, err := readState(ctx, r.db, e.spec.ID)
	if err != nil {
		r.c.stateLoadFailures.Add(1)
		r.warn("load:"+e.spec.ID, "A trigger's state could not be read; it is skipped for this event",
			map[string]any{"trigger_id": e.tid, "error": safeLabel(err.Error())})
		return false
	}
	if len(raw) > MaxStateBytes || !json.Valid(raw) && len(raw) > 0 {
		raw = nil // damaged: start from nothing rather than feed a trigger garbage
		r.c.stateDiscarded.Add(1)
	}
	e.mu.Lock()
	if !e.stateLoaded {
		e.state, e.stateLoaded = raw, true
	}
	e.mu.Unlock()
	return true
}

// call runs the trigger, turning a panic into an error.
func (r *Registry) call(ctx context.Context, e *entry, v worldmodel.View, d worldmodel.Diff, st *State) (cands []proactive.Candidate, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			r.c.panics.Add(1)
			cands, err = nil, fmt.Errorf("panic: %v", rec)
		}
	}()
	return e.trig.Evaluate(ctx, v, d, st)
}

// checkAll refuses a result the registry cannot pass on: a malformed candidate, too
// many of them, or a state that is too big or not JSON.
func (r *Registry) checkAll(kind string, cands []proactive.Candidate, st *State, old json.RawMessage) error {
	if len(cands) > MaxCandidatesPerEvaluation {
		r.c.invalidCandidates.Add(1)
		return fmt.Errorf("%d candidates, the limit is %d", len(cands), MaxCandidatesPerEvaluation)
	}
	for i, c := range cands {
		if err := checkCandidate(kind, c); err != nil {
			r.c.invalidCandidates.Add(1)
			return fmt.Errorf("candidate %d: %w", i, err)
		}
	}
	if st.Dirty && !bytes.Equal(st.Raw, old) {
		if len(st.Raw) > MaxStateBytes {
			r.c.invalidState.Add(1)
			return fmt.Errorf("state is %d bytes, the limit is %d", len(st.Raw), MaxStateBytes)
		}
		if len(st.Raw) > 0 && !json.Valid(st.Raw) {
			r.c.invalidState.Add(1)
			return fmt.Errorf("state is not valid JSON")
		}
	}
	return nil
}

// failed counts a trigger's error. At MaxConsecutiveErrors it returns the one
// notice and holds the switch-off back until the notice is decided.
func (r *Registry) failed(e *entry, spec Spec, eff int, v worldmodel.View, err error) []proactive.Candidate {
	r.c.errors.Add(1)
	e.mu.Lock()
	e.errs++
	n := e.errs
	e.mu.Unlock()
	r.warn("err:"+spec.ID, "A trigger failed", map[string]any{
		"trigger_id": e.tid, "consecutive_errors": n, "error": safeLabel(err.Error()),
	})

	p := &pending{e: e}
	var out []proactive.Candidate
	if n >= MaxConsecutiveErrors {
		c := r.disabledNotice(e, spec, eff, v)
		out = append(out, c)
		p.ids, p.disable = []string{c.ID}, true
	}
	r.addPending(p)
	return out
}

// disabledNotice is the one message the owner gets when a trigger is switched off.
// Its text is a template naming the kind and the ID; it never carries the error,
// which can hold text from outside. It is capped by the mode like any candidate.
func (r *Registry) disabledNotice(e *entry, spec Spec, eff int, v worldmodel.View) proactive.Candidate {
	return proactive.Candidate{
		ID:        r.newID(),
		TriggerID: e.tid,
		DedupKey:  "triggers:" + spec.ID + ":auto_disabled",
		Severity:  proactive.SeverityNotice,
		Salience:  0.5,
		Subject:   e.tid,
		Title: fmt.Sprintf("Trigger %s (%s) was switched off after %d errors in a row",
			spec.ID, spec.Kind, MaxConsecutiveErrors),
		Evidence: map[string]any{
			"trigger_kind": spec.Kind, "trigger_id": spec.ID, "consecutive_errors": MaxConsecutiveErrors,
		},
		IntentID: spec.IntentID,
		Trust:    proactive.TrustSystem,
		Compose:  proactive.ComposeTemplate,
		At:       v.Now(),
		Shadow:   eff != 2,
	}
}

func (r *Registry) addPending(p *pending) {
	r.pendMu.Lock()
	r.pend = append(r.pend, p)
	r.pendMu.Unlock()
}

func (r *Registry) takePending() []*pending {
	r.pendMu.Lock()
	defer r.pendMu.Unlock()
	p := r.pend
	r.pend = nil
	return p
}

// discardPending drops what the previous event left uncommitted.
func (r *Registry) discardPending() {
	for _, p := range r.takePending() {
		r.c.discarded.Add(1)
		r.rollback(p)
	}
}

// rollback undoes the in-memory part of a pending change that will not be applied.
func (r *Registry) rollback(p *pending) {
	if p.disable {
		p.e.mu.Lock()
		p.e.errs = MaxConsecutiveErrors - 1 // the next error asks for the notice again
		p.e.mu.Unlock()
	}
}

// Committed tells the registry which candidates attention has now decided (their
// decisions are recorded). It applies, for every trigger evaluated for the last
// event whose candidates are all among ids, what that evaluation changed: the new
// state is adopted and saved (only if it differs from the saved one), the error count
// is saved, and a switch-off takes effect. A trigger one of whose candidates is
// missing from ids keeps its old state and is re-evaluated from it.
//
// The save is an UPDATE of the trigger's own row, after the decision, and never part
// of attention's transaction: the two writes are ordered and keyed by candidate ID.
// A failed save is counted and logged; the state stays adopted in memory (so the
// alert is not repeated by this process) and is retried with the next change.
//
// Committed runs on the pipeline goroutine, right after the decisions, with a local
// database write bounded by a timeout.
func (r *Registry) Committed(ctx context.Context, ids []string) {
	pend := r.takePending()
	if len(pend) == 0 {
		return
	}
	for _, p := range pend {
		if !allIn(p.ids, ids) {
			r.c.discarded.Add(1)
			r.rollback(p)
			continue
		}
		r.apply(ctx, p)
	}
}

func allIn(want, have []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// apply makes one pending change final.
func (r *Registry) apply(ctx context.Context, p *pending) {
	e := p.e
	e.mu.Lock()
	if p.stateDirty {
		e.state = p.state
		e.unsaved = true
	}
	var u rowUpdate
	if e.unsaved {
		s := string(e.state)
		u.state = &s
	}
	errs := e.errs
	if p.disable {
		errs = MaxConsecutiveErrors
		e.errs = errs
	}
	if errs != e.persistedEr {
		u.errors = &errs
	}
	guard := e.updatedMs
	if p.disable || p.turnOff {
		u.disable = true
		u.newStamp = max(r.stamp().UnixMilli(), guard+1)
		e.spec.Mode = ModeOff // takes effect now, in memory; the table follows
	}
	e.mu.Unlock()

	if u.state == nil && u.errors == nil && !u.disable {
		return
	}
	wrote, err := updateRow(ctx, r.db, e.spec.ID, guard, u)
	if err != nil {
		r.c.saveFailures.Add(1)
		r.warn("save:"+e.spec.ID, "A trigger's state could not be saved; it will be retried",
			map[string]any{"trigger_id": e.tid, "error": safeLabel(err.Error())})
		return
	}
	if !wrote {
		return // the spec was changed or removed meanwhile: this write no longer applies
	}
	e.mu.Lock()
	if u.state != nil {
		e.unsaved = false
		r.c.stateSaved.Add(1)
	}
	if u.errors != nil {
		e.persistedEr = *u.errors
	}
	if u.disable {
		e.updatedMs = u.newStamp
		e.spec.UpdatedAt = time.UnixMilli(u.newStamp)
		if p.disable {
			r.c.autoDisabled.Add(1)
		} else {
			r.c.turnedOff.Add(1)
		}
	}
	e.mu.Unlock()
}
