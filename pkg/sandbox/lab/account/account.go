// Package account is the lab group "account": put balances and positions into the
// simulated venues and read them back, without placing orders. It touches VenueState
// only through the simulator's own accessors, under its lock.
package account

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// SeedArgs is the request of "seed". Only binance and okx have a simulator today.
type SeedArgs struct {
	Venue     string             `json:"venue"`              // binance | okx
	Balances  map[string]float64 `json:"balances,omitempty"` // asset -> free
	Positions []sandbox.Position `json:"positions,omitempty"`
	Replace   bool               `json:"replace,omitempty"`  // clear balances and positions first
	Baseline  *bool              `json:"baseline,omitempty"` // default true: a reset returns to this seed
}

const (
	argVenue = `"venue":{"type":"string","enum":["binance","okx"]}`
	argTail  = `"status":{"type":"string","enum":["open","closed","all"],"description":"default all"},` +
		`"symbol":{"type":"string","description":"wire symbol: BTCUSDT on binance, BTC-USDT-SWAP on okx"}`
)

type request struct {
	Op string `json:"op"`
	SeedArgs
	Status string `json:"status"`
	Symbol string `json:"symbol"`
}

// Group serves the simulator sim, which lives as long as the process.
func Group(sim *sandbox.StatefulSimulator) lab.Group {
	return lab.Group{
		Name: "account",
		Description: "Seed and read the simulated venues' accounts (binance, okx; one account each). " +
			"Prices are not set here: see sandbox_market.",
		Ops: []lab.Op{
			{Name: "seed", Description: "set free balances and put positions; by default the result is what sandbox_reset_simulator returns to",
				Args: json.RawMessage(`{` + argVenue + `,"balances":{"type":"object","description":"asset -> free amount"},` +
					`"positions":{"type":"array","items":{"type":"object"},"description":"[{symbol (wire), side long|short, contracts, entryPrice, leverage}]; the market must exist"},` +
					`"replace":{"type":"boolean","description":"clear balances and positions first"},` +
					`"baseline":{"type":"boolean","description":"default true"}}`),
				Required: []string{"venue"}, Run: run(sim, seed)},
			{Name: "get", Description: "balances, positions and order counts of a venue",
				Args: json.RawMessage(`{` + argVenue + `}`), Required: []string{"venue"}, Run: run(sim, get)},
			{Name: "orders", Description: "the venue's orders, oldest first",
				Args: json.RawMessage(`{` + argVenue + `,` + argTail + `}`), Required: []string{"venue"}, Run: run(sim, orders)},
		},
	}
}

func run(sim *sandbox.StatefulSimulator, op func(*sandbox.StatefulSimulator, request) (any, error)) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var r request
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		switch r.Venue {
		case "binance", "okx":
		case "":
			return nil, lab.Errorf(lab.CodeBadArgs, "venue is required")
		default: // GetState would create state for any name
			return nil, lab.Errorf(lab.CodeUnavailable, "venue %q has no simulator account (binance and okx do)", r.Venue)
		}
		return op(sim, r)
	}
}

func seed(sim *sandbox.StatefulSimulator, r request) (any, error) {
	for a, v := range r.Balances {
		if a == "" || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, lab.Errorf(lab.CodeBadArgs, "balance %q: want a non-empty asset and a finite amount >= 0", a)
		}
	}
	err := sim.Mutate(r.Venue, func(st *sandbox.VenueState) error {
		ps, err := positions(st, r.Positions) // checked before anything is written
		if err != nil {
			return err
		}
		if r.Replace {
			clear(st.Balances)
			clear(st.Positions)
		}
		for a, v := range r.Balances {
			st.Balances[a] = sandbox.Balance{Free: v}
		}
		for _, p := range ps {
			st.Positions[p.Symbol] = p
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if r.Baseline == nil || *r.Baseline {
		sim.Rebaseline(r.Venue)
	}
	return get(sim, r)
}

// positions validates ps against the venue's markets and returns copies to store. A
// position is keyed by wire symbol, which must be a market; the contract size comes
// from it unless given, because the venue's wire format multiplies by it.
func positions(st *sandbox.VenueState, ps []sandbox.Position) ([]*sandbox.Position, error) {
	out := make([]*sandbox.Position, 0, len(ps))
	for _, p := range ps {
		m, ok := st.Markets[p.Symbol]
		side := strings.ToLower(p.Side)
		switch {
		case !ok:
			return nil, lab.Errorf(lab.CodeBadArgs, "position %q: no such market on this venue", p.Symbol)
		case side != "long" && side != "short":
			return nil, lab.Errorf(lab.CodeBadArgs, "position %q: side must be long or short", p.Symbol)
		case !(p.Contracts > 0) || p.EntryPrice < 0 || p.Leverage < 0:
			return nil, lab.Errorf(lab.CodeBadArgs, "position %q: contracts must be > 0, entryPrice and leverage >= 0", p.Symbol)
		}
		p.Side = side
		p.ContractSize = cmp.Or(p.ContractSize, m.ContractSize)
		p.Leverage = cmp.Or(p.Leverage, st.Leverage[p.Symbol])
		p.MarkPrice = cmp.Or(p.MarkPrice, p.EntryPrice) // the venue refreshes it from the price source
		if p.UpdatedAt.IsZero() {
			p.UpdatedAt = time.Now()
		}
		if p.ContractSize <= 0 {
			return nil, lab.Errorf(lab.CodeBadArgs, "position %q: the market has no contract size, give contractSize", p.Symbol)
		}
		out = append(out, &p)
	}
	return out, nil
}

func get(sim *sandbox.StatefulSimulator, r request) (any, error) {
	var out struct {
		Venue     string                     `json:"venue"`
		Balances  map[string]sandbox.Balance `json:"balances"`
		Positions []sandbox.Position         `json:"positions"`
		Open      int                        `json:"open_orders"`
		Closed    int                        `json:"closed_orders"`
	}
	out.Venue = r.Venue
	sim.Read(r.Venue, func(st *sandbox.VenueState) { // values are copied: Respond edits the pointers later
		out.Balances = maps.Clone(st.Balances)
		for _, p := range st.Positions {
			out.Positions = append(out.Positions, *p)
		}
		out.Open, out.Closed = len(st.OpenOrders), len(st.ClosedOrders)
	})
	slices.SortFunc(out.Positions, func(a, b sandbox.Position) int { return cmp.Compare(a.Symbol, b.Symbol) })
	return out, nil
}

func orders(sim *sandbox.StatefulSimulator, r request) (any, error) {
	status := cmp.Or(r.Status, "all")
	if status != "open" && status != "closed" && status != "all" {
		return nil, lab.Errorf(lab.CodeBadArgs, "status must be open, closed or all")
	}
	var list []sandbox.Order
	sim.Read(r.Venue, func(st *sandbox.VenueState) {
		add := func(m map[string]*sandbox.Order) {
			for _, o := range m {
				if o != nil && (r.Symbol == "" || o.Symbol == r.Symbol) {
					c := *o
					c.Params = maps.Clone(o.Params)
					list = append(list, c)
				}
			}
		}
		if status != "closed" {
			add(st.OpenOrders)
		}
		if status != "open" {
			add(st.ClosedOrders)
		}
	})
	slices.SortFunc(list, func(a, b sandbox.Order) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(len(a.ID), len(b.ID)), cmp.Compare(a.ID, b.ID))
	})
	return map[string]any{"venue": r.Venue, "orders": list}, nil
}
