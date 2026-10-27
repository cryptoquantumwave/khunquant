package market

import (
	"errors"
	"math"
	"net/url"
	"strconv"
	"time"
)

// DefaultFunding is the funding rate (per 8 h) a pair has until the `set` op gives it one.
const DefaultFunding = 0.0001

const fundingInterval = 8 * time.Hour

// SetFunding sets the funding rate a venue (or "*": every venue without its own) serves for
// p's perpetual. Funding payments are not simulated here, only the current rate.
func (b *Book) SetFunding(venue string, p Pair, rate float64) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || math.Abs(rate) >= 1 {
		return errors.New("funding_rate must be a fraction per 8 h, e.g. 0.0001")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.funding[venue] == nil {
		b.funding[venue] = map[Pair]float64{}
	}
	b.funding[venue][p] = rate
	return nil
}

// Funding is the funding rate p has on venue.
func (b *Book) Funding(venue string, p Pair) float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if r, ok := b.funding[venue][p]; ok {
		return r
	}
	if r, ok := b.funding[Wildcard][p]; ok {
		return r
	}
	return DefaultFunding
}

// nextSettlement is the next 00:00, 08:00 or 16:00 UTC after t.
func nextSettlement(t time.Time) time.Time { return alignDown(t, fundingInterval).Add(fundingInterval) }

// binancePremium serves /fapi/v1/premiumIndex (what ccxt reads for mark price and funding
// rate): one symbol as an object, none as an array of the modelled pairs. Mark and index
// are the book's price.
func (b *Book) binancePremium(q url.Values) (int, []byte, bool) {
	var pairs []Pair
	if w := q.Get("symbol"); w != "" {
		if p, _, ok := ParseWire("binance", w); ok {
			pairs = []Pair{p}
		}
	} else {
		pairs = b.Pairs("binance")
	}
	now, rows := b.Now(), []map[string]any{}
	for _, p := range pairs {
		px, ok := b.Price("binance", p, now)
		if !ok {
			continue
		}
		rows = append(rows, map[string]any{
			"symbol": Wire("binance", p, true), "markPrice": num(px), "indexPrice": num(px), "estimatedSettlePrice": num(px),
			"lastFundingRate": num(b.Funding("binance", p)), "interestRate": "0.00010000",
			"nextFundingTime": ms(nextSettlement(now)), "time": ms(now),
		})
	}
	switch {
	case len(rows) == 0:
		return 0, nil, false
	case q.Get("symbol") != "":
		return reply(200, rows[0])
	}
	return reply(200, rows)
}

// okxInstruments lists what an OKX request for instId (or instType SWAP when empty or ANY) asks
// for: the modelled swaps, or the one named instrument.
func (b *Book) okxInstruments(q url.Values, swapsOnly bool) []instrument {
	id := q.Get("instId")
	if id == "" || id == "ANY" {
		if id == "" && q.Get("instType") != "SWAP" {
			return nil
		}
		var out []instrument
		for _, p := range b.Pairs("okx") {
			out = append(out, instrument{p, true})
		}
		return out
	}
	if p, swap, ok := ParseWire("okx", id); ok && (swap || !swapsOnly) {
		return []instrument{{p, swap}}
	}
	return nil
}

type instrument struct {
	p    Pair
	swap bool
}

// okxMarkPrice serves /api/v5/public/mark-price (instId, or instType=SWAP for all).
func (b *Book) okxMarkPrice(q url.Values) (int, []byte, bool) {
	now, rows := b.Now(), []map[string]string{}
	for _, in := range b.okxInstruments(q, false) {
		px, ok := b.Price("okx", in.p, now)
		if !ok {
			continue
		}
		instType := "MARGIN"
		if in.swap {
			instType = "SWAP"
		}
		rows = append(rows, map[string]string{
			"instType": instType, "instId": Wire("okx", in.p, in.swap), "markPx": num(px), "ts": strconv.FormatInt(ms(now), 10),
		})
	}
	if len(rows) == 0 {
		return 0, nil, false
	}
	return okxReply(rows)
}

// okxFundingRate serves /api/v5/public/funding-rate (a swap's instId, or ANY for all).
func (b *Book) okxFundingRate(q url.Values) (int, []byte, bool) {
	now, rows := b.Now(), []map[string]string{}
	at, next := nextSettlement(now), nextSettlement(now).Add(fundingInterval)
	for _, in := range b.okxInstruments(q, true) {
		if _, ok := b.Price("okx", in.p, now); !ok {
			continue
		}
		rate := num(b.Funding("okx", in.p))
		rows = append(rows, map[string]string{
			"instType": "SWAP", "instId": Wire("okx", in.p, true), "method": "current_period", "fundingRate": rate,
			"nextFundingRate": rate, "fundingTime": strconv.FormatInt(ms(at), 10), "nextFundingTime": strconv.FormatInt(ms(next), 10),
			"minFundingRate": "-0.00375", "maxFundingRate": "0.00375", "premium": "0", "settState": "settled",
			"settFundingRate": rate, "ts": strconv.FormatInt(ms(now), 10),
		})
	}
	if len(rows) == 0 {
		return 0, nil, false
	}
	return okxReply(rows)
}
