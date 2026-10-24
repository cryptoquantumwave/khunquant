package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dcaExchange implements dca.Exchange over the order helpers and gates of this
// package, the ones execute_dca_order, create_order and the approval backend use.
// Every order it places goes through broker.TradingProviderFor (a simulation in
// paper mode, in which a reconcile looks in that same provider).
type dcaExchange struct {
	cfg   *config.Config
	store *dca.Store
	now   func() time.Time
}

var _ dca.Exchange = (*dcaExchange)(nil)

// NewDCAExchange builds the exchange the dca.execute workflow runs on. now is the
// engine's clock (the reconcile's settle delay is measured on it).
func NewDCAExchange(cfg *config.Config, store *dca.Store, now func() time.Time) dca.Exchange {
	if now == nil {
		now = time.Now
	}
	return &dcaExchange{cfg: cfg, store: store, now: now}
}

func (e *dcaExchange) market(p *dca.Plan) (broker.MarketDataProvider, error) {
	_, real, err := broker.TradingProviderFor(e.cfg, p.Provider, p.Account)
	if err != nil {
		if errors.Is(err, broker.ErrOrderExecutionUnsupported) {
			return nil, fmt.Errorf("provider %q does not support order execution", p.Provider)
		}
		return nil, err
	}
	md, ok := real.(broker.MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support market data", p.Provider)
	}
	return md, nil
}

// Quote prices the run: the base amount from the plan's amount and the last price,
// then its value in USD through EstimateNotionalUSDWithFallback (an unknown value is
// an error, which refuses the run).
func (e *dcaExchange) Quote(ctx context.Context, p *dca.Plan) (dca.Quote, error) {
	md, err := e.market(p)
	if err != nil {
		return dca.Quote{}, err
	}
	tk, err := md.FetchTicker(ctx, p.Symbol)
	if err != nil {
		return dca.Quote{}, fmt.Errorf("ticker of %s: %w", p.Symbol, err)
	}
	if tk.Last == nil || !positiveFinite(*tk.Last) {
		return dca.Quote{}, fmt.Errorf("ticker of %s has no usable last price", p.Symbol)
	}
	base := p.AmountPerOrder
	if p.AmountUnit != "base" {
		base = p.AmountPerOrder / *tk.Last
	}
	if !positiveFinite(base) {
		return dca.Quote{}, fmt.Errorf("invalid order amount %v", base)
	}
	est, err := broker.EstimateNotionalUSDWithFallback(ctx, e.cfg, md, p.Symbol, "buy", base, nil)
	if err != nil {
		return dca.Quote{}, errors.New(unmeasurableOrderMessage(p.Symbol, p.Provider, err))
	}
	if !est.USDKnown || !positiveFinite(est.USD) || !positiveFinite(est.USDPerQuote) {
		return dca.Quote{}, errors.New("the USD value of the run is unknown")
	}
	src := est.RateSource
	if src == "" {
		src = est.Source
	}
	return dca.Quote{
		Price: *tk.Last, BaseAmount: base, QuoteAmount: est.QuoteNotional, QuoteCcy: est.Quote,
		USD: est.USD, USDPerQuote: est.USDPerQuote, RateSource: src, Paper: broker.IsPaperMode(e.cfg),
	}, nil
}

// Gates runs the gates of execute_dca_order for a buy, in its order, and returns the
// run valued again now (the workflow re-checks the mandate's caps against it).
func (e *dcaExchange) Gates(ctx context.Context, p *dca.Plan, q dca.Quote) (dca.Quote, error) {
	if err := broker.CheckPermission(e.cfg, p.Provider, p.Account, config.ScopeTrade); err != nil {
		return q, fmt.Errorf("permission check failed: %w", err)
	}
	if p.ExecPeriod != "" && p.MaxExecPerPeriod > 0 {
		n, err := e.store.CountExecutionsInPeriodAt(ctx, p.ID, p.ExecPeriod, e.now(), e.cfg.TradingRisk.PaperTradingMode)
		if err != nil {
			return q, fmt.Errorf("guardrail check failed: %w", err)
		}
		if n >= p.MaxExecPerPeriod {
			return q, fmt.Errorf("period cap reached: %d/%d executions this %s", n, p.MaxExecPerPeriod, p.ExecPeriod)
		}
	}
	if err := broker.CheckOrderType(e.cfg, "market"); err != nil {
		return q, err
	}
	md, err := e.market(p)
	if err != nil {
		return q, err
	}
	// The caps are in USD; the order is valued in USD again now, and an order whose
	// USD value cannot be determined is refused, whether or not a cap is set.
	est, err := broker.EstimateNotionalUSDWithFallback(ctx, e.cfg, md, p.Symbol, "buy", q.BaseAmount, nil)
	if err != nil {
		return q, errors.New(unmeasurableOrderMessage(p.Symbol, p.Provider, err))
	}
	if !est.USDKnown || !positiveFinite(est.USD) {
		return q, errors.New("the order's USD value is unknown")
	}
	if maxDCA := e.cfg.TradingRisk.MaxDCAAmountPerOrderUSD; maxDCA > 0 && est.USD > maxDCA {
		return q, fmt.Errorf("DCA order notional %.2f USD exceeds configured max_dca_amount_per_order_usd %.2f", est.USD, maxDCA)
	}
	if err := broker.CheckRiskNotional(e.cfg, est.USD); err != nil {
		return q, err
	}
	// A buy is risk-increasing: the per-account daily-loss check, failing closed.
	if err := broker.GlobalLossTracker.CheckDailyLossFor(e.cfg.TradingRisk.DailyLossLimitUSD, broker.AccountKey(e.cfg, p.Provider, p.Account)); err != nil {
		return q, err
	}
	q.USD, q.USDPerQuote, q.QuoteAmount = est.USD, est.USDPerQuote, est.QuoteNotional
	return q, nil
}

// Throttle takes the provider's rate-limit token. A simulated order never reaches an
// exchange and spends none (create_order and the approval backend do the same).
func (e *dcaExchange) Throttle(p *dca.Plan) error {
	if broker.IsPaperMode(e.cfg) {
		return nil
	}
	if !broker.DefaultLimiter.Allow(p.Provider) {
		return fmt.Errorf("%w for provider %q", dca.ErrRateLimited, p.Provider)
	}
	return nil
}

// PlaceMandateBuy is the one place a mandate run reaches an exchange: a market buy
// of the journaled base amount through broker.PlaceOrderIdempotent.
func (e *dcaExchange) PlaceMandateBuy(ctx context.Context, p *dca.Plan, q dca.Quote, key string, attempt int, first time.Time) (dca.Fill, error) {
	tp, _, err := broker.TradingProviderFor(e.cfg, p.Provider, p.Account)
	if err != nil {
		return dca.Fill{}, err
	}
	var outcome broker.PlaceOutcome
	opts := []broker.PlaceOption{broker.WithClock(e.now), broker.WithOutcome(func(o broker.PlaceOutcome, _ string) { outcome = o })}
	req := broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: p.Symbol, Type: "market", Side: "buy", Amount: q.BaseAmount}
	order, err := broker.PlaceOrderIdempotent(ctx, tp, req, key, attempt, first, opts...)
	if err != nil {
		return dca.Fill{}, err
	}
	paper := broker.IsPaperMode(e.cfg)
	if outcome == broker.OutcomePlaced || outcome == broker.OutcomePlacedAfterReconcile {
		publishOrder(ctx, orderReport{
			tool: NameExecuteDCAOrder, provider: p.Provider, account: p.Account, symbol: p.Symbol, side: "buy",
			kind: "market", amount: q.BaseAmount, unit: orderfeed.UnitBase, order: order,
			notionalUSD: q.USD, notionalKnown: true, paper: paper,
		})
	}
	f := dcaFillOf(order)
	f.Paper = paper
	if paper {
		f.Done = true // a simulated order is complete in its answer and cannot be fetched
	}
	return f, nil
}

func dcaFillOf(o ccxt.Order) dca.Fill {
	f := dca.Fill{OrderID: derefStr(o.Id), Status: strings.ToLower(derefStr(o.Status))}
	if o.Filled != nil && positiveOrZero(*o.Filled) {
		f.Known, f.Filled = true, *o.Filled
	}
	if o.Average != nil && positiveFinite(*o.Average) {
		f.AvgPrice = *o.Average
	} else if o.Price != nil && positiveFinite(*o.Price) {
		f.AvgPrice = *o.Price
	}
	if o.Fee.Cost != nil && positiveFinite(*o.Fee.Cost) {
		f.Fee = *o.Fee.Cost
	}
	switch f.Status {
	case "closed", "filled", "canceled", "cancelled", "expired", "rejected":
		f.Done = true
	}
	return f
}

// Fetch reads the order back.
func (e *dcaExchange) Fetch(ctx context.Context, p *dca.Plan, orderID string) (dca.Fill, error) {
	if broker.IsPaperMode(e.cfg) {
		return dca.Fill{}, errNoFetchInPaper
	}
	tp, _, err := broker.TradingProviderFor(e.cfg, p.Provider, p.Account)
	if err != nil {
		return dca.Fill{}, err
	}
	o, err := tp.FetchOrder(ctx, orderID, p.Symbol)
	if err != nil {
		return dca.Fill{}, err
	}
	return dcaFillOf(o), nil
}

// Rate is the USD value of one unit of the plan's quote currency now.
func (e *dcaExchange) Rate(ctx context.Context, p *dca.Plan) (float64, error) {
	md, err := e.market(p)
	if err != nil {
		return 0, err
	}
	rate, _, err := broker.QuoteToUSDWithFallback(ctx, e.cfg, md, broker.SymbolQuote(p.Symbol, p.Provider))
	return rate, err
}

// ---- the question ----

// dcaAsker turns a run outside its mandate into a code-built order proposal for
// exactly the plan's amount, from origin mandate, through the approval service.
type dcaAsker struct {
	cfg  *config.Config
	lang func() string
}

// NewDCAAsker builds the asker. The proposal is placed only if the owner approves
// the card; nothing here places anything.
func NewDCAAsker(cfg *config.Config, lang func() string) dca.Asker { return &dcaAsker{cfg: cfg, lang: lang} }

func (a *dcaAsker) Ask(ctx context.Context, p *dca.Plan, m dca.Mandate, q dca.Quote, why string) (string, error) {
	svc := currentProposer()
	if svc == nil {
		return "", errors.New("order approvals are not running")
	}
	text := "Outside the mandate: " + why + ". Approve to buy this once."
	if a.lang != nil && a.lang() == "th" {
		text = "อยู่นอกวงเงินที่อนุมัติ: " + why + " อนุมัติเพื่อซื้อครั้งนี้ครั้งเดียว"
	}
	if r := []rune(text); len(r) > approval.MaxRationaleRunes {
		text = string(r[:approval.MaxRationaleRunes-1]) + "."
	}
	pct := a.cfg.Proactive.Mandates.PriceMovePct
	if pct <= 0 {
		pct = dca.DefaultPriceMovePct
	}
	req := approval.ProposeRequest{
		Order: approval.OrderRequest{
			Provider: p.Provider, Account: p.Account, Market: approval.MarketSpot, Symbol: p.Symbol, Side: "buy",
			Type: "market", Amount: q.BaseAmount, CollarPct: max(pct, approval.DefaultCollarPct),
		},
		Rationale: plainLine(text), Origin: approval.OriginMandate, MandateID: m.ID, Expiry: approval.MandateAskExpiry,
	}
	prop, err := svc.Propose(ctx, req)
	if err != nil {
		return "", err
	}
	return prop.ID, nil
}

// plainLine makes model-free, code-built prose acceptable to approval.CleanRationale.
func plainLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
