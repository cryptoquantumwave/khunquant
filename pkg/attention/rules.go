package attention

import (
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

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
	// ReasonLowAcceptance and ReasonHoldout mark a Notice that learning moved from a
	// message to the digest (see learning.go). They exist only with
	// proactive.learning.adapt on.
	ReasonLowAcceptance = "low_acceptance"
	ReasonHoldout       = "holdout"
	ReasonBudget        = "budget"
	ReasonComposeLLM    = "compose_llm"
	ReasonTemplate      = "template"
	// ReasonOwnerAlertQuiet marks a candidate the owner asked for (an alert they
	// set) that is delivered outside their active hours, where any other candidate
	// would be held for the digest. It replaces compose_llm / template on that
	// decision, so the owner can see why it came at night.
	ReasonOwnerAlertQuiet = "owner_alert_quiet_hours"
	// ReasonDeferred marks a notify or escalate decision the outbox could not
	// hand over in time (its lane was full, or it waited past its lease) and moved
	// to the digest instead.
	ReasonDeferred = "deferred_to_digest"

	// ReasonInhibitedPrefix starts the reason of an inhibited candidate; the
	// dedup key of the inhibitor follows it ("inhibited_by:dn:12:margin").
	ReasonInhibitedPrefix = "inhibited_by:"
)

// InhibitRule says that while a message about WhenPrefix is fresh, messages
// about SuppressPrefix are noise: the first already explains the second. The
// match is on dedup keys.
type InhibitRule struct {
	// WhenPrefix selects the inhibitor: a notify or escalate decision whose
	// dedup key starts with it.
	WhenPrefix string
	// SuppressPrefix selects the candidates that are dropped.
	SuppressPrefix string
	// SameBaseAsset restricts the rule to a candidate and an inhibitor about
	// the same base asset (see baseAsset). When either has none, the rule does
	// not match.
	SameBaseAsset bool
	// For is how long after the inhibitor was decided the rule holds.
	For time.Duration
}

// defaultInhibits is the static inhibition table. A config override can extend
// it later; nothing reads one yet.
var defaultInhibits = []InhibitRule{
	{WhenPrefix: "dn:", SuppressPrefix: "price_cross:", SameBaseAsset: true, For: time.Hour},
	// The delta-neutral health trigger's dedup keys start with its kind, dn_health:
	// (the registry requires the kind as the prefix of every key).
	{WhenPrefix: "dn_health:", SuppressPrefix: "price_cross:", SameBaseAsset: true, For: time.Hour},
	// The portfolio triggers' keys start with their kind (the registry requires it):
	// liquidation_distance:… explains a move_vs_vol:… of the same base asset, and
	// drawdown:portfolio:… (the kind drawdown, subject portfolio) explains any move.
	// TestInhibit_RowsAreNamed pins these rows, so a renamed kind fails there, loudly.
	// (The older liquidation: and move: rows were removed: no key the registry accepts
	// can carry those prefixes.)
	{WhenPrefix: "liquidation_distance:", SuppressPrefix: "move_vs_vol:", SameBaseAsset: true, For: time.Hour},
	{WhenPrefix: "drawdown:portfolio", SuppressPrefix: "move_vs_vol:", SameBaseAsset: false, For: 30 * time.Minute},
}

// knownQuotes are the quote currencies a Subject must end in to be read as a
// market symbol. The list is deliberately short: a Subject such as "stop-loss"
// must not be taken for a symbol, because a wrong base asset silences a real
// alert. A symbol with another quote is simply not inhibited.
var knownQuotes = map[string]bool{
	"USDT": true, "USDC": true, "USD": true, "BUSD": true, "FDUSD": true, "TUSD": true, "DAI": true,
	"THB": true, "EUR": true, "GBP": true, "JPY": true, "KRW": true, "BTC": true, "ETH": true, "BNB": true,
}

const maxAssetRunes = 20

// baseAsset is the upper-case base asset a candidate is about, or "" when it
// has none. Evidence["base_asset"] wins when it is a plain string of letters
// and digits; otherwise it is read from Subject when that is a market symbol:
// "BTC/USDT", "BTC/USDT:USDT", "BTC-USDT" and "BTC-USDT-SWAP" all give "BTC".
func baseAsset(c proactive.Candidate) string {
	if v, ok := c.Evidence["base_asset"].(string); ok {
		if b := cleanAsset(v); b != "" {
			return b
		}
	}
	return assetFromSymbol(c.Subject)
}

func cleanAsset(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || len(s) > maxAssetRunes || !isAlnum(s) {
		return ""
	}
	return s
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return s != ""
}

func assetFromSymbol(subject string) string {
	subject = strings.TrimSpace(subject)
	var base, quote string
	switch {
	case strings.Contains(subject, "/"):
		b, rest, _ := strings.Cut(subject, "/")
		q, settle, hasSettle := strings.Cut(rest, ":")
		if hasSettle && !isAlnum(settle) {
			return ""
		}
		base, quote = b, q
	case strings.Contains(subject, "-"):
		parts := strings.Split(subject, "-")
		switch len(parts) {
		case 2:
		case 3:
			if s := strings.ToUpper(parts[2]); s != "SWAP" && s != "FUTURES" && s != "PERP" {
				return ""
			}
		default:
			return ""
		}
		base, quote = parts[0], parts[1]
	default:
		return ""
	}
	if !knownQuotes[strings.ToUpper(quote)] {
		return ""
	}
	return cleanAsset(base)
}

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
	// InhibitedBy is the dedup key of the fresh delivered (or in-flight) message
	// that explains this candidate under an InhibitRule; "" when none does.
	InhibitedBy string

	InActiveHours bool
	// SendsToday counts the MESSAGES made of non-critical, non-protective
	// notify/escalate decisions that were delivered (or are in flight) today in
	// the profile's time zone. A group of decisions delivered together is one.
	SendsToday int
	Budget     int
	// JoinsPending: the candidate would be taken into a message that is already
	// pending and still open, so it adds no message to SendsToday.
	JoinsPending bool

	// The learning facts. Adapt is false unless proactive.learning.adapt is on, and
	// while it is false rules 7b and 7c cannot match whatever the other fields hold:
	// a zero facts changes nothing.
	Adapt bool
	// Mean is the owner's acceptance posterior mean for this candidate's trigger kind
	// and hour bucket (0.5, the prior, when nothing is known) and Responses how many
	// responses it rests on.
	Mean      float64
	Responses int
	// LowThreshold is the mean below which a Notice goes to the digest.
	LowThreshold float64
	// HoldoutRate and HoldoutDraw: a Notice is held out when the draw, uniform in
	// [0, 1) and made by the store, is below the rate.
	HoldoutRate float64
	HoldoutDraw float64
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
//	5   a fresh inhibitor explains c (see InhibitRule)         drop               inhibited_by:<key>
//	6   now outside Profile.ActiveHours, and not c.OwnerRequested
//	                                                           digest             quiet_hours
//	7   c.Severity == Info                                     digest             info_to_digest
//	7b  adapt, c is learnable (below), no budget hit, the posterior has >= MinEvidence
//	    responses and its mean < the low-acceptance threshold  digest             low_acceptance
//	7c  adapt, c is learnable, no budget hit, and the holdout draw < HoldoutRate
//	                                                           digest             holdout
//	8   Severity < Critical and sends today >= budget          digest             budget
//	    (Warning: budget+1; Critical is never budgeted; a candidate that joins
//	    a still-open pending message adds none and is not held back)
//	9   c.Compose == ComposeLLM                                escalate           compose_llm
//	10  otherwise                                              notify             template
//
// A candidate with OwnerRequested (the firing of an alert the owner set) skips
// rule 6 only: it is delivered in quiet hours as it would be in active hours, and
// rules 9 and 10 then report owner_alert_quiet_hours instead of their own reason
// when it is quiet hours. Rules 1 to 5, 7 and 8 apply to it unchanged, so it can
// still be muted, dropped as a repeat, inhibited, only logged in shadow, held for
// the digest as Info, or held for the digest over the daily budget.
//
// Rules 7b and 7c are the only place learning changes a disposition, and they can
// only move a learnable candidate (see learnable: a Notice that is neither protective
// nor the owner's own alert) from a message to the digest. Rules 0 to 7 and 8 to 10 are
// untouched, so a Warning, a Critical, a protective candidate, an owner's alert and
// anything already dropped, logged or held for the digest is decided as it is without
// learning. Both rules also stand aside when rule 8 would hold the candidate, so that
// the budget keeps its reason and a holdout is only ever drawn against a message that
// would have gone out.
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
//   - Rule 5 runs after rule 4, so a protective Critical is never inhibited, and
//     before rule 6, so an inhibited candidate is dropped rather than sent to
//     the digest: the inhibitor already told the owner. An inhibitor is a
//     notify or escalate decision whose dedup key starts with
//     InhibitRule.WhenPrefix, decided within InhibitRule.For, that was
//     delivered or is still in flight (inside the 10-minute lease). A decision
//     whose send failed does not inhibit: the owner was not told. Rules that
//     need the same base asset never match when either side has none. Rule 5 is
//     evaluated in live mode only; shadow decisions are rule 1.
//   - The budget (rule 8) counts messages, not decisions. Delivered decisions
//     count once per intervention ID (decisions delivered together share one).
//     In-flight decisions count once per group window: decisions made within
//     AttentionConfig.GroupWindowSeconds of the oldest of them are one pending
//     message, which is what the outbox will make of them. With no window every
//     in-flight decision is its own message. A candidate decided while the
//     newest pending message is still open (less than a window after its first
//     decision) joins it and adds no message, so it is not held to the budget
//     that message already used; that is what lets a group of five cost one.
//     A reservation is held until the decision is delivered (stays counted) or
//     reported failed (released). Only notify/escalate rows count; a delivered
//     digest item does not.
//   - The window of rule 3 is the candidate's DedupWindow (else the configured
//     default), except for a protective Critical, whose window is
//     AttentionConfig.CriticalRepeatMinutes when that is set (30 by default): a
//     breach that persists is delivered again at most that often, neither once per
//     dedup window of hours nor on every tick. A protective Critical still ignores
//     in-flight decisions.
//   - Rule 3b does not apply to a protective Critical candidate: it is about
//     things the owner has NOT been told, and a queued item must never swallow
//     the one alert that must not wait.
//   - Rule 4 returns escalate for a protective Critical with ComposeLLM, so the
//     LLM may add an explanation; the alert itself is not gated on it.
//   - A Critical candidate that is not protective still obeys rules 2, 3, 3b, 5
//     and 6 (mute, dedup, inhibition, quiet hours) but is never budgeted: rule 8
//     applies only to severities below Critical, and a delivered Critical does
//     not count toward the budget.
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
	case f.InhibitedBy != "":
		return proactive.DispDrop, ReasonInhibitedPrefix + f.InhibitedBy
	case !f.InActiveHours && !c.OwnerRequested:
		return proactive.DispDigest, ReasonQuietHours
	case c.Severity == proactive.SeverityInfo:
		return proactive.DispDigest, ReasonInfoToDigest
	case f.lowAcceptance(c):
		return proactive.DispDigest, ReasonLowAcceptance
	case f.heldOut(c):
		return proactive.DispDigest, ReasonHoldout
	case f.overBudget(c):
		return proactive.DispDigest, ReasonBudget
	case c.Compose == proactive.ComposeLLM:
		return proactive.DispEscalate, deliverReason(c, f, ReasonComposeLLM)
	default:
		return proactive.DispNotify, deliverReason(c, f, ReasonTemplate)
	}
}

// deliverReason is the reason of a candidate that goes out through rules 9 and 10:
// base, except that an owner's alert delivered in quiet hours says so.
func deliverReason(c proactive.Candidate, f facts, base string) string {
	if c.OwnerRequested && !f.InActiveHours {
		return ReasonOwnerAlertQuiet
	}
	return base
}

// limitFor is how many non-critical sends a day allows a candidate of severity
// s: the budget, plus one for a Warning.
func (f facts) limitFor(s proactive.Severity) int {
	if s == proactive.SeverityWarning {
		return f.Budget + 1
	}
	return f.Budget
}

// overBudget is the condition of rule 8.
func (f facts) overBudget(c proactive.Candidate) bool {
	return c.Severity < proactive.SeverityCritical && f.SendsToday >= f.limitFor(c.Severity) && !f.JoinsPending
}

// learnable reports whether learning may change c at all: a Notice that is not
// protective and not an alert the owner set. Nothing else is ever demoted or held out.
func learnable(c proactive.Candidate) bool {
	return c.Severity == proactive.SeverityNotice && !c.Protective && !c.OwnerRequested
}

// learns: rules 7b and 7c apply to c on these facts.
func (f facts) learns(c proactive.Candidate) bool {
	return f.Adapt && learnable(c) && !f.overBudget(c)
}

func (f facts) lowAcceptance(c proactive.Candidate) bool {
	return f.learns(c) && f.Responses >= MinEvidence && f.Mean < f.LowThreshold
}

func (f facts) heldOut(c proactive.Candidate) bool {
	return f.learns(c) && f.HoldoutRate > 0 && f.HoldoutDraw < f.HoldoutRate
}

func isProtectiveCritical(c proactive.Candidate) bool {
	return c.Protective && c.Severity == proactive.SeverityCritical
}
