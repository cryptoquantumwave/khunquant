package tools

// Sizing a spot order by a quote-currency amount ("buy 20 USDT of BTC").
//
// The exchange flags that do this (OKX tgtCcy, Binance quoteOrderQty) mean
// different things on each venue and do not exist at all on some, so the tool
// converts instead: quote_amount / price, rounded DOWN to the market's amount
// step, checked against the market's minimums. The result is an ordinary base
// amount, and every gate of create_order and propose_order then runs on it
// exactly as if the model had passed it as `amount`.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// quoteAmountSchema is the schema entry shared by create_order and propose_order.
func quoteAmountSchema() map[string]any {
	return map[string]any{
		"type": "number",
		"description": "Spot only: order size as an amount of the symbol's QUOTE currency (e.g. 20 for 20 USDT of BTC/USDT, or THB for BTC/THB), " +
			"as an alternative to amount. Give exactly one of amount and quote_amount. The system converts it to a base quantity at the " +
			"order's own price (ask for a buy, bid for a sell, the limit price for a limit order), rounds it down to the market's step and " +
			"checks the market minimums; do not do the arithmetic yourself. Never pass tgtCcy or quoteOrderQty in params.",
	}
}

// quoteAmountArg reads quote_amount. present is false when the argument is
// absent or null; a present value that is not a positive, finite number is an
// error (NaN and Inf arrive straight from JSON tool arguments and defeat an
// ordinary "<= 0" guard).
func quoteAmountArg(args map[string]any) (v float64, present bool, err error) {
	raw, ok := args["quote_amount"]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch n := raw.(type) {
	case float64:
		v = n
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	default:
		return 0, true, errors.New("quote_amount must be a positive, finite number")
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, true, errors.New("quote_amount must be a positive, finite number")
	}
	return v, true, nil
}

// quoteAmountContractRefusal is the refusal for quote_amount on a contract symbol.
// The futures tools already size by a USD notional, so the message says so.
func quoteAmountContractRefusal(symbol string) string {
	return fmt.Sprintf("quote_amount sizes spot orders only: %s is a contract (futures/swap) symbol whose order size is a contract count. "+
		"The futures tools already take a USD value: use futures_open_position (or propose_order with market=futures) with notional_usd, "+
		"and the system computes the contracts", symbol)
}

// quoteSizing is the result of converting a quote amount to a base quantity.
type quoteSizing struct {
	Symbol      string
	QuoteAmount float64
	Base        float64
	Price       float64
	Source      string // a broker.Source* value
	QuoteCcy    string
	BaseCcy     string
}

func (q quoteSizing) priceLabel() string {
	switch q.Source {
	case broker.SourceTickerAsk:
		return "ask"
	case broker.SourceTickerBid:
		return "bid"
	case broker.SourceTickerLast:
		return "last"
	case broker.SourceLimitPrice:
		return "limit price"
	}
	return "price"
}

// describe renders "quote 20 USDT ≈ 0.000204 BTC at ask 98000".
func (q quoteSizing) describe() string {
	return fmt.Sprintf("quote %s %s ≈ %s %s at %s %s",
		fmtQty(q.QuoteAmount), q.QuoteCcy, fmtQty(q.Base), q.BaseCcy, q.priceLabel(), fmtQty(q.Price))
}

// fmtQty is the shortest exact decimal, never an exponent.
func fmtQty(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// symbolParts splits "BTC/USDT" (or "BTC/USDT:USDT") into base and quote.
func symbolParts(symbol string) (base, quote string) {
	base, rest, ok := strings.Cut(symbol, "/")
	if !ok {
		return symbol, ""
	}
	quote, _, _ = strings.Cut(rest, ":")
	return base, quote
}

// fallbackAmountStep floors an amount to 8 decimals when the market publishes
// no amount precision (the Bitkub and Binance TH catalogues carry none).
const fallbackAmountStep = 1e-8

// sizeSpotByQuote converts quoteAmount of the symbol's quote currency into a
// base quantity. limitPrice is the order's limit price, nil for a market order.
//
// The price is the one the simulation and the order-value cap use
// (broker.EstimateNotionalQuote: the ask for a buy, the bid for a sell, the
// last as the fallback), so the estimate, the cap and the fill agree. When no
// price can be determined, or the market catalogue cannot be read, the order is
// refused: it is never sized from zero or a guess.
func sizeSpotByQuote(ctx context.Context, md broker.MarketDataProvider, symbol, side string, limitPrice *float64, quoteAmount float64) (quoteSizing, error) {
	if strings.Contains(symbol, ":") {
		return quoteSizing{}, errors.New(quoteAmountContractRefusal(symbol))
	}
	if math.IsNaN(quoteAmount) || math.IsInf(quoteAmount, 0) || quoteAmount <= 0 {
		return quoteSizing{}, errors.New("quote_amount must be a positive, finite number")
	}
	baseCcy, quoteCcy := symbolParts(symbol)
	out := quoteSizing{Symbol: symbol, QuoteAmount: quoteAmount, QuoteCcy: quoteCcy, BaseCcy: baseCcy}

	switch {
	case limitPrice != nil:
		if math.IsNaN(*limitPrice) || math.IsInf(*limitPrice, 0) || *limitPrice <= 0 {
			return quoteSizing{}, errors.New("price must be a positive, finite number")
		}
		out.Price, out.Source = *limitPrice, broker.SourceLimitPrice
	case md == nil:
		return quoteSizing{}, fmt.Errorf("cannot size the order: no price for %s (the provider has no market data)", symbol)
	default:
		nq, err := broker.EstimateNotionalQuote(ctx, md, symbol, side, 1, nil)
		if err != nil {
			return quoteSizing{}, fmt.Errorf("cannot size the order: no price for %s: %w", symbol, err)
		}
		out.Price, out.Source = nq.Price, nq.Source
	}
	if math.IsNaN(out.Price) || math.IsInf(out.Price, 0) || out.Price <= 0 {
		return quoteSizing{}, fmt.Errorf("cannot size the order: no usable price for %s", symbol)
	}

	if md == nil {
		return quoteSizing{}, fmt.Errorf("cannot size the order: no market data for %s to read its amount step and minimums", symbol)
	}
	markets, err := md.LoadMarkets(ctx)
	if err != nil {
		return quoteSizing{}, fmt.Errorf("cannot size the order: the market catalogue is unavailable: %w", err)
	}
	m, ok := markets[symbol]
	if !ok {
		return quoteSizing{}, fmt.Errorf("cannot size the order: %s is not in the market catalogue", symbol)
	}

	raw := quoteAmount / out.Price
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw <= 0 {
		return quoteSizing{}, fmt.Errorf("cannot size the order: %s at %s is not a usable quantity", fmtQty(quoteAmount), fmtQty(out.Price))
	}
	step := fallbackAmountStep
	if m.Precision.Amount != nil && positiveFinite(*m.Precision.Amount) {
		step = *m.Precision.Amount
	}
	out.Base = floorToStep(raw, step)
	if !positiveFinite(out.Base) {
		return quoteSizing{}, fmt.Errorf("quote %s %s buys less than one tradable step (%s %s) of %s at %s %s",
			fmtQty(quoteAmount), quoteCcy, fmtQty(step), baseCcy, symbol, out.priceLabel(), fmtQty(out.Price))
	}
	if err := checkMarketMinimums(m, out); err != nil {
		return quoteSizing{}, err
	}
	return out, nil
}

// checkMarketMinimums refuses a converted order under the market's minimum amount or cost.
func checkMarketMinimums(m ccxt.MarketInterface, s quoteSizing) error {
	if min := m.Limits.Amount.Min; min != nil && positiveFinite(*min) && s.Base < *min {
		return fmt.Errorf("quote %s %s converts to %s %s, below the minimum order amount of %s %s for %s",
			fmtQty(s.QuoteAmount), s.QuoteCcy, fmtQty(s.Base), s.BaseCcy, fmtQty(*min), s.BaseCcy, s.Symbol)
	}
	if min := m.Limits.Cost.Min; min != nil && positiveFinite(*min) && s.Base*s.Price < *min {
		return fmt.Errorf("quote %s %s converts to an order worth %s %s, below the minimum order cost of %s %s for %s",
			fmtQty(s.QuoteAmount), s.QuoteCcy, fmtQty(s.Base*s.Price), s.QuoteCcy, fmtQty(*min), s.QuoteCcy, s.Symbol)
	}
	return nil
}

// floorToStep rounds x DOWN to a multiple of step and returns it as a clean
// decimal (no 0.20400000000000003). The tolerance only absorbs binary
// representation error (0.3/0.1 = 2.9999999999999996); the result is checked
// against x so it can never exceed it by more than that error.
func floorToStep(x, step float64) float64 {
	if !positiveFinite(x) || !positiveFinite(step) {
		return 0
	}
	n := math.Floor(x/step + 1e-9)
	decimals := 0
	if s := strconv.FormatFloat(step, 'f', -1, 64); strings.Contains(s, ".") {
		decimals = len(s) - strings.Index(s, ".") - 1
	}
	pow := math.Pow(10, float64(decimals))
	clean := func(n float64) float64 { return math.Round(n*step*pow) / pow }
	r := clean(n)
	for r > x && n > 0 && (r-x)/x > 1e-9 {
		n--
		r = clean(n)
	}
	if r > x*(1+1e-9) {
		return 0
	}
	return r
}
