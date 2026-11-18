package broker

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// SimulationProvider wraps a real Provider and intercepts all TradingProvider
// calls, returning realistic simulated fills based on live market prices.
// It fully satisfies all 4 broker interfaces — reads are delegated to the
// underlying provider unchanged.
//
// Every simulated order is recorded in a PaperBook, keyed by the account it was
// placed on, so it can be listed, fetched, cancelled and found in history, and
// survives a restart. An open limit order fills lazily: when the market has
// crossed it by the time the order is next read (see settle). There is no
// background goroutine.
//
// Production code does not construct it directly: TradingProviderFor wraps the
// real provider when config.TradingRiskConfig.PaperTradingMode is on.
type SimulationProvider struct {
	inner Provider
	book  *PaperBook
	// acct is the account key (AccountKey) the book is partitioned by.
	acct string
}

// paperOrderSeq numbers simulated orders. It is process-wide, not per provider,
// because TradingProviderFor builds a fresh provider per call and two orders placed
// in the same millisecond must still get different IDs. It is advanced atomically
// because the instances are shared by concurrent tool calls.
var paperOrderSeq atomic.Int64

// NewSimulationProvider wraps inner so that order-mutating calls are simulated,
// with an in-memory order book (it does not survive the process). inner must
// implement at least PortfolioProvider + MarketDataProvider.
func NewSimulationProvider(inner Provider) *SimulationProvider {
	book, err := NewMemoryPaperBook()
	if err != nil { // cannot happen for an in-memory database; fail loudly if it does
		panic(fmt.Sprintf("simulation: in-memory paper book: %v", err))
	}
	return NewSimulationProviderWithBook(inner, book, NormalizeAccountKey(inner.ID(), ""))
}

// NewSimulationProviderWithBook wraps inner with the given order book, keeping the
// orders under acct (use AccountKey).
func NewSimulationProviderWithBook(inner Provider, book *PaperBook, acct string) *SimulationProvider {
	return &SimulationProvider{inner: inner, book: book, acct: acct}
}

// --- Provider ---

func (s *SimulationProvider) ID() string              { return s.inner.ID() + ":sim" }
func (s *SimulationProvider) Category() AssetCategory { return s.inner.Category() }
func (s *SimulationProvider) GetMarketStatus(ctx context.Context, symbol string) (MarketStatus, error) {
	return s.inner.GetMarketStatus(ctx, symbol)
}

// --- PortfolioProvider (delegated) ---

func (s *SimulationProvider) GetBalances(ctx context.Context) ([]Balance, error) {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support portfolio data")
	}
	return pp.GetBalances(ctx)
}

func (s *SimulationProvider) GetWalletBalances(ctx context.Context, walletType string) ([]WalletBalance, error) {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support portfolio data")
	}
	return pp.GetWalletBalances(ctx, walletType)
}

func (s *SimulationProvider) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return 0, fmt.Errorf("simulation: inner provider does not support price fetching")
	}
	return pp.FetchPrice(ctx, asset, quote)
}

func (s *SimulationProvider) SupportedWalletTypes() []string {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return nil
	}
	return pp.SupportedWalletTypes()
}

// --- MarketDataProvider (delegated) ---

func (s *SimulationProvider) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return ccxt.Ticker{}, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchTicker(ctx, symbol)
}

func (s *SimulationProvider) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchTickers(ctx, symbols)
}

func (s *SimulationProvider) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchOHLCV(ctx, symbol, timeframe, since, limit)
}

func (s *SimulationProvider) FetchOrderBook(ctx context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return ccxt.OrderBook{}, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchOrderBook(ctx, symbol, depth)
}

func (s *SimulationProvider) LoadMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.LoadMarkets(ctx)
}

// --- TradingProvider (simulated) ---

// PaperOrderIDPrefix starts every simulated order ID, so a simulated order can
// never be mistaken for a real exchange ID (and vice versa) in logs, history
// or a follow-up cancel.
const PaperOrderIDPrefix = "paper-"

// CreateOrder simulates an order. Nothing reaches the exchange; only the
// ticker is read.
//
// A market order fills at the price a real one would be valued at: the ask for
// a buy, the bid for a sell, the last price when the ticker has no usable edge
// (resolveOrderPrice, the same rule the order-value cap uses). If no price can
// be determined the order is refused rather than "filled" at zero.
//
// A limit order fills immediately, at its limit price, when it is marketable
// against that same edge; otherwise it is recorded "open" and fills on a later
// read once the market crosses it (settle). Stop and take-profit orders are
// recorded "open": there is no trigger engine behind them.
//
// Average, Remaining, Fee and Cost are always set (zero when nothing filled),
// so downstream code that dereferences them behaves as it does for a real fill.
func (s *SimulationProvider) CreateOrder(
	ctx context.Context,
	symbol, orderType, side string,
	amount float64,
	price *float64,
	_ map[string]interface{},
) (ccxt.Order, error) {
	if !isPositiveFinite(amount) {
		return ccxt.Order{}, errors.New("simulation: amount must be a positive, finite number")
	}
	seq := paperOrderSeq.Add(1)
	nowMs := processNow().UnixMilli()
	id := fmt.Sprintf("%s%d-%d", PaperOrderIDPrefix, nowMs, seq)

	md, _ := s.inner.(MarketDataProvider)
	marketPx, _, pxErr := resolveOrderPrice(ctx, md, symbol, side, nil)

	var fillPrice float64
	status := paperStatusOpen
	switch orderType {
	case "market":
		if pxErr != nil {
			return ccxt.Order{}, fmt.Errorf("simulation: cannot price a market order for %s: %w", symbol, pxErr)
		}
		fillPrice, status = marketPx, paperStatusClosed
	case "limit":
		if price == nil || !isPositiveFinite(*price) {
			return ccxt.Order{}, errors.New("simulation: a limit order needs a positive, finite price")
		}
		fillPrice = *price
		if pxErr == nil && limitCrossed(side, *price, marketPx) {
			status = paperStatusClosed
		}
	default:
		if price != nil {
			fillPrice = *price
		}
	}

	row := paperRow{
		ID: id, Symbol: symbol, Type: orderType, Side: side,
		Amount: amount, Price: fillPrice, Status: status, CreatedMs: nowMs,
	}
	if status == paperStatusClosed {
		row.Filled, row.Average, row.Cost, row.ClosedMs = amount, fillPrice, amount*fillPrice, nowMs
	}
	// Re-price the account's older open orders on this symbol first, so the book is
	// current when the new order is added to it.
	if err := s.settle(ctx, symbol); err != nil {
		return ccxt.Order{}, err
	}
	if err := s.book.insert(ctx, s.acct, row); err != nil {
		return ccxt.Order{}, err
	}
	return row.order(), nil
}

// limitCrossed reports whether a limit order is marketable against the edge price
// resolveOrderPrice returns for its side: a buy when the ask is at or below the
// limit, a sell when the bid is at or above it.
func limitCrossed(side string, limit, edge float64) bool {
	return (side == "buy" && limit >= edge) || (side == "sell" && limit <= edge)
}

// order renders the row as a ccxt order the way an exchange would report it.
func (r paperRow) order() ccxt.Order {
	zeroFee := 0.0
	id, symbol, typ, side, status := r.ID, r.Symbol, r.Type, r.Side, r.Status
	amount, price, average, filled, cost := r.Amount, r.Price, r.Average, r.Filled, r.Cost
	remaining := r.Amount - r.Filled
	created := r.CreatedMs
	o := ccxt.Order{
		Id:        &id,
		Symbol:    &symbol,
		Type:      &typ,
		Side:      &side,
		Amount:    &amount,
		Price:     &price,
		Average:   &average,
		Filled:    &filled,
		Remaining: &remaining,
		Cost:      &cost,
		Status:    &status,
		Fee:       ccxt.Fee{Cost: &zeroFee},
		Datetime:  ptr(time.UnixMilli(created).UTC().Format(time.RFC3339)),
		Timestamp: &created,
	}
	if r.ClosedMs > 0 {
		closed := r.ClosedMs
		o.LastTradeTimestamp = &closed
		o.LastUpdateTimestamp = &closed
	}
	return o
}

// trade renders a filled order as its single simulated fill.
func (r paperRow) trade() ccxt.Trade {
	id, order, symbol, typ, side := r.ID, r.ID, r.Symbol, r.Type, r.Side
	amount, price, cost := r.Filled, r.Average, r.Cost
	at := r.ClosedMs
	zeroFee := 0.0
	return ccxt.Trade{
		Id:        &id,
		Order:     &order,
		Symbol:    &symbol,
		Type:      &typ,
		Side:      &side,
		Amount:    &amount,
		Price:     &price,
		Cost:      &cost,
		Timestamp: &at,
		Datetime:  ptr(time.UnixMilli(at).UTC().Format(time.RFC3339)),
		Fee:       ccxt.Fee{Cost: &zeroFee},
	}
}

// settle is the lazy fill: it re-prices the account's open limit orders (one
// symbol, or all when symbol is empty) with the same resolveOrderPrice edge rule
// CreateOrder uses, and fills each one the market has crossed, at its limit price.
// It runs on every read and on create, so an order fills "on the next read after
// the market crossed it", not at the moment it crossed. A symbol that cannot be
// priced leaves its orders open. Stop and take-profit orders are never settled.
func (s *SimulationProvider) settle(ctx context.Context, symbol string) error {
	open, err := s.book.open(ctx, s.acct, symbol)
	if err != nil {
		return err
	}
	md, _ := s.inner.(MarketDataProvider)
	edge := map[string]*float64{} // symbol|side -> edge price; nil when unpriceable
	for _, o := range open {
		if o.Type != "limit" {
			continue
		}
		k := o.Symbol + "|" + o.Side
		px, seen := edge[k]
		if !seen {
			if v, _, perr := resolveOrderPrice(ctx, md, o.Symbol, o.Side, nil); perr == nil {
				px = &v
			}
			edge[k] = px
		}
		if px == nil || !limitCrossed(o.Side, o.Price, *px) {
			continue
		}
		if _, err := s.book.fill(ctx, s.acct, o.ID, o.Price, processNow().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// paperID checks id is one of the simulation's own. A real exchange order ID is
// never answered from the book.
func paperID(id string) error {
	if !IsPaperOrderID(id) {
		return fmt.Errorf("%w: %q", ErrNotPaperOrderID, id)
	}
	return nil
}

// CancelOrder cancels a stored open paper order. An unknown paper ID is an error;
// an order that is already filled or cancelled is returned as it stands, unchanged.
func (s *SimulationProvider) CancelOrder(ctx context.Context, id, _ string) (ccxt.Order, error) {
	if err := paperID(id); err != nil {
		return ccxt.Order{}, err
	}
	row, err := s.book.get(ctx, s.acct, id)
	if err != nil {
		return ccxt.Order{}, err
	}
	// A crossed order has filled by now; cancelling it must not undo the fill.
	if err := s.settle(ctx, row.Symbol); err != nil {
		return ccxt.Order{}, err
	}
	if _, err := s.book.cancel(ctx, s.acct, id, processNow().UnixMilli()); err != nil {
		return ccxt.Order{}, err
	}
	row, err = s.book.get(ctx, s.acct, id)
	if err != nil {
		return ccxt.Order{}, err
	}
	return row.order(), nil
}

// FetchOrder returns the stored paper order, filled first if the market has crossed
// it. An unknown ID is an error, never a made-up "open" order.
func (s *SimulationProvider) FetchOrder(ctx context.Context, id, _ string) (ccxt.Order, error) {
	if err := paperID(id); err != nil {
		return ccxt.Order{}, err
	}
	row, err := s.book.get(ctx, s.acct, id)
	if err != nil {
		return ccxt.Order{}, err
	}
	if row.Status == paperStatusOpen {
		if err := s.settle(ctx, row.Symbol); err != nil {
			return ccxt.Order{}, err
		}
		if row, err = s.book.get(ctx, s.acct, id); err != nil {
			return ccxt.Order{}, err
		}
	}
	return row.order(), nil
}

// FetchOpenOrders returns the account's open paper orders after the lazy fill, for
// one symbol or (symbol empty) all of them.
func (s *SimulationProvider) FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	if err := s.settle(ctx, symbol); err != nil {
		return nil, err
	}
	rows, err := s.book.open(ctx, s.acct, symbol)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.order())
	}
	return out, nil
}

// FetchClosedOrders returns the account's filled and cancelled paper orders.
func (s *SimulationProvider) FetchClosedOrders(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	if err := s.settle(ctx, symbol); err != nil {
		return nil, err
	}
	rows, err := s.book.closed(ctx, s.acct, symbol, sinceMs(since), limit)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.order())
	}
	return out, nil
}

// FetchMyTrades returns the simulated fills: one per filled paper order.
func (s *SimulationProvider) FetchMyTrades(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	if err := s.settle(ctx, symbol); err != nil {
		return nil, err
	}
	rows, err := s.book.closed(ctx, s.acct, symbol, sinceMs(since), 0)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Trade, 0, len(rows))
	for _, r := range rows {
		if r.Filled > 0 {
			out = append(out, r.trade())
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func sinceMs(since *int64) int64 {
	if since == nil {
		return 0
	}
	return *since
}

// --- TransferProvider (simulated) ---

func (s *SimulationProvider) Transfer(_ context.Context, asset string, amount float64, from, to string) (ccxt.TransferEntry, error) {
	id := fmt.Sprintf("paper-transfer-%d", processNow().UnixMilli())
	return ccxt.TransferEntry{
		Id:          &id,
		Currency:    &asset,
		Amount:      &amount,
		FromAccount: &from,
		ToAccount:   &to,
	}, nil
}

// ptr returns a pointer to v. Duplicated locally to avoid cross-package dep.
func ptr(v string) *string { return &v }
