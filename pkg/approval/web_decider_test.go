package approval

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

func armWebGate(t *testing.T, ok bool) {
	t.Helper()
	config.SetWebLauncherGate(func(string) (bool, string) { return ok, "no" })
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
}

const webSender = "pico:pico-user"

func decideWeb(p *proc, prop Proposal, ctx context.Context) (Proposal, error) {
	return p.svc.Decide(ctx, prop.Code, prop.Nonce, true, webSender)
}

// A web approval is recorded as the web's and carried all the way to a placed order:
// the re-proof before execution accepts it while the route is active.
func TestDecide_WebOwnerApprovesAndTheOrderIsPlaced(t *testing.T) {
	armWebGate(t, true)
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	got, err := decideWeb(p, prop, WithWebDecider(context.Background()))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.DecidedBy != identity.WebOwner {
		t.Fatalf("DecidedBy = %q, want web", got.DecidedBy)
	}
	p.run(prop.ID)
	if st := p.get(prop.ID).Status; st != StatusExecuted || w.backend.orderCount() != 1 {
		t.Fatalf("status %s, %d orders: a web approval must run through the re-proof", st, w.backend.orderCount())
	}
}

func TestDecide_WebDecisionIsRefusedUnlessEveryConditionHolds(t *testing.T) {
	off := false
	cases := []struct {
		name   string
		gate   bool
		marker bool
		mutate func(*config.Config)
	}{
		{"no web marker (a model or another channel)", true, false, nil},
		{"(c) launcher authentication not established", false, true, nil},
		{"web_is_owner false", true, true, func(c *config.Config) { c.Proactive.Owner.WebIsOwner = &off }},
		{"(a) no owner", true, true, func(c *config.Config) { c.Proactive.Owner = config.OwnerConfig{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			armWebGate(t, tc.gate)
			w := newWorld(t)
			p := w.newProc(t0)
			prop := p.propose(sellBTC())
			if tc.mutate != nil {
				tc.mutate(w.cfg)
			}
			ctx := context.Background()
			if tc.marker {
				ctx = WithWebDecider(ctx)
			}
			_, err := decideWeb(p, prop, ctx)
			mustErrIs(t, err, ErrNotOwner)
			if got := p.get(prop.ID); got.Status != StatusPending || got.DecidedBy != "" {
				t.Errorf("proposal changed: %+v", got)
			}
		})
	}
}

// "web" smuggled in as the sender is no owner, with or without the marker.
func TestDecide_TheStringWebIsNotAnOwner(t *testing.T) {
	armWebGate(t, true)
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	_, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, identity.WebOwner)
	mustErrIs(t, err, ErrNotOwner)
}

// Turning the web route off between the approval and the placement stops the order.
func TestExecute_WebApprovalDoesNotSurviveTheRouteGoingInactive(t *testing.T) {
	armWebGate(t, true)
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	if _, err := decideWeb(p, prop, WithWebDecider(context.Background())); err != nil {
		t.Fatal(err)
	}
	armWebGate(t, false)
	p.run(prop.ID)
	if got := p.get(prop.ID); got.Status != StatusFailed || w.backend.orderCount() != 0 {
		t.Fatalf("status %s, %d orders", got.Status, w.backend.orderCount())
	}
}
