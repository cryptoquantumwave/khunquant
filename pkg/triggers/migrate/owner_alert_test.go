package migrate

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func TestOriginPrefixIsTheOwnerAlertPrefix(t *testing.T) {
	if OriginPrefix != triggers.MigratedOriginPrefix {
		t.Fatalf("OriginPrefix = %q, triggers.MigratedOriginPrefix = %q", OriginPrefix, triggers.MigratedOriginPrefix)
	}
}

// An alert the owner set is the owner's whichever way it reached the registry: made
// by the tool while triggers were live, or promoted from a cron job, and it stays
// theirs across a rollback and a second promotion.
func TestOwnerAlert_MarkSurvivesPromotionAndRollback(t *testing.T) {
	w := newWorld(t)
	a := w.price("price_alert:a", defaultPrice())
	b := w.indicator("indicator_alert:b", defaultIndicator())
	tool := w.addTool(priceTool(60000, true))

	owner := func(when string) {
		t.Helper()
		for _, id := range []string{a.ID, b.ID} {
			s, ok := w.specFor(id)
			if !ok || !triggers.OwnerAlertSpec(s.Kind, s.Origin) {
				t.Fatalf("%s: the spec of cron job %s is not the owner's alert: %+v (found %v)", when, id, s, ok)
			}
		}
		s, ok := w.spec(tool.ID)
		if !ok || !triggers.OwnerAlertSpec(s.Kind, s.Origin) {
			t.Fatalf("%s: the tool's spec is not the owner's alert: %+v", when, s)
		}
	}

	w.sync(Shadow)
	owner("imported in shadow")
	w.sync(Live)
	owner("promoted")
	w.sync(Shadow)
	owner("rolled back")
	w.sync(Live)
	owner("promoted again")
}
