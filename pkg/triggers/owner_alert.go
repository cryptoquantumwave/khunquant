package triggers

import "strings"

// Origins of the specs that are alerts the owner set.
//
// An alert is the owner's when the alert tools wrote it (OriginSetPriceAlert,
// OriginSetIndicatorAlert: the owner asked for it in a chat) or when it was
// migrated from the owner's own alert job in the cron table (MigratedOriginPrefix
// followed by the job's ID). Nothing else is: not a built-in spec, not a portfolio
// trigger, not the external trigger. These strings are the one place the rule lives;
// pkg/tools writes the first two and pkg/triggers/migrate the third, and tests in
// both pin them to these constants.
const (
	OriginSetPriceAlert     = "tool:set_price_alert"
	OriginSetIndicatorAlert = "tool:set_indicator_alert"
	MigratedOriginPrefix    = "migration:cron:"
)

// OwnerAlertSpec reports whether a spec of this kind and origin is an alert the
// owner set, so that its candidates may carry proactive.Candidate.OwnerRequested.
// Both the crossing kinds' factories (which set the mark) and the registry (which
// clears it from anything else) ask it, so a kind not named here cannot acquire the
// mark by accident: adding one is a change to this function and its test.
func OwnerAlertSpec(kind, origin string) bool {
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
