package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// HYP-2: a proactive proposal may not sell the spot leg of an open delta-neutral plan.

func dnVenue(t *testing.T, id string, held float64) *pricedVenue {
	t.Helper()
	v := newPricedVenue(t, id)
	v.setTicker("BTC/USDT", 100, 100, 100)
	v.balances = append(v.balances, broker.Balance{Asset: "BTC", Free: held})
	return v
}

func legsOf(l ...dnSpotLeg) dnLegSource {
	return func(context.Context) ([]dnSpotLeg, error) { return l, nil }
}

func dnProactiveCtx() context.Context {
	return approval.WithOrderOrigin(context.Background(), approval.OriginProactive)
}

func sellReq(venue string, amount float64) approval.OrderRequest {
	return approval.OrderRequest{Provider: venue, Symbol: "BTC/USDT", Side: "sell", Amount: amount}
}

func TestDNLeg_ProactiveSellOfWholeSpotLegRefusedNamingThePlan(t *testing.T) {
	v := dnVenue(t, "dn-whole", 0.005)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 7, Name: "btc-hedge", Provider: v.id, Asset: "BTC", Quantity: 0.005, FuturesBase: 0.005})
	_, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.005))
	if err == nil || !strings.Contains(err.Error(), "plan 7 (btc-hedge)") || !strings.Contains(err.Error(), "delta-neutral") {
		t.Fatalf("selling the whole spot leg from a proactive turn must be refused, naming the plan: %v", err)
	}
}

func TestDNLeg_ProactiveSellLeavingTheLegAllowed(t *testing.T) {
	v := dnVenue(t, "dn-surplus", 0.015)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 7, Name: "btc-hedge", Provider: v.id, Asset: "BTC", Quantity: 0.005, FuturesBase: 0.005})
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.010)); err != nil {
		t.Fatalf("a sale that leaves the whole leg held must be allowed: %v", err)
	}
	// A little more than the surplus eats into the leg.
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.0101)); err == nil {
		t.Fatal("a sale that dips into the leg must be refused")
	}
}

func TestDNLeg_SumsLegsOfSeveralPlans(t *testing.T) {
	v := dnVenue(t, "dn-sum", 0.010)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf(
		dnSpotLeg{PlanID: 1, Name: "a", Provider: v.id, Asset: "BTC", Quantity: 0.004, FuturesBase: 0.004},
		dnSpotLeg{PlanID: 2, Name: "b", Provider: v.id, Asset: "BTC", Quantity: 0.004, FuturesBase: 0.004},
		dnSpotLeg{PlanID: 3, Name: "other-venue", Provider: "elsewhere", Asset: "BTC", Quantity: 9, FuturesBase: 9},
		dnSpotLeg{PlanID: 4, Name: "other-asset", Provider: v.id, Asset: "ETH", Quantity: 9, FuturesBase: 9},
	)
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.002)); err != nil {
		t.Fatalf("0.010 held, 0.008 reserved, selling 0.002: %v", err)
	}
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.003)); err == nil || !strings.Contains(err.Error(), "plan 1") || !strings.Contains(err.Error(), "plan 2") {
		t.Fatalf("selling 0.003 dips into the two legs and must name both plans: %v", err)
	}
}

func TestDNLeg_NoOpenPlanAllowed(t *testing.T) {
	v := dnVenue(t, "dn-none", 0.005)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf()
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.005)); err != nil {
		t.Fatalf("with no open plan the sale is allowed: %v", err)
	}
	// A plan with no recorded spot quantity (a closed one, say) reserves nothing.
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 1, Name: "flat", Provider: v.id, Asset: "BTC", Quantity: 0, FuturesBase: 0})
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.005)); err != nil {
		t.Fatalf("a flat plan reserves nothing: %v", err)
	}
}

func TestDNLeg_OwnerTurnNotRefused(t *testing.T) {
	v := dnVenue(t, "dn-owner", 0.005)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = func(context.Context) ([]dnSpotLeg, error) {
		t.Error("the owner's turn must not even read the plans")
		return nil, errors.New("must not be read")
	}
	for name, ctx := range map[string]context.Context{
		"owner":     approval.WithOrderOrigin(context.Background(), approval.OriginOwner),
		"no-origin": context.Background(),
	} {
		if _, err := b.Prepare(ctx, sellReq(v.id, 0.005)); err != nil {
			t.Errorf("%s: the owner's own sale of the leg must stand: %v", name, err)
		}
	}
}

func TestDNLeg_BuyNotRefused(t *testing.T) {
	v := dnVenue(t, "dn-buy", 0.005)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = func(context.Context) ([]dnSpotLeg, error) { return nil, errors.New("must not be read") }
	req := sellReq(v.id, 0.001)
	req.Side = "buy"
	if _, err := b.Prepare(dnProactiveCtx(), req); err != nil && strings.Contains(err.Error(), "delta-neutral") {
		t.Fatalf("a buy is not held to the rule: %v", err)
	}
}

func TestDNLeg_PlanStoreErrorRefuses(t *testing.T) {
	v := dnVenue(t, "dn-err", 1)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = func(context.Context) ([]dnSpotLeg, error) { return nil, errors.New("database is locked") }
	_, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.001))
	if err == nil || !strings.Contains(err.Error(), "could not be read") || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("an unreadable plan store must refuse a proactive spot sell: %v", err)
	}
}

// An orphan spot leg (its futures leg never opened) hedges nothing: selling it is the
// plan's own recovery and must not be refused. For a partial orphan only the hedged part is held.
func TestDNLeg_OrphanSpotLegSellAllowedHedgedPartHeld(t *testing.T) {
	v := dnVenue(t, "dn-orphan", 0.005)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 1, Name: "orphan", Provider: v.id, Asset: "BTC", Quantity: 0.005, FuturesBase: 0})
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.005)); err != nil {
		t.Fatalf("selling a spot leg with no short is the recovery close: %v", err)
	}
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 1, Name: "partial", Provider: v.id, Asset: "BTC", Quantity: 0.005, FuturesBase: 0.003})
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.002)); err != nil {
		t.Fatalf("selling the 0.002 that has no short must be allowed: %v", err)
	}
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.003)); err == nil {
		t.Fatal("selling 0.003 dips into the hedged 0.003 and must be refused")
	}
}

// A plan with an unrecorded fill cannot be sized: refuse.
func TestDNLeg_UnrecordedFillRefuses(t *testing.T) {
	v := dnVenue(t, "dn-unrec", 1)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 5, Name: "u", Provider: v.id, Asset: "BTC", UnrecordedFills: true})
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.001)); err == nil || !strings.Contains(err.Error(), "plan 5 (u)") {
		t.Fatalf("an unsized plan must refuse: %v", err)
	}
}

func TestDNLeg_NoBalanceRowRefuses(t *testing.T) {
	// A plan holds the asset but the balance has no row for it: nothing shows the
	// sale leaves the leg in place.
	v := dnVenue(t, "dn-nobal", 0)
	v.balances = []broker.Balance{{Asset: "USDT", Free: 1e9}}
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 1, Name: "x", Provider: v.id, Asset: "BTC", Quantity: 0.005, FuturesBase: 0.005})
	if _, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.001)); err == nil {
		t.Fatal("a sale of an asset the account does not hold, against an open leg, must be refused")
	}
}

// The approval.order recheck applies the same rule: a plan that opened (or grew)
// after the proposal stops the order.
func TestDNLeg_RecheckRefusesWhenAPlanOpenedAfterTheProposal(t *testing.T) {
	v := dnVenue(t, "dn-recheck", 0.005)
	b := NewApprovalOrderBackend(capConfig(0))
	b.dnLegs = legsOf()
	prep, err := b.Prepare(dnProactiveCtx(), sellReq(v.id, 0.005))
	if err != nil {
		t.Fatalf("no plan at propose time: %v", err)
	}
	if _, err := b.Recheck(dnProactiveCtx(), prep.Params); err != nil {
		t.Fatalf("still no plan at recheck: %v", err)
	}
	b.dnLegs = legsOf(dnSpotLeg{PlanID: 9, Name: "late", Provider: v.id, Asset: "BTC", Quantity: 0.005, FuturesBase: 0.005})
	_, err = b.Recheck(dnProactiveCtx(), prep.Params)
	if err == nil || !strings.Contains(err.Error(), "plan 9 (late)") {
		t.Fatalf("a plan opened after the proposal must stop the order at recheck: %v", err)
	}
	// The owner's own proposal is not stopped by it.
	if _, err := b.Recheck(approval.WithOrderOrigin(context.Background(), approval.OriginOwner), prep.Params); err != nil {
		t.Fatalf("the owner's own order is not held to the rule at recheck: %v", err)
	}
}

// storeDNLegs reads the real store: open plans and the net spot quantity of their
// filled legs; a closed plan nets to nothing and a draft is not read.
func TestDNLeg_StoreDNLegsReadsOpenPlansAndFilledQuantity(t *testing.T) {
	cfg := capConfig(0)
	cfg.Agents.Defaults.Workspace = t.TempDir()
	store, err := deltaneutral.NewStore(cfg.WorkspacePath())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(name, status string) int64 {
		id, err := store.SavePlan(ctx, &deltaneutral.Plan{
			Name: name, Asset: "BTC", Status: status, Mode: deltaneutral.ExecutionModeApproval,
			SpotProvider: "binance", SpotSymbol: "BTC/USDT", SpotSide: "buy",
			FuturesProvider: "binance", FuturesSymbol: "BTC/USDT:USDT", FuturesSide: "short",
			FuturesLeverage: 1, CapitalUSDT: 1000, RiskPolicy: deltaneutral.DefaultRiskPolicy(), CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fill := func(planID int64, side string, qty float64) {
		eid, err := store.SaveExecution(ctx, &deltaneutral.Execution{PlanID: planID, AttemptID: "a", State: "completed", RequestedAt: now, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SaveExecutionLeg(ctx, &deltaneutral.ExecutionLeg{
			ExecutionID: eid, LegType: string(deltaneutral.LegTypeSpot), Provider: "binance", Symbol: "BTC/USDT", Side: side,
			OrderType: "market", State: "filled", FilledQuantity: qty, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fill(mk("open", deltaneutral.PlanStatusActive), "buy", 0.005)
	closed := mk("closed", deltaneutral.PlanStatusClosed)
	fill(closed, "buy", 0.005)
	fill(closed, "sell", 0.005)
	mk("draft", deltaneutral.PlanStatusDraft)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	legs, err := NewApprovalOrderBackend(cfg).storeDNLegs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 1 || legs[0].Name != "open" || legs[0].Quantity != 0.005 || legs[0].Asset != "BTC" || legs[0].Provider != "binance" {
		t.Fatalf("legs = %+v", legs)
	}
}
