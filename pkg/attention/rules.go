package attention

import "github.com/cryptoquantumwave/khunquant/pkg/proactive"

// Reasons are the machine-readable Decision.Reason values. They are stored in
// attention_decisions and read by the intervention log and the digest, so they
// are part of the on-disk format: never rename one.
const (
	ReasonOff          = "off"
	ReasonShadow       = "shadow"
	ReasonMuted        = "muted"
	ReasonDedupSame    = "dedup_same_tier"
	ReasonDedupQueued  = "dedup_queued"
	ReasonProtective   = "protective"
	ReasonQuietHours   = "quiet_hours"
	ReasonInfoToDigest = "info_to_digest"
	ReasonBudget       = "budget"
	ReasonComposeLLM   = "compose_llm"
	ReasonTemplate     = "template"
)

// facts is everything the rule table needs to know that is not on the
// candidate itself. Gathering them is the store's job (one transaction), so
// evaluate is a pure function and the table below is the only place the order
// of the rules lives.
type facts struct {
	Mode string // effective attention mode: off | shadow | live

	// Muted is true when an active mute applies to this candidate. The store
	// has already applied the protective exception (below), so evaluate
	// does not repeat it.
	Muted bool
	// DedupHit: the owner was already told about this situation at this tier or
	// higher within the dedup window, or such a message is in flight.
	DedupHit bool
	// QueuedHit: an undelivered digest item for the same situation at this tier
	// or higher already exists within the dedup window.
	QueuedHit bool

	InActiveHours bool
	// SendsToday counts non-critical, non-protective notify/escalate decisions
	// that were delivered (or are in flight) today in the profile's time zone.
	SendsToday int
	Budget     int
}

// evaluate is the rule table. Rules are evaluated in order and the first match
// wins.
//
//	#   condition                                              disposition        reason
//	0   effective mode is off                                  drop (no row)      off
//	1   c.Shadow, or effective mode is shadow                  log                shadow
//	2   an active mute matches c.DedupKey / c.TriggerID        drop               muted
//	3   state[c.DedupKey] severity >= c.Severity, in window    drop               dedup_same_tier
//	3b  undelivered digest item, severity >= c.Severity        drop               dedup_queued
//	4   c.Protective && c.Severity == Critical                 notify | escalate  protective
//	5   now outside Profile.ActiveHours                        digest             quiet_hours
//	6   c.Severity == Info                                     digest             info_to_digest
//	7   Severity < Critical and sends today >= budget          digest             budget
//	    (Warning: budget+1; Critical is never budgeted)
//	8   c.Compose == ComposeLLM                                escalate           compose_llm
//	9   otherwise                                              notify             template
//
// Notes on the table:
//
//   - A mute (rule 2) never suppresses a protective Critical candidate unless
//     its prefix equals the candidate's full DedupKey or full TriggerID. A broad
//     "dn:" snooze must not hide a liquidation warning. The store applies this
//     before setting facts.Muted.
//   - Rule 3 also counts in-flight decisions (notify/escalate not yet marked
//     delivered or failed, decided within the last 10 minutes), so two
//     concurrent decisions for one situation cannot both pass. They report the
//     same reason, dedup_same_tier. A protective Critical ignores in-flight
//     decisions (only delivered state dedups it), so a caller that crashed
//     between Decide and MarkDelivered cannot silence it.
//   - The same reservation applies to rule 7: an in-flight non-critical
//     notify/escalate counts against today's budget until it is delivered
//     (stays counted) or reported failed (released). Only notify/escalate rows
//     count; a delivered digest item does not.
//   - Rule 3b does not apply to a protective Critical candidate: it is about
//     things the owner has NOT been told, and a queued item must never swallow
//     the one alert that must not wait.
//   - Rule 4 returns escalate for a protective Critical with ComposeLLM, so the
//     LLM may add an explanation; the alert itself is not gated on it.
//   - A Critical candidate that is not protective still obeys rules 2, 3, 3b and
//     5 (mute, dedup, quiet hours) but is never budgeted: rule 7 applies only to
//     severities below Critical, and a delivered Critical does not count toward
//     the budget.
//   - "A row for every decision" has one exception besides mode off: shadow
//     coalescing. In shadow (effective mode shadow, or c.Shadow), if a
//     log/shadow row for the same dedup key at severity >= the candidate's was
//     recorded within the last 60 minutes, Decide returns the same log/shadow
//     decision (carrying that row's ID) and inserts nothing; a higher severity
//     inserts at once. This bounds a per-minute shadow trigger to about two rows
//     an hour per key. Shadow rows are pruned after 30 days.
func evaluate(c proactive.Candidate, f facts) (proactive.Disposition, string) {
	switch {
	case f.Mode == modeOff:
		return proactive.DispDrop, ReasonOff
	case c.Shadow || f.Mode == modeShadow:
		return proactive.DispLog, ReasonShadow
	case f.Muted:
		return proactive.DispDrop, ReasonMuted
	case f.DedupHit:
		return proactive.DispDrop, ReasonDedupSame
	case f.QueuedHit && !isProtectiveCritical(c):
		return proactive.DispDrop, ReasonDedupQueued
	case isProtectiveCritical(c):
		if c.Compose == proactive.ComposeLLM {
			return proactive.DispEscalate, ReasonProtective
		}
		return proactive.DispNotify, ReasonProtective
	case !f.InActiveHours:
		return proactive.DispDigest, ReasonQuietHours
	case c.Severity == proactive.SeverityInfo:
		return proactive.DispDigest, ReasonInfoToDigest
	case c.Severity < proactive.SeverityCritical && f.SendsToday >= f.limitFor(c.Severity):
		return proactive.DispDigest, ReasonBudget
	case c.Compose == proactive.ComposeLLM:
		return proactive.DispEscalate, ReasonComposeLLM
	default:
		return proactive.DispNotify, ReasonTemplate
	}
}

// limitFor is how many non-critical sends a day allows a candidate of severity
// s: the budget, plus one for a Warning.
func (f facts) limitFor(s proactive.Severity) int {
	if s == proactive.SeverityWarning {
		return f.Budget + 1
	}
	return f.Budget
}

func isProtectiveCritical(c proactive.Candidate) bool {
	return c.Protective && c.Severity == proactive.SeverityCritical
}
