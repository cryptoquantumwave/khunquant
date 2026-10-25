package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// The intent ledger is read for changes: a watch on a symbol that the owner created or
// changed counts, the ledger's own expiry does not, and an intent with no symbol has no
// subject to match.
func TestIntentChanges_ReadsTheLedgerByTheTriggersSymbol(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	st, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	prov := intent.Provenance{SenderID: "telegram:1001", Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser}

	watch, err := st.CreateIntent(ctx, intent.Intent{
		Type: intent.TypeWatch, Title: "BTC below 60k", Status: intent.StatusActive, Source: intent.SourceExplicit, Provenance: prov,
		Trigger: &intent.TriggerSpec{Kind: "price_cross", Params: json.RawMessage(`{"symbol":"BTC/USDT","below":60000}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIntent(ctx, intent.Intent{
		Type: intent.TypeGoal, Title: "grow", Status: intent.StatusActive, Source: intent.SourceExplicit, Provenance: prov,
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := st.Transition(ctx, watch.ID, intent.StatusCancelled, prov, "no longer needed"); err != nil {
		t.Fatal(err)
	}

	got, err := intentChanges{db: db}.ChangesSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("changes = %+v, want the creation and the cancellation of the BTC watch", got)
	}
	for _, c := range got {
		if c.Source != feedback.ChangeIntent || c.Subject != "BTC/USDT" || c.Event == "" {
			t.Errorf("change = %+v", c)
		}
	}
	if got[0].Event == got[1].Event {
		t.Errorf("two changes share one event key: %+v", got)
	}
	if after, _ := (intentChanges{db: db}).ChangesSince(ctx, now); len(after) != 0 {
		t.Errorf("changes since the last one = %+v, want none", after)
	}
}
