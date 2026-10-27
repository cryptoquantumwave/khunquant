// Package marketops is the lab group "market": drive SimMarket's prices. It is a package
// of its own because lab holds a *market.Book, so market must not import lab.
package marketops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

const (
	symVenue = `"symbol":{"type":"string","description":"BASE/QUOTE, e.g. BTC/USDT (a spot market and its USDT swap share it)"},` +
		`"venue":{"type":"string","enum":["binance","okx","*"],"description":"default *: every venue without a model of its own"}`
)

// Options are what the group needs from the instance.
type Options struct {
	Home string // the instance HOME: csv_path must lie inside it
}

// Group sets the prices the simulated venues serve and fill at.
func Group(b *market.Book, o Options) lab.Group {
	return lab.Group{
		Name: "market",
		Description: "Simulated market prices: one price per symbol, shared by every venue's ticker, klines and fills. " +
			"Ops start from the current price; history is kept.",
		Ops: []lab.Op{
			{Name: "get", Description: "the price now of every modelled symbol, or of one",
				Args: json.RawMessage(`{` + symVenue + `}`), Run: run(b, nil)},
			{Name: "set", Description: "hold a constant price from now",
				Args:     json.RawMessage(`{` + symVenue + `,"price":{"type":"number"}}`),
				Required: []string{"symbol", "price"}, Run: run(b, setOp)},
			{Name: "path", Description: "move from the current price to `to` over `over`, then hold; keyframes pass prices on the way",
				Args: json.RawMessage(`{` + symVenue + `,"to":{"type":"number"},"over":{"type":"string","description":"duration, e.g. 3m"},` +
					`"shape":{"type":"string","enum":["linear","step"]},` +
					`"keyframes":{"type":"array","items":{"type":"object"},"description":"[{at:\"+90s\",price}] inside over"}}`),
				Required: []string{"symbol", "to", "over"}, Run: run(b, pathOp)},
			{Name: "shock", Description: "gap the price by pct percent now (history keeps the old price)",
				Args:     json.RawMessage(`{` + symVenue + `,"pct":{"type":"number"}}`),
				Required: []string{"symbol", "pct"}, Run: run(b, shockOp)},
			{Name: "walk", Description: "seeded random walk from the current price; vol and drift are per step; the same seed gives the same path",
				Args: json.RawMessage(`{` + symVenue + `,"vol":{"type":"number"},"drift":{"type":"number"},"seed":{"type":"integer"},` +
					`"step":{"type":"string","description":"default 1m"}}`),
				Required: []string{"symbol", "vol", "seed"}, Run: run(b, walkOp)},
			{Name: "replay", Description: "play a candle CSV (ts,open,high,low,close[,volume]) from now; the bar holding `start` opens at the current boundary",
				Args: json.RawMessage(`{` + symVenue + `,"csv_path":{"type":"string","description":"a file inside the instance home"},` +
					`"csv_inline":{"type":"string","description":"the CSV text itself"},` +
					`"timeframe":{"type":"string","description":"the CSV bar length, e.g. 1m"},` +
					`"start":{"type":"string","description":"RFC 3339 or unix time of the CSV bar to begin with; default the first"}}`),
				Required: []string{"symbol", "timeframe"}, Run: run(b, replayOp(o.Home))},
		},
	}
}

type req struct {
	Op        string  `json:"op"`
	Symbol    string  `json:"symbol"`
	Venue     string  `json:"venue"`
	Price     float64 `json:"price"`
	To        float64 `json:"to"`
	Over      string  `json:"over"`
	Shape     string  `json:"shape"`
	Pct       float64 `json:"pct"`
	Vol       float64 `json:"vol"`
	Drift     float64 `json:"drift"`
	Seed      *uint64 `json:"seed"`
	Step      string  `json:"step"`
	CSVPath   string  `json:"csv_path"`
	CSVInline string  `json:"csv_inline"`
	Timeframe string  `json:"timeframe"`
	Start     string  `json:"start"`
	Keyframes []struct {
		At    string  `json:"at"`
		Price float64 `json:"price"`
	} `json:"keyframes"`
}

// run decodes the request strictly (a misspelt field must not be ignored), applies op
// (nil for get) to the symbol and answers with the resulting prices. Every op but get
// needs a symbol.
func run(b *market.Book, op func(*market.Book, req, market.Pair) error) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a req
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		var only *market.Pair
		if a.Symbol != "" || op != nil {
			p, err := market.ParsePair(a.Symbol)
			if err != nil {
				return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
			}
			only = &p
		}
		if op != nil {
			if err := op(b, a, *only); err != nil {
				return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
			}
		}
		return map[string]any{"prices": b.Snapshot(a.Venue, only)}, nil
	}
}

func setOp(b *market.Book, a req, p market.Pair) error { return b.Set(a.Venue, p, a.Price) }

func shockOp(b *market.Book, a req, p market.Pair) error { return b.Shock(a.Venue, p, a.Pct) }

// offset reads a duration, with the optional leading "+" of a keyframe ("+90s").
func offset(s string) (time.Duration, error) { return time.ParseDuration(strings.TrimPrefix(s, "+")) }

func pathOp(b *market.Book, a req, p market.Pair) error {
	over, err := offset(a.Over)
	if err != nil {
		return err
	}
	var kfs []market.Keyframe
	for _, k := range a.Keyframes {
		at, err := offset(k.At)
		if err != nil {
			return err
		}
		kfs = append(kfs, market.Keyframe{At: at, Price: k.Price})
	}
	shape := market.Shape(a.Shape)
	if shape == "" {
		shape = market.ShapeLinear
	}
	return b.Path(a.Venue, p, a.To, over, shape, kfs)
}

func walkOp(b *market.Book, a req, p market.Pair) error {
	step := time.Minute
	if a.Step != "" {
		var err error
		if step, err = offset(a.Step); err != nil {
			return err
		}
	}
	if a.Seed == nil {
		return errors.New("walk needs a seed")
	}
	return b.Walk(a.Venue, p, market.WalkParams{Vol: a.Vol, Drift: a.Drift, Seed: *a.Seed, Step: step})
}

// replayOp reads the CSV from csv_inline or from csv_path, which must lie inside home.
func replayOp(home string) func(*market.Book, req, market.Pair) error {
	return func(b *market.Book, a req, p market.Pair) error {
		tf, err := offset(a.Timeframe)
		if err != nil {
			return err
		}
		var start time.Time
		if a.Start != "" {
			if start, err = market.ParseCSVTime(a.Start); err != nil {
				return errors.New("start must be RFC 3339 or a unix time")
			}
		}
		var rd io.Reader
		switch {
		case (a.CSVInline == "") == (a.CSVPath == ""):
			return errors.New("replay needs exactly one of csv_path and csv_inline")
		case a.CSVInline != "":
			rd = strings.NewReader(a.CSVInline)
		default:
			path := a.CSVPath
			if !filepath.IsAbs(path) {
				path = filepath.Join(home, path)
			}
			if !sandbox.PathInside(home, path) {
				return errors.New("csv_path must be inside the instance home")
			}
			f, err := os.Open(path)
			if err != nil {
				return errors.New("cannot read csv_path")
			}
			defer f.Close()
			rd = f
		}
		return b.Replay(a.Venue, p, rd, tf, start)
	}
}
