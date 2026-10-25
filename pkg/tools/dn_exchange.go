package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dnExchange implements deltaneutral.Exchange over the order helpers and gates of
// this package, the same ones the order tools and the approval backend use.
type dnExchange struct {
	cfg *config.Config
	now func() time.Time
	// real: this exchange acts for exits, on the real providers whatever paper mode says.
	real bool
}

// NewDNExchange builds the exchange the delta-neutral workflows run on. now is the
// engine's clock (the reconcile's settle delay is measured on it).
func NewDNExchange(cfg *config.Config, now func() time.Time) deltaneutral.Exchange {
	if now == nil {
		now = time.Now
	}
	return &dnExchange{cfg: cfg, now: now}
}

// baseAsset is the base asset of a spot symbol ("BTC/USDT" -> "BTC").
func baseAsset(symbol string) string {
	if i := strings.Index(symbol, "/"); i > 0 {
		return symbol[:i]
	}
	return symbol
}

func (e *dnExchange) spot(p *deltaneutral.Plan) (broker.TradingProvider, broker.Provider, error) {
	if e.real { // an exit is real: a simulated sell would leave the real position open
		prov, err := broker.CreateProviderForAccount(p.SpotProvider, p.SpotAccount, e.cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("spot provider: %w", err)
		}
		tp, ok := prov.(broker.TradingProvider)
		if !ok {
			return nil, nil, errors.New("spot provider does not support order execution")
		}
		return tp, prov, nil
	}
	tp, real, err := broker.TradingProviderFor(e.cfg, p.SpotProvider, p.SpotAccount)
	if err != nil {
		return nil, nil, fmt.Errorf("spot provider: %w", err)
	}
	return tp, real, nil
}

// Exits is this exchange acting for exits.
func (e *dnExchange) Exits() deltaneutral.Exchange {
	return &dnExchange{cfg: e.cfg, now: e.now, real: true}
}

// Account reads the live futures position (the plan's side) and the spot balances.
// The contract size is the position's own, else the market's; if neither says, the
// account is unreadable, because sizing a close in contracts without it would be a guess.
func (e *dnExchange) Account(ctx context.Context, p *deltaneutral.Plan) (deltaneutral.Account, error) {
	a := deltaneutral.Account{ContractSize: 1, MinContracts: 1}
	fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
	if err != nil {
		return a, fmt.Errorf("futures provider: %w", err)
	}
	positions, err := fp.FetchFuturesPositions(ctx, []string{p.FuturesSymbol})
	if err != nil {
		return a, fmt.Errorf("read the futures position: %w", err)
	}
	// A market that no longer validates (delisted, inactive) must not keep a position open:
	// the catalogue is read for the sizes only.
	mkts, _ := fp.LoadFuturesMarkets(ctx)
	mkt := mkts[p.FuturesSymbol]
	marketSize := 0.0
	if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
		marketSize = *mkt.ContractSize
	}
	if mkt.Limits.Amount.Min != nil && *mkt.Limits.Amount.Min > 0 {
		a.MinContracts = *mkt.Limits.Amount.Min
	}
	a.ContractSize = marketSize
	_, planSide, _ := futuresPositionSide(p.FuturesSide)
	for _, pos := range positions {
		if pos.Contracts == nil || *pos.Contracts == 0 || pos.Symbol == nil ||
			normalizeFuturesSymbol(*pos.Symbol) != normalizeFuturesSymbol(p.FuturesSymbol) {
			continue
		}
		// In hedge mode the account may hold the other side of the symbol too (the owner's
		// own): that one is not this plan's, whatever it is worth. A position that reports
		// no side, or "net", stands for the plan's.
		if pos.Side != nil && planSide != "" && !strings.EqualFold(*pos.Side, "net") && !strings.EqualFold(*pos.Side, planSide) {
			continue
		}
		a.Contracts, a.PnL = math.Abs(*pos.Contracts), pos.UnrealizedPnl
		if pos.Side != nil && !strings.EqualFold(*pos.Side, "net") {
			a.PositionSide = *pos.Side
		}
		if pos.MarginMode != nil {
			a.MarginMode = *pos.MarginMode
		}
		if pos.ContractSize != nil && *pos.ContractSize > 0 {
			a.ContractSize = *pos.ContractSize
		}
		break
	}
	if !positiveFinite(a.ContractSize) {
		if a.Contracts > 0 {
			return a, fmt.Errorf("the contract size of %s cannot be determined, so a close cannot be sized in contracts", p.FuturesSymbol)
		}
		a.ContractSize = 1
	}
	_, sp, err := e.spot(p)
	if err != nil {
		return a, err
	}
	pp, ok := sp.(broker.PortfolioProvider)
	if !ok {
		return a, errors.New("the spot provider cannot report balances")
	}
	bals, err := pp.GetBalances(ctx)
	if err != nil {
		return a, fmt.Errorf("read the spot balance: %w", err)
	}
	for _, b := range bals {
		if strings.EqualFold(b.Asset, baseAsset(p.SpotSymbol)) {
			a.SpotFree += b.Free
			a.SpotTotal += b.Free + b.Locked
		}
	}
	return a, nil
}

// ExitOrder builds a closing order: reduce-only futures on the side opposite the live
// position (some responses carry no side, or "net": the plan's own position side then
// stands for it when choosing the order side, so the recorded leg offsets the entry),
// and a spot sell (a buy to close a spot short).
func (e *dnExchange) ExitOrder(p *deltaneutral.Plan, leg deltaneutral.LegType, amount float64, a deltaneutral.Account) (deltaneutral.LegOrder, error) {
	if leg == deltaneutral.LegTypeFutures {
		of := a.PositionSide
		if of == "" {
			if _, planPos, err := futuresPositionSide(p.FuturesSide); err == nil {
				of = planPos
			}
		}
		mode := a.MarginMode
		if mode == "" {
			mode = p.FuturesMarginMode
		}
		return deltaneutral.LegOrder{Leg: leg, Side: futuresCloseSide(of), PositionSide: a.PositionSide, Amount: amount,
			Exit: true, MarginMode: mode, PnlBefore: a.PnL, LivePosition: a.Contracts}, nil
	}
	side := "sell"
	if strings.EqualFold(p.SpotSide, "sell") {
		side = "buy"
	}
	return deltaneutral.LegOrder{Leg: leg, Side: side, Amount: amount, Exit: true}, nil
}

// Lookup finds the order an earlier attempt of a step placed, by the client order ID
// derived from key. supported is false when the adapter cannot take a client order ID
// and look it up.
func (e *dnExchange) Lookup(ctx context.Context, p *deltaneutral.Plan, leg deltaneutral.LegType, key string) (deltaneutral.Order, bool, bool, error) {
	var (
		target      broker.Provider
		market, sym = broker.OrderMarketSpot, p.SpotSymbol
		legOrder    = deltaneutral.LegOrder{Leg: leg}
	)
	if leg == deltaneutral.LegTypeFutures {
		fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
		if err != nil {
			return deltaneutral.Order{}, false, false, err
		}
		target, market, sym = fp, broker.OrderMarketFutures, p.FuturesSymbol
	} else {
		tp, _, err := e.spot(p)
		if err != nil {
			return deltaneutral.Order{}, false, false, err
		}
		target = tp
	}
	capable, ok := target.(broker.ClientIDCapable)
	lookup, ok2 := target.(broker.ClientIDLookup)
	if !ok || !ok2 {
		return deltaneutral.Order{}, false, false, nil
	}
	field, maxLen, ok := capable.ClientOrderIDKey(market)
	if !ok || field == "" || maxLen < broker.MinClientIDLen {
		return deltaneutral.Order{}, false, false, nil
	}
	o, found, err := lookup.FetchOrderByClientID(ctx, market, sym, broker.ClientOrderID(key, maxLen))
	if err != nil || !found {
		return deltaneutral.Order{}, false, true, err
	}
	return dnOrderOf(o, legOrder), true, true, nil
}

// Holdings reads the futures position (on the plan's side) and the spot balance.
func (e *dnExchange) Holdings(ctx context.Context, p *deltaneutral.Plan) (deltaneutral.Holdings, error) {
	var h deltaneutral.Holdings
	fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
	if err != nil {
		return h, fmt.Errorf("futures provider: %w", err)
	}
	_, posSide, err := futuresPositionSide(p.FuturesSide)
	if err != nil {
		return h, err
	}
	mkt, err := validateActiveSwapMarket(ctx, fp, p.FuturesSymbol, 0)
	if err != nil {
		return h, err
	}
	h.ContractSize = 1
	if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
		h.ContractSize = *mkt.ContractSize
	}
	positions, err := fp.FetchFuturesPositions(ctx, []string{p.FuturesSymbol})
	if err != nil {
		return h, fmt.Errorf("read the futures position: %w", err)
	}
	for _, pos := range positions {
		if pos.Symbol == nil || !strings.EqualFold(*pos.Symbol, p.FuturesSymbol) || pos.Contracts == nil {
			continue
		}
		if pos.Side != nil && !strings.EqualFold(*pos.Side, posSide) {
			continue
		}
		h.FuturesContracts += math.Abs(*pos.Contracts)
	}
	_, sp, err := e.spot(p)
	if err != nil {
		return h, err
	}
	pp, ok := sp.(broker.PortfolioProvider)
	if !ok {
		return h, errors.New("the spot provider cannot report balances")
	}
	bals, err := pp.GetBalances(ctx)
	if err != nil {
		return h, fmt.Errorf("read the spot balance: %w", err)
	}
	for _, b := range bals {
		if strings.EqualFold(b.Asset, baseAsset(p.SpotSymbol)) {
			h.SpotBase += b.Free + b.Locked
		}
	}
	return h, nil
}

// Quote validates the plan against the markets and sizes the first leg.
func (e *dnExchange) Quote(ctx context.Context, p *deltaneutral.Plan, first deltaneutral.LegType) (deltaneutral.Sizing, error) {
	return e.quote(ctx, p, first, p.SpotNotionalUSDT, true)
}

// QuoteDelta sizes a resize, futures leg first. An increase hedges the spot quantity
// NET of the spot taker fee, which comes off what is bought; a decrease sells exactly
// the notional, whose fee comes out of the proceeds.
func (e *dnExchange) QuoteDelta(ctx context.Context, p *deltaneutral.Plan, usd float64, decrease bool) (deltaneutral.Sizing, error) {
	return e.quote(ctx, p, deltaneutral.LegTypeFutures, usd, !decrease)
}

func (e *dnExchange) quote(ctx context.Context, p *deltaneutral.Plan, first deltaneutral.LegType, notional float64, netOfFee bool) (deltaneutral.Sizing, error) {
	var sz deltaneutral.Sizing
	fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
	if err != nil {
		return sz, fmt.Errorf("futures provider: %w", err)
	}
	if _, _, err := futuresPositionSide(p.FuturesSide); err != nil {
		return sz, fmt.Errorf("invalid futures_side %q: %w", p.FuturesSide, err)
	}
	lev := p.FuturesLeverage
	if lev <= 0 {
		lev = 1
	}
	mkt, err := validateActiveSwapMarket(ctx, fp, p.FuturesSymbol, int64(lev))
	if err != nil {
		return sz, fmt.Errorf("market validation: %w", err)
	}
	if sz.Mark, err = fp.FetchFuturesMarkPrice(ctx, p.FuturesSymbol); err != nil || !positiveFinite(sz.Mark) {
		return sz, fmt.Errorf("futures mark price unavailable: %v", err)
	}
	sz.ContractSize, sz.MinContracts = 1, 1
	if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
		sz.ContractSize = *mkt.ContractSize
	}
	if mkt.Limits.Amount.Min != nil && *mkt.Limits.Amount.Min > 0 {
		sz.MinContracts = *mkt.Limits.Amount.Min
	}
	_, sp, err := e.spot(p)
	if err != nil {
		return sz, err
	}
	if st, err := sp.GetMarketStatus(ctx, p.SpotSymbol); err == nil && st == broker.MarketClosed {
		return sz, fmt.Errorf("spot market %s is closed", p.SpotSymbol)
	}
	md, ok := sp.(broker.MarketDataProvider)
	if !ok {
		return sz, errors.New("spot provider does not support market data")
	}
	tk, err := md.FetchTicker(ctx, p.SpotSymbol)
	if err != nil || tk.Last == nil || !positiveFinite(*tk.Last) {
		return sz, fmt.Errorf("spot price unavailable: %v", err)
	}
	sz.SpotPrice = *tk.Last
	// The account as it is before the open: the baselines a later reconcile and the
	// recovery's assessment compare with.
	h, err := e.Holdings(ctx, p)
	if err != nil {
		return sz, err
	}
	sz.FuturesBefore, sz.SpotBefore = h.FuturesContracts, h.SpotBase

	if first == deltaneutral.LegTypeFutures {
		// The futures leg covers the NET spot quantity after the spot taker fee,
		// rounded down to the lot step (see contractsFromNotional).
		if netOfFee {
			notional *= 1 - deltaneutral.SpotTakerFee
		}
		sz.FirstAmount, err = contractsFromNotional(notional, sz.Mark, sz.ContractSize, sz.MinContracts)
		if err != nil {
			return sz, fmt.Errorf("contract count computation: %w", err)
		}
	} else {
		sz.FirstAmount = notional / sz.SpotPrice
	}
	if !positiveFinite(sz.FirstAmount) {
		return sz, fmt.Errorf("the plan's size is below one tradable step of %s", p.FuturesSymbol)
	}
	return sz, nil
}

// LegOrder builds the order of a leg: the futures side is the ORDER side (buy or
// sell) and the position side is separate.
func (e *dnExchange) LegOrder(p *deltaneutral.Plan, leg deltaneutral.LegType, amount float64) (deltaneutral.LegOrder, error) {
	if leg == deltaneutral.LegTypeFutures {
		side, pos, err := futuresPositionSide(p.FuturesSide)
		if err != nil {
			return deltaneutral.LegOrder{}, fmt.Errorf("invalid futures_side %q: %w", p.FuturesSide, err)
		}
		return deltaneutral.LegOrder{Leg: leg, Side: side, PositionSide: pos, Amount: amount}, nil
	}
	return deltaneutral.LegOrder{Leg: leg, Side: strings.ToLower(strings.TrimSpace(p.SpotSide)), Amount: amount}, nil
}

// Gate runs the order gates of this repository for an order that adds risk, in
// their order: permission, the cap valued in USD (an unknown value refuses), the
// per-account daily loss, and for futures the leverage opt-in. An exit meets the
// permission and the leverage opt-in for reducing only; a hedge completion (the second
// leg of an entry, see deltaneutral.LegOrder.Hedge) meets the permission only.
func (e *dnExchange) Gate(ctx context.Context, p *deltaneutral.Plan, sz deltaneutral.Sizing, o deltaneutral.LegOrder) error {
	provider, account, symbol := p.SpotProvider, p.SpotAccount, p.SpotSymbol
	if o.Leg == deltaneutral.LegTypeFutures {
		provider, account, symbol = p.FuturesProvider, p.FuturesAccount, p.FuturesSymbol
	}
	if err := broker.CheckPermission(e.cfg, provider, account, config.ScopeTrade); err != nil {
		return fmt.Errorf("%s leg permission denied: %w", o.Leg, err)
	}
	if o.Hedge { // completing a hedge whose first leg filled: permission is the only brake
		return nil
	}
	if o.Exit { // getting out: neither the cap nor the daily-loss brake may stop it
		if o.Leg == deltaneutral.LegTypeFutures {
			return broker.CheckLeverage(e.cfg, "exit delta-neutral position", broker.RiskReduce)
		}
		return nil
	}
	if o.Leg != deltaneutral.LegTypeFutures {
		// A contract symbol must never be bought through the spot path. An exit and a hedge
		// completion return above, so getting out of a position stays possible.
		if msg := contractSymbolRefusal(symbol); msg != "" {
			return fmt.Errorf("spot leg: %s", msg)
		}
	}
	var usd float64
	if o.Leg == deltaneutral.LegTypeFutures {
		if broker.IsPaperMode(e.cfg) {
			return fmt.Errorf("%w: futures orders have no simulation", broker.ErrPaperModeUnsupported)
		}
		if !exchanges.USDLike(broker.SymbolQuote(symbol, provider)) {
			return fmt.Errorf("%s is not quoted in a USD-like currency, so its USD value cannot be established", symbol)
		}
		usd = positionNotionalUSD(o.Amount, sz.ContractSize, sz.Mark)
	} else {
		_, sp, err := e.spot(p)
		if err != nil {
			return err
		}
		md, _ := sp.(broker.MarketDataProvider)
		est, err := broker.EstimateNotionalUSDWithFallback(ctx, e.cfg, md, symbol, o.Side, o.Amount, nil)
		if err != nil {
			return errors.New(unmeasurableOrderMessage(symbol, provider, err))
		}
		if est.USDKnown {
			usd = est.USD
		}
	}
	if !positiveFinite(usd) {
		return fmt.Errorf("the %s leg's USD value is unknown, so the per-order cap cannot be checked", o.Leg)
	}
	if err := broker.CheckRiskNotional(e.cfg, usd); err != nil {
		return fmt.Errorf("delta-neutral leg: %w", err)
	}
	if err := broker.GlobalLossTracker.CheckDailyLossFor(e.cfg.TradingRisk.DailyLossLimitUSD, broker.AccountKey(e.cfg, provider, account)); err != nil {
		return err
	}
	if o.Leg == deltaneutral.LegTypeFutures {
		if err := broker.CheckLeverage(e.cfg, "open delta-neutral futures leg", broker.RiskIncrease); err != nil {
			return err
		}
	}
	return nil
}

// Reserve takes the rate-limit tokens of BOTH legs up front, so a missing second
// token cannot strand a half-open position.
func (e *dnExchange) Reserve(p *deltaneutral.Plan) error {
	for _, id := range []string{p.FuturesProvider, p.SpotProvider} {
		if !broker.DefaultLimiter.Allow(id) {
			return fmt.Errorf("rate limit exceeded for provider %q: %w", id, broker.ErrRateLimited)
		}
	}
	return nil
}

func (e *dnExchange) SetLeverage(ctx context.Context, p *deltaneutral.Plan) error {
	fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
	if err != nil {
		return err
	}
	_, pos, err := futuresPositionSide(p.FuturesSide)
	if err != nil {
		return err
	}
	lev := p.FuturesLeverage
	if lev <= 0 {
		lev = 1
	}
	_, err = fp.SetFuturesLeverage(ctx, p.FuturesSymbol, int64(lev), p.FuturesMarginMode, pos)
	return err
}

// Place places the leg through broker.PlaceOrderIdempotent: futures through the
// futures provider (in paper mode only a reduce-only exit may, so an open is
// refused), spot through TradingProviderFor, which is a simulation in paper mode.
func (e *dnExchange) Place(ctx context.Context, p *deltaneutral.Plan, sz deltaneutral.Sizing, o deltaneutral.LegOrder, key string, attempt int, first time.Time) (deltaneutral.Order, error) {
	var outcome broker.PlaceOutcome
	opts := []broker.PlaceOption{broker.WithClock(e.now), broker.WithOutcome(func(oc broker.PlaceOutcome, _ string) { outcome = oc })}
	req := broker.OrderRequest{Type: "market", Side: o.Side, Amount: o.Amount}
	var target broker.Provider
	provider, account, symbol, unit := p.SpotProvider, p.SpotAccount, p.SpotSymbol, orderfeed.UnitBase
	if o.Leg == deltaneutral.LegTypeFutures {
		if broker.IsPaperMode(e.cfg) && !o.Exit { // only a reduce-only exit may reach the real exchange
			return deltaneutral.Order{}, fmt.Errorf("%w: futures orders have no simulation", broker.ErrPaperModeUnsupported)
		}
		fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
		if err != nil {
			return deltaneutral.Order{}, err
		}
		target = fp
		before, mode := sz.FuturesBefore, p.FuturesMarginMode
		if o.Exit {
			before = sz.FuturesHeld
		}
		if o.MarginMode != "" {
			mode = o.MarginMode
		}
		req.Market, req.Symbol, req.PositionSide, req.MarginMode, req.PositionBefore, req.ReduceOnly = broker.OrderMarketFutures, p.FuturesSymbol, o.PositionSide, mode, &before, o.Exit
		provider, account, symbol, unit = p.FuturesProvider, p.FuturesAccount, p.FuturesSymbol, orderfeed.UnitContracts
	} else {
		tp, _, err := e.spot(p)
		if err != nil {
			return deltaneutral.Order{}, err
		}
		target = tp
		req.Market, req.Symbol = broker.OrderMarketSpot, p.SpotSymbol
	}
	order, err := broker.PlaceOrderIdempotent(ctx, target, req, key, attempt, first, opts...)
	if err != nil {
		return deltaneutral.Order{}, err
	}
	// Reported once, by the attempt that placed it: finding an earlier attempt's order places nothing.
	if outcome == broker.OutcomePlaced || outcome == broker.OutcomePlacedAfterReconcile {
		if o.Exit && o.Leg == deltaneutral.LegTypeFutures { // C-4: the loss this reduce realised, prorated against the position
			filled := o.Amount
			if order.Filled != nil && *order.Filled > 0 {
				filled = *order.Filled
			}
			recordRealisedLoss(symbol, o.PnlBefore, filled, o.Amount, o.LivePosition, nil)
		}
		publishOrder(ctx, orderReport{
			tool: dnToolName(o.Op), provider: provider, account: account, symbol: symbol, side: o.Side,
			kind: "market", amount: o.Amount, unit: unit, order: order,
			notionalUSD: derefFloat(order.Cost), notionalKnown: order.Cost != nil && *order.Cost > 0 && usdQuoted(symbol, provider),
		})
	}
	return dnOrderOf(order, o), nil
}

// dnToolName is the order-feed name of the tool an operation belongs to.
func dnToolName(op string) string {
	switch op {
	case deltaneutral.OpResize:
		return NameResizeDeltaNeutralPosition
	case deltaneutral.OpUnwind:
		return NameUnwindDeltaNeutralPosition
	}
	return NameOpenDeltaNeutralPosition
}

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// dnOrderOf reduces a ccxt order to what the workflow journals. A spot buy's fee is
// in the base asset and a futures fee in USDT (see spotFeeCostUSDT).
func dnOrderOf(o ccxt.Order, lo deltaneutral.LegOrder) deltaneutral.Order {
	out := deltaneutral.Order{ID: derefStr(o.Id), Status: strings.ToLower(derefStr(o.Status))}
	if o.Amount != nil && positiveFinite(*o.Amount) {
		out.Amount = *o.Amount
	}
	if o.Filled != nil && positiveOrZero(*o.Filled) {
		out.Filled, out.FilledKnown = *o.Filled, true
	}
	if o.Average != nil && positiveFinite(*o.Average) {
		out.Avg = *o.Average
	}
	if o.Cost != nil && positiveFinite(*o.Cost) {
		out.Cost = *o.Cost
	}
	if lo.Leg == deltaneutral.LegTypeFutures {
		if o.Fee.Cost != nil && *o.Fee.Cost > 0 {
			out.FeeUSDT = *o.Fee.Cost
		}
	} else {
		out.FeeUSDT = spotFeeCostUSDT(o, lo.Side, out.Avg)
	}
	switch out.Status {
	case "closed", "filled", "canceled", "cancelled", "expired", "rejected":
		out.Done = true
	}
	return out
}

func (e *dnExchange) legOf(p *deltaneutral.Plan, leg deltaneutral.LegType) deltaneutral.LegOrder {
	lo, _ := e.LegOrder(p, leg, 0)
	return lo
}

// Fetch reads an order back.
func (e *dnExchange) Fetch(ctx context.Context, p *deltaneutral.Plan, leg deltaneutral.LegType, id string) (deltaneutral.Order, error) {
	var (
		o   ccxt.Order
		err error
	)
	if leg == deltaneutral.LegTypeFutures {
		fp, ferr := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
		if ferr != nil {
			return deltaneutral.Order{}, ferr
		}
		o, err = fp.FetchFuturesOrder(ctx, id, p.FuturesSymbol)
	} else {
		tp, _, terr := e.spot(p)
		if terr != nil {
			return deltaneutral.Order{}, terr
		}
		o, err = tp.FetchOrder(ctx, id, p.SpotSymbol)
	}
	if err != nil {
		return deltaneutral.Order{}, err
	}
	return dnOrderOf(o, e.legOf(p, leg)), nil
}

// Cancel withdraws an order that is still open.
func (e *dnExchange) Cancel(ctx context.Context, p *deltaneutral.Plan, leg deltaneutral.LegType, id string) error {
	if leg == deltaneutral.LegTypeFutures {
		fp, err := futuresProvider(ctx, e.cfg, p.FuturesProvider, p.FuturesAccount)
		if err != nil {
			return err
		}
		_, err = fp.CancelFuturesOrder(ctx, id, p.FuturesSymbol)
		return err
	}
	tp, _, err := e.spot(p)
	if err != nil {
		return err
	}
	_, err = tp.CancelOrder(ctx, id, p.SpotSymbol)
	return err
}

// ---- the approval side of dn.recover ----

// ApprovalCloser lets dn.recover propose a close through the approval service.
// It is the whole of dn.recover's reach into orders: the proposal carries a
// reduce-only order for futures, and the approval service alone executes it.
type ApprovalCloser struct{ svc *approval.Service }

// NewApprovalCloser wraps the approval service.
func NewApprovalCloser(svc *approval.Service) *ApprovalCloser { return &ApprovalCloser{svc: svc} }

var _ deltaneutral.CloseProposer = (*ApprovalCloser)(nil)

// ProposeClose creates the proposal. It is a risk-reducing proposal from a
// background process, made on the strength of a candidate attention decided to
// deliver now, from facts read off the exchange by code.
func (a *ApprovalCloser) ProposeClose(ctx context.Context, r deltaneutral.CloseRequest) (deltaneutral.Proposed, error) {
	req := approval.ProposeRequest{
		Order: approval.OrderRequest{
			Provider: r.Provider, Account: r.Account, Market: r.Market, Symbol: r.Symbol, Side: r.Side,
			Type: "market", ReduceOnly: r.ReduceOnly,
		},
		Rationale: r.Rationale, Origin: approval.OriginProactive, Trust: proactive.TrustSystem,
		DeliverNow: r.Candidate.DeliverNow, CandidateID: r.Candidate.CandidateID,
	}
	if r.Market == approval.MarketFutures {
		req.Order.Contracts = r.Amount
	} else {
		req.Order.Amount = r.Amount
	}
	p, err := a.svc.Propose(ctx, req)
	if err != nil {
		return deltaneutral.Proposed{}, err
	}
	return deltaneutral.Proposed{ID: p.ID, Code: p.Code, ExpiresAt: p.ExpiresAt}, nil
}

// Status is the proposal's status as the approval service names it.
func (a *ApprovalCloser) Status(ctx context.Context, id string) (string, error) {
	p, err := a.svc.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return string(p.Status), nil
}

// WithdrawProposals cancels the still-pending recovery proposals among ids because an
// unwind closed their plan, through the approval service's narrow cancel (it edits each
// card once). A proposal already approved is not touched: it is reported as still
// executing, so the owner knows that exit is theirs. Both lists come from the status
// read afterwards, so a repeated call (finalize ran again) reports the same.
func (a *ApprovalCloser) WithdrawProposals(ctx context.Context, ids []string) (deltaneutral.Withdrawal, error) {
	var w deltaneutral.Withdrawal
	_, err := a.svc.CancelForClosedPlan(ctx, ids)
	for _, id := range ids {
		p, gerr := a.svc.Get(ctx, id)
		if gerr != nil {
			err = errors.Join(err, gerr)
			continue
		}
		switch {
		case p.Status == approval.StatusCancelled && p.Outcome == approval.ReasonPlanClosed:
			w.Cancelled = append(w.Cancelled, p.Code)
		case p.Status == approval.StatusApproved || p.Status == approval.StatusExecuting:
			w.StillExecuting = append(w.StillExecuting, p.Code)
		}
	}
	return w, err
}
