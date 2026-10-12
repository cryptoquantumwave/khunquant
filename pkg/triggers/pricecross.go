package triggers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// PriceCrossParams are the parameters of a price_cross spec: "tell me when SYMBOL
// goes above/below THRESHOLD", once per crossing.
//
// HysteresisPct (default 0.3) is how far back past the threshold, in percent of
// it, the price has to move before the alert can arm again. CooldownMinutes
// (default 60) is the least time between two firings of a recurring alert.
// FireIfAlready (default true) fires once on the first fresh observation when the
// price is already on the target side, which is what the cron alerts did. Message is
// the user's own text: outside data, cleaned and capped wherever it is used.
type PriceCrossParams struct {
	Provider      string   `json:"provider"`
	Account       string   `json:"account,omitempty"`
	Symbol        string   `json:"symbol"`
	Direction     string   `json:"direction"`
	Threshold     float64  `json:"threshold"`
	HysteresisPct *float64 `json:"hysteresis_pct,omitempty"`
	Recurring     bool     `json:"recurring"`
	CooldownMin   int      `json:"cooldown_minutes,omitempty"`
	FireIfAlready *bool    `json:"fire_if_already,omitempty"`
	Message       string   `json:"message,omitempty"`
}

// Validate checks the parameters. The factory calls it, and so can a tool that
// wants to refuse early with the same words.
func (p PriceCrossParams) Validate() error {
	switch {
	case !validAlertToken(p.Provider, false):
		return fmt.Errorf("provider must be 1-%d letters, digits, '_', '-', '.' or '@'", maxAlertTokenLen)
	case !validAlertToken(p.Account, true):
		return fmt.Errorf("account must be at most %d letters, digits, '_', '-', '.' or '@'", maxAlertTokenLen)
	case !validAlertSymbol(p.Symbol):
		return fmt.Errorf("symbol must be 1-%d letters, digits or '/', ':', '_', '-', '.'", maxAlertTokenLen)
	case p.Direction != DirAbove && p.Direction != DirBelow:
		return fmt.Errorf("direction must be %q or %q", DirAbove, DirBelow)
	case !finite(p.Threshold) || p.Threshold <= 0:
		return fmt.Errorf("threshold must be a positive number")
	case p.HysteresisPct != nil && (!finite(*p.HysteresisPct) || *p.HysteresisPct < 0 || *p.HysteresisPct > maxHysteresisPct):
		return fmt.Errorf("hysteresis_pct must be from 0 to %d", maxHysteresisPct)
	case p.CooldownMin < 0 || p.CooldownMin > maxCooldownMin:
		return fmt.Errorf("cooldown_minutes must be from 1 to %d", maxCooldownMin)
	}
	return nil
}

func (p PriceCrossParams) hysteresisPct() float64 {
	if p.HysteresisPct == nil {
		return defaultHysteresisPct
	}
	return *p.HysteresisPct
}

func (p PriceCrossParams) fireIfAlready() bool { return p.FireIfAlready == nil || *p.FireIfAlready }

// JSON encodes the parameters for a Spec.
func (p PriceCrossParams) JSON() json.RawMessage {
	b, _ := json.Marshal(p) // plain fields: cannot fail
	return b
}

// PriceCrossFactory builds price_cross triggers.
func PriceCrossFactory(cfg CrossConfig) Factory {
	cfg = cfg.withStats()
	return func(spec Spec) (Trigger, error) {
		var p PriceCrossParams
		if err := decodeStrict(spec.Params, &p); err != nil {
			return nil, err
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return &priceCross{
			id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg,
			line: newLine(p.Direction, p.Threshold, p.Threshold*p.hysteresisPct()/100, cooldownOrDefault(p.CooldownMin), p.fireIfAlready(), p.Recurring),
		}, nil
	}
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parameters: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("parameters: trailing data")
	}
	return nil
}

type priceCross struct {
	id   string
	p    PriceCrossParams
	cfg  CrossConfig
	line line
}

func (t *priceCross) ID() string { return t.id }

func (t *priceCross) Kinds() []events.Kind { return []events.Kind{events.KindPerceptionMarketTick} }

// Demands: the ticker of the symbol at the poll interval. The registry merges equal
// demands of all the alerts into one.
func (t *priceCross) Demands() []perception.Demand {
	return []perception.Demand{{
		Provider: t.p.Provider, Account: t.p.Account, Symbol: t.p.Symbol,
		Kind: perception.DemandTicker, MinInterval: PriceCrossPollInterval,
	}}
}

// Evaluate judges the alert on the tick that was just applied and on nothing else.
//
// The price must be the one THIS event set: the view's stamp for it equals the
// diff's time. A price the model restored from a checkpoint keeps its old stamp and
// is therefore never acted on, and neither is the price of another symbol's tick
// (the registry routes every tick here). It must also be no older than priceMaxAge
// (three polls) by the view's clock, come from an exchange (trust at least
// TrustExchange) and be a positive finite number. Anything else changes no state and
// fires nothing; the first observation that passes is the "first observation" of
// fire_if_already.
func (t *priceCross) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	price, at, ok := v.Price(t.p.Provider, t.p.Symbol)
	if !ok || !at.Equal(d.At) {
		return nil, nil
	}
	if !v.Fresh(at, priceMaxAge) {
		t.cfg.Stats.PriceStale.Add(1)
		return nil, nil
	}
	if !finite(price) || price <= 0 {
		t.cfg.Stats.PriceInvalid.Add(1)
		return nil, nil
	}
	trust, ok := v.PriceTrust(t.p.Provider, t.p.Symbol)
	if !ok || trust < proactive.TrustExchange {
		t.cfg.Stats.PriceUntrusted.Add(1)
		return nil, nil
	}

	cs := loadCross(st)
	fired := t.line.step(&cs, price, v.Now())
	saveCross(st, cs, fired, t.p.Recurring)
	if !fired {
		return nil, nil
	}
	t.cfg.Stats.Fired.Add(1)
	return []proactive.Candidate{t.candidate(price, trust, v)}, nil
}

func (t *priceCross) candidate(price float64, trust proactive.Trust, v worldmodel.View) proactive.Candidate {
	thai := t.cfg.isThai()
	thr, now := fmtThreshold(t.p.Threshold), fmtNumber(price)
	msg := CleanAlertMessage(t.p.Message)
	ev := map[string]any{
		"price": price, "threshold": t.p.Threshold, "direction": t.p.Direction,
		"provider": t.p.Provider, "symbol": t.p.Symbol, "message": msg,
	}
	return crossCandidate(
		fmt.Sprintf("%s:%s:%s:%s:%s", KindPriceCross, t.p.Provider, t.p.Symbol, t.p.Direction, thr),
		t.p.Symbol,
		crossTitle(thai, t.p.Symbol, t.p.Direction == DirAbove, thr, now),
		crossDetail(thai, t.p.Provider, thr, now), msg,
		ev, trust, t.p.CooldownMin, v.Now())
}
