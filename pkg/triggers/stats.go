package triggers

import "sync/atomic"

type counters struct {
	evaluated, candidates, errors, panics       atomic.Uint64
	invalidCandidates, invalidState             atomic.Uint64
	autoDisabled, discarded, turnedOff          atomic.Uint64
	stateSaved, saveFailures                    atomic.Uint64
	stateLoadFailures, stateDiscarded, unusable atomic.Uint64
	ownerMarkCleared                            atomic.Uint64
}

// Stats are the registry's counters and gauges: counts only, no ID, no text.
type Stats struct {
	Evaluated         uint64 // trigger evaluations run
	Candidates        uint64 // candidates passed on
	Errors            uint64 // evaluations that failed (an error, a panic or a malformed result)
	Panics            uint64 // of which panics
	InvalidCandidates uint64 // evaluations refused for a malformed or too many candidates
	InvalidState      uint64 // evaluations refused for a state over its limit or not JSON
	AutoDisabled      uint64 // specs switched off after repeated errors
	TurnedOff         uint64 // one-shot specs switched off after their last candidate was decided
	Discarded         uint64 // evaluations whose changes were dropped because attention had not decided them
	StateSaved        uint64
	SaveFailures      uint64
	StateLoadFailures uint64
	StateDiscarded    uint64 // saved states that were damaged and dropped
	Unusable          uint64 // stored specs this build cannot run (unknown kind, refused parameters)
	OwnerMarkCleared  uint64 // candidates that came back marked OwnerRequested from a spec that is not an owner's alert; the mark was removed

	Specs int // gauge: specs held, including unusable ones
}

// Stats returns the counters.
func (r *Registry) Stats() Stats {
	c := &r.c
	return Stats{
		Evaluated: c.evaluated.Load(), Candidates: c.candidates.Load(), Errors: c.errors.Load(),
		Panics: c.panics.Load(), InvalidCandidates: c.invalidCandidates.Load(), InvalidState: c.invalidState.Load(),
		AutoDisabled: c.autoDisabled.Load(), TurnedOff: c.turnedOff.Load(), Discarded: c.discarded.Load(), StateSaved: c.stateSaved.Load(),
		SaveFailures: c.saveFailures.Load(), StateLoadFailures: c.stateLoadFailures.Load(),
		StateDiscarded: c.stateDiscarded.Load(), Unusable: c.unusable.Load(), OwnerMarkCleared: c.ownerMarkCleared.Load(),
		Specs: len(r.snap.Load().ordered),
	}
}

// noteShadow counts a candidate that was marked Shadow, by the kind of its spec.
func (r *Registry) noteShadow(kind string) {
	r.shadowMu.Lock()
	if r.shadowByKind == nil {
		r.shadowByKind = map[string]uint64{}
	}
	r.shadowByKind[kind]++
	r.shadowMu.Unlock()
}

// ShadowCandidates is how many candidates of the given spec kinds have been marked
// Shadow (logged by attention, never delivered) since the registry was built. A
// migration compares it with what the cron path really delivered.
func (r *Registry) ShadowCandidates(kinds ...string) uint64 {
	r.shadowMu.Lock()
	defer r.shadowMu.Unlock()
	var n uint64
	for _, k := range kinds {
		n += r.shadowByKind[k]
	}
	return n
}
