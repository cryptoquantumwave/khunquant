package simchat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// fakeExchange accepts any order; the test decides nothing about trading.
type fakeExchange struct{}

func (fakeExchange) Prepare(_ context.Context, req approval.OrderRequest) (approval.Prepared, error) {
	return approval.Prepared{Params: approval.OrderParams{
		Provider: req.Provider, Account: req.Account, Market: approval.MarketSpot, Symbol: req.Symbol, Side: req.Side,
		Type: "market", Amount: req.Amount, RefPrice: 60000, NotionalUSD: 738, PriceCollarPct: approval.DefaultCollarPct,
	}, Reducing: true, Mode: approval.ModeLive}, nil
}

func (fakeExchange) Recheck(context.Context, approval.OrderParams) (approval.Rechecked, error) {
	return approval.Rechecked{Reducing: true, Mode: approval.ModeLive, NotionalUSD: 738, LastPrice: 60000}, nil
}
func (fakeExchange) Throttle(approval.OrderParams) (bool, time.Duration) { return true, 0 }
func (fakeExchange) PlaceApprovedOrder(context.Context, approval.OrderParams, string, int, time.Time, approval.Rechecked) (approval.Placed, error) {
	return approval.Placed{OrderID: "x"}, nil
}

func (fakeExchange) Fetch(context.Context, approval.OrderParams, string) (approval.Fill, error) {
	return approval.Fill{}, nil
}

func (fakeExchange) Record(context.Context, approval.OrderParams, approval.Rechecked, approval.Placed, approval.Fill) {
}

// simNotifier is the owner's chat: the approval service's card goes out through the
// channel's own SendWithIDs, as notify would hand it to the manager.
type simNotifier struct {
	mu sync.Mutex
	ch *Channel
}

func (n *simNotifier) Deliver(ctx context.Context, nt notify.Notification) (notify.Result, error) {
	ids, err := n.ch.SendWithIDs(ctx, bus.OutboundMessage{
		Channel: channelName, ChatID: "dm:owner", Content: nt.Title + "\n" + nt.Body, Actions: nt.Actions,
		Severity: nt.Severity, DedupKey: nt.DedupKey, InterventionID: nt.InterventionID,
	})
	if err != nil {
		return notify.Result{}, err
	}
	return notify.Result{Delivered: true, PlatformMessageIDs: ids,
		Route: proactive.OwnerRoute{Channel: channelName, ChatID: "dm:owner", SenderID: "simchat:owner"}}, nil
}

type cardEditor struct{ ch *Channel }

func (e cardEditor) EditCard(ctx context.Context, _, chatID, messageID, text string) error {
	return e.ch.EditMessage(ctx, chatID, messageID, text)
}

// A real order-approval card (rendered by the approval service, delivered through the
// channel) carries approve and reject buttons whose data parses, and pressing reject as
// the owner goes through the real control dispatcher: the proposal is rejected and the
// card is edited with its buttons removed.
func TestPressRejectOnApprovalCard(t *testing.T) {
	r := newRig(t)
	cfg := testConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: channelName, ChatID: "dm:owner", SenderID: "simchat:owner"}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	svc, err := approval.New(context.Background(), approval.Options{
		DB: sqlitextest.Open(t), Cfg: cfg, Backend: fakeExchange{}, Notifier: &simNotifier{ch: r.ch}, Editor: cardEditor{ch: r.ch},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ch.SetControlRouter(control.New(control.Deps{Cfg: cfg, Approvals: svc}))

	p, err := svc.Propose(context.Background(), approval.ProposeRequest{
		Order:  approval.OrderRequest{Provider: "okx", Account: "main", Symbol: "BTC/USDT", Side: "sell", Amount: 0.0123},
		Origin: approval.OriginOwner,
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := r.mustRun("outbox", `{"to":"dm:owner"}`)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("outbox = %v, want the card", msgs)
	}
	have := map[string]bool{}
	for _, b := range msgs[0].(map[string]any)["buttons"].([]any) {
		btn := b.(map[string]any)
		id, _, err := channels.ParseCallbackData(btn["data"].(string), 64)
		if err != nil || id != btn["action"] {
			t.Errorf("button %v does not parse: %q %v", btn, id, err)
		}
		have[id] = true
	}
	if !have["approve"] || !have["reject"] {
		t.Fatalf("card buttons = %v, want approve and reject", have)
	}

	res := r.mustRun("press", `{"on":{"to":"dm:owner","severity":"warning","contains":["BTC"]},"action":"reject","as":"owner"}`)
	if res["edit"] != true && res["text"] == "" {
		t.Fatalf("press result = %v", res)
	}
	got, err := svc.Get(context.Background(), p.ID)
	if err != nil || got.Status != approval.StatusRejected {
		t.Fatalf("the proposal is %q (%v), want rejected", got.Status, err)
	}
	msgs = r.mustRun("outbox", `{"to":"dm:owner"}`)["messages"].([]any)
	if card := msgs[0].(map[string]any); card["buttons_live"] != false {
		t.Fatalf("the card still has live buttons: %v", card)
	}
	if es := r.entries("chat.edit"); len(es) == 0 || payloadOf(t, es[0])["buttons_removed"] != true {
		t.Fatalf("chat.edit entries = %+v, want buttons_removed", es)
	}
	if _, err := r.run("press", `{"on":{"to":"dm:owner"},"action":"approve","as":"owner"}`); err == nil {
		t.Fatal("the removed approve button was tapped")
	}
}
