package perception

import (
	"context"
	"sort"
	"strings"
)

// maxFailureText bounds the text of a failure kept in a ReconcileReport.
const maxFailureText = 200

// SourceFailure is a source whose Reconcile returned an error or panicked.
type SourceFailure struct {
	Source string
	// Err is the error text, capped and on one line. It is the source's own
	// message (a fixed description plus account names): it is for a log line and
	// never for a prompt.
	Err string
}

// ReconcileReport says how each source fared in one ReconcileReport call. Every
// list is sorted by source name.
type ReconcileReport struct {
	// OK are the sources whose Reconcile returned without an error.
	OK []string
	// Failed are the sources whose Reconcile returned an error or panicked.
	Failed []SourceFailure
	// Unfinished are the sources still reconciling when the budget ended. They were
	// told to stop (their context is done) and finish on their own; a later call
	// does not start them again while they have not returned.
	Unfinished []string
	// Busy are the sources whose Reconcile from an earlier call has still not
	// returned, so that none was started.
	Busy []string
}

// Complete reports whether every source reconciled without an error.
func (r ReconcileReport) Complete() bool {
	return len(r.Failed) == 0 && len(r.Unfinished) == 0 && len(r.Busy) == 0
}

type reconcileResult struct {
	ms  *managed
	err error
}

// ReconcileReport asks every source to re-read its authoritative state and emit
// it, like ReconcileAll, but bounded by ctx whatever the sources do with it.
//
// ReconcileAll runs the sources one after the other and relies on each to honour
// its context. A source that waits on a lock of its own before it looks at the
// context (the market source's poll, a delta-neutral plan check that takes minutes)
// would hold the caller for as long as it likes. Here each source runs on a
// goroutine of its own; the call returns when all of them have returned or ctx is
// done, and names the sources that had not (Unfinished). Such a goroutine is not
// waited for: it holds the source's context, which is done, and ends when the
// source notices; until it does, the source is Busy and is not started again.
//
// A failure or a panic of one source is reported and does not touch the others.
// Events a source emits go into the same inbox as always; the order between
// sources is not defined.
func (m *Manager) ReconcileReport(ctx context.Context) ReconcileReport {
	m.mu.Lock()
	all := append([]*managed(nil), m.sources...)
	m.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })

	var rep ReconcileReport
	results := make(chan reconcileResult, len(all)) // never blocks a source that outlives the call
	pending := map[*managed]struct{}{}
	for _, ms := range all {
		if !ms.reconciling.CompareAndSwap(false, true) {
			rep.Busy = append(rep.Busy, ms.name)
			continue
		}
		pending[ms] = struct{}{}
		go func(ms *managed) {
			defer ms.reconciling.Store(false)
			results <- reconcileResult{ms: ms, err: m.reconcileOne(ctx, ms)}
		}(ms)
	}

	take := func(r reconcileResult) {
		delete(pending, r.ms)
		if r.err == nil {
			rep.OK = append(rep.OK, r.ms.name)
			return
		}
		m.errCount.Add(1)
		r.ms.noteError(r.err, m.clock.Now())
		rep.Failed = append(rep.Failed, SourceFailure{Source: r.ms.name, Err: oneLine(r.err.Error(), maxFailureText)})
	}
	for len(pending) > 0 {
		select {
		case r := <-results:
			take(r)
			continue
		case <-ctx.Done():
		}
		// The budget is over. Results that arrived together with it still count.
		for drained := false; !drained; {
			select {
			case r := <-results:
				take(r)
			default:
				drained = true
			}
		}
		for ms := range pending {
			rep.Unfinished = append(rep.Unfinished, ms.name)
		}
		break
	}

	sort.Strings(rep.OK)
	sort.Strings(rep.Unfinished)
	sort.Strings(rep.Busy)
	sort.Slice(rep.Failed, func(i, j int) bool { return rep.Failed[i].Source < rep.Failed[j].Source })
	return rep
}

// oneLine reduces s to a single line of at most max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
