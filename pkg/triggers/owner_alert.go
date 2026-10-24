package triggers

import "strings"

// Origins of the specs that are alerts the owner set.
//
// An alert is the owner's when the alert tools wrote it (OriginSetPriceAlert,
// OriginSetIndicatorAlert: the owner asked for it in a chat), when it was
// migrated from the owner's own alert job in the cron table (MigratedOriginPrefix
// followed by the job's ID), or when the intent compiler wrote it for a watch or
// reminder intent that is ACTIVE (IntentOriginPrefix followed by the intent's ID).
// Nothing else is: not a built-in spec, not a portfolio trigger, not the external
// trigger, not a standing order. These strings are the one place the rule lives;
// pkg/tools writes the first two, pkg/triggers/migrate the third and
// pkg/triggers/intents the fourth, and tests in each pin them to these constants.
const (
	OriginSetPriceAlert     = "tool:set_price_alert"
	OriginSetIndicatorAlert = "tool:set_indicator_alert"
	MigratedOriginPrefix    = "migration:cron:"

	// IntentOriginPrefix is the origin of a spec compiled from an ACTIVE watch or
	// reminder intent, followed by the intent's id ("intent:<id>").
	// IntentOrderOriginPrefix is the origin of one compiled from a standing order.
	// They differ at the sixth character on purpose: OwnerAlertSpec accepts the
	// first and never the second, so a standing order's firing is a Notice like any
	// other and cannot break quiet hours.
	IntentOriginPrefix      = "intent:"
	IntentOrderOriginPrefix = "intent-order:"
)

// IntentOrigin is the origin of the spec compiled from the watch or reminder id.
func IntentOrigin(id string) string { return IntentOriginPrefix + id }

// IntentOrderOrigin is the origin of the spec compiled from the standing order id.
func IntentOrderOrigin(id string) string { return IntentOrderOriginPrefix + id }

// IntentOfOrigin reads the intent id out of an origin written by IntentOrigin or
// IntentOrderOrigin. The id must be a single plain token: "intent:a:b" is neither.
func IntentOfOrigin(origin string) (id string, standingOrder, ok bool) {
	if rest, found := strings.CutPrefix(origin, IntentOriginPrefix); found {
		return rest, false, validToken(rest, maxIDLen, true)
	}
	if rest, found := strings.CutPrefix(origin, IntentOrderOriginPrefix); found {
		return rest, true, validToken(rest, maxIDLen, true)
	}
	return "", false, false
}

// OwnerAlertSpec reports whether a spec of this kind and origin is an alert the
// owner set, so that its candidates may carry proactive.Candidate.OwnerRequested.
// Both the crossing kinds' factories (which set the mark) and the registry (which
// clears it from anything else) ask it, so a kind not named here cannot acquire the
// mark by accident: adding one is a change to this function and its test.
//
// A watch or a reminder the owner made as an intent qualifies: a price_cross, an
// indicator_cross or a time_at spec with the origin "intent:<id>". That is safe only
// because of who can write such an origin: the intent compiler writes it, and only
// for an intent that is ACTIVE, and only the owner's own message can make an intent
// active (the ledger refuses an active intent that is not explicit and
// user-authored; the intent tools apply the owner check). A test lists every writer
// of the origin and fails on a new one.
func OwnerAlertSpec(kind, origin string) bool {
	if id, order, ok := IntentOfOrigin(origin); ok {
		if order || id == "" {
			return false
		}
		switch kind {
		case KindPriceCross, KindIndicatorCross, KindTimeAt:
			return true
		}
		return false
	}
	var tool string
	switch kind {
	case KindPriceCross:
		tool = OriginSetPriceAlert
	case KindIndicatorCross:
		tool = OriginSetIndicatorAlert
	default:
		return false
	}
	if origin == tool {
		return true
	}
	rest, ok := strings.CutPrefix(origin, MigratedOriginPrefix)
	return ok && rest != ""
}
