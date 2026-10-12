package triggers

import "sync/atomic"

type counters struct {
	evaluated, candidates, errors, panics       atomic.Uint64
	invalidCandidates, invalidState             atomic.Uint64
	autoDisabled, discarded                     atomic.Uint64
	stateSaved, saveFailures                    atomic.Uint64
	stateLoadFailures, stateDiscarded, unusable atomic.Uint64
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
	Discarded         uint64 // evaluations whose changes were dropped because attention had not decided them
	StateSaved        uint64
	SaveFailures      uint64
	StateLoadFailures uint64
	StateDiscarded    uint64 // saved states that were damaged and dropped
	Unusable          uint64 // stored specs this build cannot run (unknown kind, refused parameters)

	Specs int // gauge: specs held, including unusable ones
}

// Stats returns the counters.
func (r *Registry) Stats() Stats {
	c := &r.c
	return Stats{
		Evaluated: c.evaluated.Load(), Candidates: c.candidates.Load(), Errors: c.errors.Load(),
		Panics: c.panics.Load(), InvalidCandidates: c.invalidCandidates.Load(), InvalidState: c.invalidState.Load(),
		AutoDisabled: c.autoDisabled.Load(), Discarded: c.discarded.Load(), StateSaved: c.stateSaved.Load(),
		SaveFailures: c.saveFailures.Load(), StateLoadFailures: c.stateLoadFailures.Load(),
		StateDiscarded: c.stateDiscarded.Load(), Unusable: c.unusable.Load(),
		Specs: len(r.snap.Load().ordered),
	}
}
