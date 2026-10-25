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
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ApprovalOrderBackend is the approval package's view of the exchanges: the order
// gates, the pricing and the placing, built from the same helpers create_order and
// the futures tools use, so a proposal is held to exactly the rules a direct order
// is. It lives here, not in pkg/approval, because those helpers are this
// package's; pkg/approval depends on the interface only.
//
// Every order that follows from a proposal is placed by PlaceApprovedOrder, which
// is the only caller of broker.PlaceOrderIdempotent that follows from a proposal (the
// delta-neutral open, dnExchange.Place, is the other caller in the program, and is not
// reached from a proposal; TestOrderExecutionCallGraph keeps both lists exact).
type ApprovalOrderBackend struct {
	cfg *config.Config
	now func() time.Time
}

var _ approval.OrderBackend = (*ApprovalOrderBackend)(nil)

// NewApprovalOrderBackend builds the backend over cfg. A config reload builds a
// new one (the approval service is rebuilt with it).
func NewApprovalOrderBackend(cfg *config.Config) *ApprovalOrderBackend {
	return &ApprovalOrderBackend{cfg: cfg, now: time.Now}
}

// orderSpec is an order as the gates see it: after validation, with the amount
// already in the unit the exchange takes.
type orderSpec struct {
	provider, account, market, symbol, side, orderType string
	amount                                             float64
	price                                              *float64
	reduceOnly                                         bool
}

// evaluation is what the gates learnt about an order.
type evaluation struct {
	ref, notionalUSD         float64
	reducing                 bool
	mode                     string
	positionSide, marginMode string
	pnl                      *float64
	positionContracts        float64
}

func gateErr(gate string, err error) error {
	return fmt.Errorf("gate %s refused the order: %w", gate, err)
}

func positiveFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }

// Prepare implements approval.OrderBackend: validate, price and run the dry gates.
func (b *ApprovalOrderBackend) Prepare(ctx context.Context, req approval.OrderRequest) (approval.Prepared, error) {
	spec := orderSpec{
		provider: strings.TrimSpace(req.Provider), account: strings.TrimSpace(req.Account),
		market:     strings.ToLower(strings.TrimSpace(req.Market)),
		symbol:     strings.TrimSpace(req.Symbol),
		side:       strings.ToLower(strings.TrimSpace(req.Side)),
		orderType:  strings.ToLower(strings.TrimSpace(req.Type)),
		price:      req.Price,
		reduceOnly: req.ReduceOnly,
	}
	if spec.market == "" {
		spec.market = approval.MarketSpot
	}
	if spec.orderType == "" {
		spec.orderType = "market"
	}
	collar := approval.DefaultCollarPct
	if req.CollarPct != 0 {
		collar = req.CollarPct
	}
	switch {
	case spec.provider == "" || spec.symbol == "":
		return approval.Prepared{}, errors.New("provider and symbol are required")
	case spec.market != approval.MarketSpot && spec.market != approval.MarketFutures:
		return approval.Prepared{}, errors.New("market must be spot or futures")
	case spec.side != "buy" && spec.side != "sell":
		return approval.Prepared{}, errors.New("side must be buy or sell")
	case spec.orderType != "market" && spec.orderType != "limit":
		return approval.Prepared{}, errors.New("type must be market or limit")
	case spec.orderType == "limit" && (spec.price == nil || !positiveFinite(*spec.price)):
		return approval.Prepared{}, errors.New("a limit order needs a positive, finite price")
	case spec.orderType == "market" && spec.price != nil:
		return approval.Prepared{}, errors.New("a market order carries no price")
	case !positiveFinite(collar) || collar > 50:
		return approval.Prepared{}, errors.New("the price collar must be a positive percentage of at most 50")
	}

	switch spec.market {
	case approval.MarketSpot:
		if strings.Contains(spec.symbol, ":") {
			return approval.Prepared{}, errors.New(approval.ErrSpotContractSymbol)
		}
		if !positiveFinite(req.Amount) {
			return approval.Prepared{}, errors.New("amount must be a positive, finite number of base units")
		}
		if spec.reduceOnly {
			return approval.Prepared{}, errors.New("reduce_only applies to futures orders only")
		}
		spec.amount = req.Amount
	default:
		spec.symbol = normalizeFuturesSymbol(spec.symbol)
		if req.Contracts != 0 {
			// An exact count, from code, and only for an order that cannot add risk:
			// the reduce-only gate then checks it against the position held.
			if !spec.reduceOnly || !positiveFinite(req.Contracts) {
				return approval.Prepared{}, errors.New("an exact contract count is for a reduce-only order, and must be positive and finite")
			}
			spec.amount = req.Contracts
			break
		}
		// A futures amount is a CONTRACT count, computed by code from a USD size
		// and the market's contract size; the model's number is only the size.
		if !positiveFinite(req.NotionalUSD) {
			return approval.Prepared{}, errors.New("a futures proposal needs notional_usd (the order's size in USD); the contract count is computed from it")
		}
		contracts, err := b.contractsFor(ctx, spec, req.NotionalUSD)
		if err != nil {
			return approval.Prepared{}, err
		}
		spec.amount = contracts
	}

	ev, err := b.gate(ctx, spec)
	if err != nil {
		return approval.Prepared{}, err
	}
	params := approval.OrderParams{
		Provider: spec.provider, Account: spec.account, Market: spec.market, Symbol: spec.symbol,
		Side: spec.side, Type: spec.orderType, Amount: spec.amount, Price: spec.price,
		ReduceOnly: spec.reduceOnly, PositionSide: ev.positionSide, MarginMode: ev.marginMode,
		RefPrice: ev.ref, NotionalUSD: ev.notionalUSD, PriceCollarPct: collar,
	}
	return approval.Prepared{Params: params, Reducing: ev.reducing, Mode: ev.mode}, nil
}

// contractsFor converts a USD size into a contract count with the shared helpers.
func (b *ApprovalOrderBackend) contractsFor(ctx context.Context, spec orderSpec, notionalUSD float64) (float64, error) {
	if !spec.reduceOnly && broker.IsPaperMode(b.cfg) {
		return 0, gateErr("paper mode", fmt.Errorf("%w: futures orders have no simulation", broker.ErrPaperModeUnsupported))
	}
	fp, err := futuresProvider(ctx, b.cfg, spec.provider, spec.account)
	if err != nil {
		return 0, err
	}
	mkt, err := validateActiveSwapMarket(ctx, fp, spec.symbol, 0)
	if err != nil {
		return 0, err
	}
	mark, err := fp.FetchFuturesMarkPrice(ctx, spec.symbol)
	if err != nil || !positiveFinite(mark) {
		return 0, fmt.Errorf("cannot resolve the mark price of %s to compute a contract count: %v", spec.symbol, err)
	}
	contractSize, minAmount := futuresContractSize(mkt), 1.0
	if mkt.Limits.Amount.Min != nil && *mkt.Limits.Amount.Min > 0 {
		minAmount = *mkt.Limits.Amount.Min
	}
	contracts, err := contractsFromNotional(notionalUSD, mark, contractSize, minAmount)
	if err != nil {
		return 0, fmt.Errorf("contract computation failed: %w", err)
	}
	if !positiveFinite(contracts) {
		return 0, fmt.Errorf("a size of %.2f USD is below one tradable step of %s", notionalUSD, spec.symbol)
	}
	return contracts, nil
}

// gate runs the order rules of this repository, in create_order's order, without
// the rate limiter: permission, order type, risk notional (in USD, an unknown USD
// value refuses), the per-account daily-loss check for a risk-increasing order,
// leverage for futures. It is the dry half of every proposal and runs again, on
// the stored params, before the order is placed.
func (b *ApprovalOrderBackend) gate(ctx context.Context, spec orderSpec) (evaluation, error) {
	if err := broker.CheckPermission(b.cfg, spec.provider, spec.account, config.ScopeTrade); err != nil {
		return evaluation{}, gateErr("permission", err)
	}
	if err := broker.CheckOrderType(b.cfg, spec.orderType); err != nil {
		return evaluation{}, gateErr("order type", err)
	}
	if spec.market == approval.MarketFutures {
		return b.gateFutures(ctx, spec)
	}
	return b.gateSpot(ctx, spec)
}

func (b *ApprovalOrderBackend) gateSpot(ctx context.Context, spec orderSpec) (evaluation, error) {
	// Defence in depth (Prepare and the canonical params refuse it first, and a
	// stored proposal reaches this gate again at Recheck): the spot path has no
	// leverage check and values an order as amount x price, and the exchange
	// library would resolve a contract symbol to the perpetual.
	if strings.Contains(spec.symbol, ":") {
		return evaluation{}, gateErr("market", errors.New(approval.ErrSpotContractSymbol))
	}
	_, p, err := broker.TradingProviderFor(b.cfg, spec.provider, spec.account)
	if err != nil {
		if errors.Is(err, broker.ErrOrderExecutionUnsupported) {
			return evaluation{}, fmt.Errorf("provider %q does not support order execution", spec.provider)
		}
		return evaluation{}, fmt.Errorf("provider %q: %w", spec.provider, err)
	}
	if status, err := p.GetMarketStatus(ctx, spec.symbol); err == nil && status == broker.MarketClosed {
		return evaluation{}, gateErr("market status", fmt.Errorf("market %s is currently closed", spec.symbol))
	}
	md, _ := p.(broker.MarketDataProvider)

	// The USD value of the order is required: an unknown value refuses (a cap or
	// not), and the cap is checked in USD, never as amount*price in the quote
	// currency.
	est, err := broker.EstimateNotionalUSDWithFallback(ctx, b.cfg, md, spec.symbol, spec.side, spec.amount, spec.price)
	if err != nil {
		return evaluation{}, gateErr("risk", errors.New(unmeasurableOrderMessage(spec.symbol, spec.provider, err)))
	}
	if !est.USDKnown || !positiveFinite(est.USD) {
		return evaluation{}, gateErr("risk", errors.New("the order's USD value is unknown"))
	}
	ref := est.Price
	if spec.price != nil { // a limit order's reference is the market, not its own limit
		q, err := broker.EstimateNotionalQuote(ctx, md, spec.symbol, spec.side, 1, nil)
		if err != nil {
			return evaluation{}, gateErr("risk", err)
		}
		ref = q.Price
	}
	if err := broker.CheckRiskNotional(b.cfg, est.USD); err != nil {
		return evaluation{}, gateErr("risk", err)
	}

	// Risk direction: the shared classifier, then for a spot sell the holding
	// check. What cannot be shown to stay within what the account holds is
	// risk-increasing.
	reducing := !orderAddsRisk(spec.symbol, spec.side, nil, false)
	if reducing && spec.side == "sell" {
		reducing = holdsAtLeast(ctx, p, spec.symbol, spec.amount)
	}
	if !reducing {
		if err := broker.GlobalLossTracker.CheckDailyLossFor(b.cfg.TradingRisk.DailyLossLimitUSD, broker.AccountKey(b.cfg, spec.provider, spec.account)); err != nil {
			return evaluation{}, gateErr("daily loss", err)
		}
	}
	if spec.side == "buy" && est.Quote != "" && strings.Contains(spec.symbol, "/") {
		if pp, ok := p.(broker.PortfolioProvider); ok {
			if bals, err := pp.GetBalances(ctx); err == nil {
				var free float64
				for _, bal := range bals {
					if strings.EqualFold(bal.Asset, est.Quote) {
						free = bal.Free
						break
					}
				}
				if free < est.QuoteNotional {
					return evaluation{}, gateErr("balance", fmt.Errorf("insufficient %s balance: have %.8g, need %.8g: %w", est.Quote, free, est.QuoteNotional, broker.ErrInsufficientFunds))
				}
			}
		}
	}
	mode := approval.ModeLive
	if broker.IsPaperMode(b.cfg) {
		mode = approval.ModePaper
	}
	return evaluation{ref: ref, notionalUSD: est.USD, reducing: reducing, mode: mode}, nil
}

// holdsAtLeast reports whether the account holds at least amount of the symbol's
// base asset. Any doubt (no balance API, a failed call) is false.
func holdsAtLeast(ctx context.Context, p broker.Provider, symbol string, amount float64) bool {
	pp, ok := p.(broker.PortfolioProvider)
	if !ok {
		return false
	}
	bals, err := pp.GetBalances(ctx)
	if err != nil {
		return false
	}
	base := symbol
	if i := strings.Index(symbol, "/"); i > 0 {
		base = symbol[:i]
	}
	for _, bal := range bals {
		if strings.EqualFold(bal.Asset, base) {
			return bal.Free >= amount
		}
	}
	return false
}

func (b *ApprovalOrderBackend) gateFutures(ctx context.Context, spec orderSpec) (evaluation, error) {
	paper := broker.IsPaperMode(b.cfg)
	if paper && !spec.reduceOnly {
		// No simulation exists for futures: only an exit is allowed in paper mode,
		// and it is a REAL exit, as futures_close_position is.
		return evaluation{}, gateErr("paper mode", fmt.Errorf("%w: futures orders have no simulation", broker.ErrPaperModeUnsupported))
	}
	dir := broker.RiskIncrease
	if spec.reduceOnly {
		dir = broker.RiskReduce
	}
	if err := broker.CheckLeverage(b.cfg, "propose a futures order", dir); err != nil {
		return evaluation{}, gateErr("leverage", err)
	}
	fp, err := futuresProvider(ctx, b.cfg, spec.provider, spec.account)
	if err != nil {
		return evaluation{}, err
	}
	mkt, err := validateActiveSwapMarket(ctx, fp, spec.symbol, 0)
	if err != nil {
		return evaluation{}, err
	}
	if !exchanges.USDLike(broker.SymbolQuote(spec.symbol, spec.provider)) {
		return evaluation{}, gateErr("risk", fmt.Errorf("%s is not quoted in a USD-like currency, so its USD value cannot be established", spec.symbol))
	}
	mark, err := fp.FetchFuturesMarkPrice(ctx, spec.symbol)
	if err != nil || !positiveFinite(mark) {
		return evaluation{}, gateErr("risk", fmt.Errorf("cannot resolve the mark price of %s: %v", spec.symbol, err))
	}
	contractSize := futuresContractSize(mkt)
	usdValue := positionNotionalUSD(spec.amount, contractSize, mark)
	if !positiveFinite(usdValue) {
		return evaluation{}, gateErr("risk", errors.New("the order's USD value is unknown"))
	}
	if err := broker.CheckRiskNotional(b.cfg, usdValue); err != nil {
		return evaluation{}, gateErr("risk", err)
	}

	ev := evaluation{ref: mark, notionalUSD: usdValue, mode: approval.ModeLive}
	if paper {
		ev.mode = approval.ModeRealExit
	}
	if !spec.reduceOnly {
		_, posSide, perr := futuresPositionSide(spec.side)
		if perr != nil {
			return evaluation{}, perr
		}
		ev.positionSide, ev.marginMode = posSide, marginModeOrDefault("")
		if err := broker.GlobalLossTracker.CheckDailyLossFor(b.cfg.TradingRisk.DailyLossLimitUSD, broker.AccountKey(b.cfg, spec.provider, spec.account)); err != nil {
			return evaluation{}, gateErr("daily loss", err)
		}
		return ev, nil
	}

	// A reduce-only order must close a position that exists, on its closing side,
	// and not by more than it holds.
	positions, err := fp.FetchFuturesPositions(ctx, []string{spec.symbol})
	if err != nil {
		return evaluation{}, gateErr("position", fmt.Errorf("cannot read the open position: %w", err))
	}
	for _, pos := range positions {
		if pos.Contracts == nil || *pos.Contracts == 0 || pos.Symbol == nil || normalizeFuturesSymbol(*pos.Symbol) != spec.symbol {
			continue
		}
		side := "long"
		if pos.Side != nil {
			side = strings.ToLower(*pos.Side)
		}
		if futuresCloseSide(side) != spec.side {
			continue
		}
		held := math.Abs(*pos.Contracts)
		if spec.amount > held*(1+1e-9) {
			return evaluation{}, gateErr("position", fmt.Errorf("the order (%.8g contracts) is larger than the open position (%.8g)", spec.amount, held))
		}
		ev.reducing = true
		ev.positionSide, ev.marginMode = side, marginModeOrDefault("")
		if pos.MarginMode != nil {
			ev.marginMode = marginModeOrDefault(*pos.MarginMode)
		}
		ev.pnl, ev.positionContracts = pos.UnrealizedPnl, held
		return ev, nil
	}
	return evaluation{}, gateErr("position", fmt.Errorf("no open %s position to reduce with a %s order", spec.symbol, spec.side))
}

// Recheck implements approval.OrderBackend: the gates again on the stored params,
// then the price collar against a fresh price.
//
// The collar applies to a RISK-INCREASING order only, and is ADVERSE-ONLY there: a
// buy (or any order that opens or enlarges a position) is abandoned when the price
// moved against it by more than the collar, a move in its favour never aborts it.
// An order that only reduces risk (ev.reducing, from the shared classifier) is
// never abandoned for a price move: a safety brake must not block getting out of a
// position, and an owner who approved an exit in a falling market wants out. It
// still has every gate that applies to it re-run, its USD value recomputed (no
// usable price means no USD value, which refuses), and the approval's age bounded
// by the service. A limit exit keeps its limit price, so it cannot fill worse.
func (b *ApprovalOrderBackend) Recheck(ctx context.Context, op approval.OrderParams) (approval.Rechecked, error) {
	ev, err := b.gate(ctx, orderSpec{
		provider: op.Provider, account: op.Account, market: op.Market, symbol: op.Symbol, side: op.Side,
		orderType: op.Type, amount: op.Amount, price: op.Price, reduceOnly: op.ReduceOnly,
	})
	if err != nil {
		return approval.Rechecked{}, err
	}
	if adverse := adverseMovePct(op.Side, op.RefPrice, ev.ref); !ev.reducing && adverse > op.PriceCollarPct {
		return approval.Rechecked{}, gateErr("price collar", fmt.Errorf(
			"the price moved %.2f%% against the order (%.8g -> %.8g, collar %.2f%%, adverse direction only); nothing was placed",
			adverse, op.RefPrice, ev.ref, op.PriceCollarPct))
	}
	return approval.Rechecked{
		Reducing: ev.reducing, Mode: ev.mode, NotionalUSD: ev.notionalUSD, LastPrice: ev.ref,
		PnLBefore: ev.pnl, PositionContracts: ev.positionContracts,
	}, nil
}

// adverseMovePct is how far, in percent of ref, the price has moved against a
// side: up for a buy, down for a sell. Zero when it moved in the order's favour.
func adverseMovePct(side string, ref, now float64) float64 {
	if !positiveFinite(ref) || !positiveFinite(now) {
		return math.Inf(1) // no usable price is not a price inside the collar
	}
	move := (now - ref) / ref * 100
	if side == "sell" {
		move = -move
	}
	return math.Max(move, 0)
}

// Throttle implements approval.OrderBackend. A simulated spot order never reaches
// an exchange and spends no token (create_order does the same); a real order,
// including a real exit while paper mode is on, spends one.
func (b *ApprovalOrderBackend) Throttle(p approval.OrderParams) (bool, time.Duration) {
	if p.Market == approval.MarketSpot && broker.IsPaperMode(b.cfg) {
		return true, 0
	}
	if broker.DefaultLimiter.Allow(p.Provider) {
		return true, 0
	}
	wait := time.Minute
	if st, ok := broker.DefaultLimiter.Status()[p.Provider]; ok && st.ResetInterval > 0 {
		wait = st.ResetInterval
	}
	return false, wait
}

// PlaceApprovedOrder implements approval.OrderBackend. It is the only caller of
// broker.PlaceOrderIdempotent that an approved proposal reaches: spot orders go through
// TradingProviderFor (a simulation in paper mode, in which a reconcile then looks
// in that same provider), futures orders through the real futures provider, which
// paper mode allows only for a reduce-only exit.
func (b *ApprovalOrderBackend) PlaceApprovedOrder(ctx context.Context, p approval.OrderParams, key string, attempt int, first time.Time, rc approval.Rechecked) (approval.Placed, error) {
	var outcome broker.PlaceOutcome
	var cid string
	opts := []broker.PlaceOption{broker.WithClock(b.now), broker.WithOutcome(func(o broker.PlaceOutcome, id string) { outcome, cid = o, id })}

	req := broker.OrderRequest{Market: p.Market, Symbol: p.Symbol, Type: p.Type, Side: p.Side, Amount: p.Amount, Price: p.Price}
	var target broker.Provider
	switch p.Market {
	case approval.MarketSpot:
		tp, _, err := broker.TradingProviderFor(b.cfg, p.Provider, p.Account)
		if err != nil {
			return approval.Placed{}, err
		}
		target = tp
	case approval.MarketFutures:
		if broker.IsPaperMode(b.cfg) && !p.ReduceOnly {
			return approval.Placed{}, fmt.Errorf("%w: futures orders have no simulation", broker.ErrPaperModeUnsupported)
		}
		fp, err := futuresProvider(ctx, b.cfg, p.Provider, p.Account)
		if err != nil {
			return approval.Placed{}, err
		}
		target = fp
		req.ReduceOnly, req.MarginMode, req.PositionSide = p.ReduceOnly, p.MarginMode, p.PositionSide
		if rc.PositionContracts > 0 {
			held := rc.PositionContracts
			req.PositionBefore = &held
		}
	default:
		return approval.Placed{}, fmt.Errorf("unknown market %q", p.Market)
	}

	order, err := broker.PlaceOrderIdempotent(ctx, target, req, key, attempt, first, opts...)
	if err != nil {
		return approval.Placed{}, err
	}
	return approval.Placed{
		OrderID: derefStr(order.Id), ClientOrderID: cid, Status: derefStr(order.Status), Outcome: string(outcome),
		Fill: fillOf(order),
	}, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fillOf reads what an order says about its own fill.
func fillOf(o ccxt.Order) approval.Fill {
	f := approval.Fill{Status: strings.ToLower(derefStr(o.Status))}
	if o.Filled != nil && positiveOrZero(*o.Filled) {
		f.Known, f.Filled = true, *o.Filled
	}
	if o.Average != nil && positiveFinite(*o.Average) {
		f.AvgPrice = *o.Average
	}
	if o.Fee.Cost != nil && positiveFinite(*o.Fee.Cost) {
		f.FeeCost, f.FeeCcy = *o.Fee.Cost, derefStr(o.Fee.Currency)
	}
	switch f.Status {
	case "closed", "filled", "canceled", "cancelled", "expired", "rejected":
		f.Done = true
	}
	return f
}

func positiveOrZero(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// errNoFetchInPaper: a simulated order lives only in the answer to its placement.
var errNoFetchInPaper = errors.New("a simulated order cannot be fetched back")

// Fetch implements approval.OrderBackend.
func (b *ApprovalOrderBackend) Fetch(ctx context.Context, p approval.OrderParams, id string) (approval.Fill, error) {
	var (
		o   ccxt.Order
		err error
	)
	if p.Market == approval.MarketFutures {
		fp, ferr := futuresProvider(ctx, b.cfg, p.Provider, p.Account)
		if ferr != nil {
			return approval.Fill{}, ferr
		}
		o, err = fp.FetchFuturesOrder(ctx, id, p.Symbol)
	} else {
		if broker.IsPaperMode(b.cfg) {
			return approval.Fill{}, errNoFetchInPaper
		}
		tp, _, terr := broker.TradingProviderFor(b.cfg, p.Provider, p.Account)
		if terr != nil {
			return approval.Fill{}, terr
		}
		o, err = tp.FetchOrder(ctx, id, p.Symbol)
	}
	if err != nil {
		return approval.Fill{}, err
	}
	return fillOf(o), nil
}

// Record implements approval.OrderBackend: the order feed, and for a futures exit
// the loss it realised against the daily limit (prorated by the fill, as
// futures_close_position does). A spot sell records no loss, as create_order does:
// no cost basis is known.
func (b *ApprovalOrderBackend) Record(ctx context.Context, p approval.OrderParams, rc approval.Rechecked, placed approval.Placed, fill approval.Fill) {
	unit := orderfeed.UnitBase
	if p.Market == approval.MarketFutures {
		unit = orderfeed.UnitContracts
	}
	id := placed.OrderID
	publishOrder(ctx, orderReport{
		tool: NameProposeOrder, provider: p.Provider, account: p.Account, symbol: p.Symbol, side: p.Side, kind: p.Type,
		amount: p.Amount, unit: unit, order: ccxt.Order{Id: &id},
		status: fill.Status, filled: fill.Filled, filledKnown: fill.Known,
		notionalUSD: p.NotionalUSD, notionalKnown: true, paper: rc.Mode == approval.ModePaper,
	})
	if p.Market == approval.MarketFutures && p.ReduceOnly {
		var verifyErr error
		if !fill.Known {
			verifyErr = errors.New("the fill could not be confirmed")
		}
		recordRealisedLoss(p.Symbol, rc.PnLBefore, fill.Filled, p.Amount, rc.PositionContracts, verifyErr)
	}
}
