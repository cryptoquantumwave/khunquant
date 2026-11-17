package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dnSpotLeg is the spot leg of one open delta-neutral plan, as the order gate needs it.
type dnSpotLeg struct {
	PlanID          int64
	Name            string
	Provider        string
	Account         string
	Asset           string  // base asset of the spot symbol, e.g. BTC
	Quantity        float64 // net spot quantity recorded from filled legs
	FuturesBase     float64 // net short futures quantity (base units) recorded from filled legs
	UnrecordedFills bool    // an order went through whose leg row was never saved
}

// dnLegSource reads the open plans' spot legs. An error means the plans could not
// be read, and the caller refuses (fail closed).
type dnLegSource func(ctx context.Context) ([]dnSpotLeg, error)

// openPlanStatuses are the statuses of a plan that may hold a spot leg: everything
// between opening and the end of its unwind.
var openPlanStatuses = []string{
	deltaneutral.PlanStatusActive, deltaneutral.PlanStatusPaused,
	deltaneutral.PlanStatusRecoveryRequired, deltaneutral.PlanStatusClosing,
}

// storeDNLegs is the production dnLegSource: it opens the delta-neutral store of
// the workspace, reads the open plans and the net spot quantity recorded for each
// from its filled legs, and closes the store again (the read is rare: a background
// spot sell only).
func (b *ApprovalOrderBackend) storeDNLegs(ctx context.Context) ([]dnSpotLeg, error) {
	store, err := deltaneutral.NewStore(b.cfg.WorkspacePath())
	if err != nil {
		return nil, err
	}
	defer store.Close()
	var legs []dnSpotLeg
	for _, status := range openPlanStatuses {
		st := status
		plans, err := store.ListPlans(ctx, deltaneutral.QueryFilter{Status: &st, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, p := range plans {
			exp, err := store.OpenLegExposure(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			legs = append(legs, dnSpotLeg{
				PlanID: p.ID, Name: p.Name, Provider: p.SpotProvider, Account: p.SpotAccount,
				Asset: baseAsset(p.SpotSymbol), Quantity: exp.SpotQty, FuturesBase: exp.FuturesBase, UnrecordedFills: len(exp.UnrecordedFills) > 0,
			})
		}
	}
	return legs, nil
}

// sameAccount: an empty account is the default account, so an empty side may be any.
// Doubt counts as a match (more refusals, never fewer).
func sameAccount(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a == "" || b == "" || strings.EqualFold(a, b)
}

// gateDNSpotLeg refuses a background (proactive) proposal that sells spot of an
// asset which is the spot leg of an open delta-neutral plan, when the sale would
// take the venue/account's balance of the asset below the sum of those plans' spot
// legs: the plan's short would be left naked. The owner's own proposal is not held
// to this rule (their decision stands), and neither is a buy.
//
// Only the HEDGED part of a plan's spot leg is held: min(recorded spot quantity,
// recorded short quantity). Spot above the short is an orphan (a futures leg that
// never opened), and selling exactly that excess is the plan's own dn.recover close,
// which ApprovalCloser.ProposeClose makes as a proactive proposal and which only
// reduces risk. Holding the whole spot quantity would block that recovery.
//
// It runs at propose time and again at Recheck (a plan may have opened or grown
// since), and fails closed: if the plans cannot be read, the sale is refused.
func (b *ApprovalOrderBackend) gateDNSpotLeg(ctx context.Context, spec orderSpec, p broker.Provider) error {
	if approval.OrderOriginFrom(ctx) != approval.OriginProactive || spec.side != "sell" {
		return nil
	}
	read := b.dnLegs
	if read == nil {
		read = b.storeDNLegs
	}
	legs, err := read(ctx)
	if err != nil {
		return fmt.Errorf("the open delta-neutral plans could not be read, so a sale of spot cannot be shown to leave their spot legs in place: %w", err)
	}
	asset := baseAsset(spec.symbol)
	var held, reserved float64
	var names []string
	for _, l := range legs {
		if !strings.EqualFold(l.Provider, spec.provider) || !sameAccount(l.Account, spec.account) || !strings.EqualFold(l.Asset, asset) {
			continue
		}
		if l.UnrecordedFills {
			// An order went through whose size was never recorded: what the plan holds
			// cannot be read, so no sale can be shown to leave it in place.
			return fmt.Errorf("plan %d (%s) has a fill that was never recorded, so its %s spot leg cannot be sized; reconcile it before a background proposal sells %s",
				l.PlanID, l.Name, asset, asset)
		}
		// Only the hedged part of a spot leg is held for the plan: spot above its short
		// is an orphan, and selling that excess is the plan's own recovery (dn.recover
		// proposes exactly that), which only reduces risk.
		hedged := math.Min(l.Quantity, l.FuturesBase)
		if hedged <= 0 {
			continue
		}
		reserved += hedged
		names = append(names, fmt.Sprintf("plan %d (%s)", l.PlanID, l.Name))
	}
	if len(names) == 0 {
		return nil
	}
	pp, ok := p.(broker.PortfolioProvider)
	if !ok {
		return fmt.Errorf("%s is the spot leg of %s and the balance cannot be read to show the sale leaves it in place", asset, strings.Join(names, ", "))
	}
	bals, err := pp.GetBalances(ctx)
	if err != nil {
		return fmt.Errorf("%s is the spot leg of %s and the balance could not be read to show the sale leaves it in place: %w", asset, strings.Join(names, ", "), err)
	}
	for _, bal := range bals {
		if strings.EqualFold(bal.Asset, asset) {
			held = bal.Free
			break
		}
	}
	if held-spec.amount < reserved-1e-9 {
		return fmt.Errorf("%s is the spot leg of the open delta-neutral %s (%.8g held, %.8g in the spot leg): selling %.8g would leave its short unhedged. "+
			"A background proposal may not sell it; only the owner can decide to", asset, strings.Join(names, ", "), held, reserved, spec.amount)
	}
	return nil
}
