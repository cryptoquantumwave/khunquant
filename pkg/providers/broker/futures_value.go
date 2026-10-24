package broker

import (
	"context"
	"math"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// PositionNotionalUSD returns the USD notional of a futures position given its
// contract count, the base-currency size of one contract and the mark price.
// A position's Contracts field is a CONTRACT COUNT, not a base quantity (OKX
// CHZ/USDT:USDT is 10 CHZ per contract, BTC/USDT:USDT is 0.01 BTC), so valuing it
// as contracts × markPrice is wrong by the contract size. A missing or non-positive
// contractSize is treated as 1 (Binance USDT-M, where contracts are base units).
// Returns 0 for any non-finite input or a non-positive mark price.
//
// A caller that must not guess the size (a trigger deciding on exposure) resolves
// it first with ContractSizeResolver and does not call this when the size is
// unknown.
func PositionNotionalUSD(contracts, contractSize, markPrice float64) float64 {
	for _, v := range []float64{contracts, contractSize, markPrice} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0
		}
	}
	if markPrice <= 0 {
		return 0
	}
	if contractSize <= 0 {
		contractSize = 1
	}
	return math.Abs(contracts) * contractSize * markPrice
}

// FuturesMarketLoader is the part of a FuturesProvider that ContractSizeResolver
// needs: the market catalogue.
type FuturesMarketLoader interface {
	LoadFuturesMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error)
}

// ContractSizeResolver finds the contract size of futures positions: the
// position's own ContractSize when present and positive, otherwise the market's.
// The market catalogue is loaded at most once per resolver, and only if some
// position needs it; a resolver is therefore cheap to build and is kept for as
// long as the caller is willing to trust one catalogue. It is not safe for
// concurrent use.
type ContractSizeResolver struct {
	src     FuturesMarketLoader
	markets map[string]ccxt.MarketInterface
	loaded  bool
}

// NewContractSizeResolver returns a resolver that loads markets from src.
func NewContractSizeResolver(src FuturesMarketLoader) *ContractSizeResolver {
	return &ContractSizeResolver{src: src}
}

// Size returns (contractSize, true) when it can be determined, else (0, false).
// symbol is the CCXT market symbol used for the catalogue lookup.
func (r *ContractSizeResolver) Size(ctx context.Context, p ccxt.Position, symbol string) (float64, bool) {
	if p.ContractSize != nil && *p.ContractSize > 0 && !math.IsInf(*p.ContractSize, 0) {
		return *p.ContractSize, true
	}
	if !r.loaded && r.src != nil {
		r.loaded = true
		if m, err := r.src.LoadFuturesMarkets(ctx); err == nil {
			r.markets = m
		}
	}
	if m, ok := r.markets[symbol]; ok && m.ContractSize != nil && *m.ContractSize > 0 && !math.IsInf(*m.ContractSize, 0) {
		return *m.ContractSize, true
	}
	return 0, false
}
