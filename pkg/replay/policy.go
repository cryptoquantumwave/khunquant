package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// CurrentPolicy is the name that stands for the build's own defaults.
const CurrentPolicy = "current"

// Policy is what a replay judges the events with: the triggers, the attention rules
// and the owner's profile. "current" is the build's defaults (the five portfolio
// kinds with their default parameters, the default attention configuration and the
// default profile); a file replaces it entirely, so a change under test is a file
// that differs from another in the line that changed.
//
// Nothing here is read from the owner's configuration or databases.
type Policy struct {
	// BuiltinPortfolio adds the five portfolio kinds as the gateway does, with their
	// default parameters.
	BuiltinPortfolio bool `json:"builtin_portfolio,omitempty"`
	// Specs are the trigger specs to install. An empty Mode is live (the replay's
	// global triggers mode is live as well); an empty ID is numbered in order.
	Specs []SpecDef `json:"specs,omitempty"`
	// Attention replaces the default attention configuration (budget, dedup window,
	// critical repeat). Its mode is ignored: attention is always live in a replay.
	Attention *config.AttentionConfig `json:"attention,omitempty"`
	// Profile replaces the default owner profile (time zone, active hours, budget).
	Profile *intent.Profile `json:"profile,omitempty"`
	// Language is "th" for Thai messages; anything else is English.
	Language string `json:"language,omitempty"`
	// SnapshotMinutes is the portfolio source's period (default 15).
	SnapshotMinutes int `json:"snapshot_minutes,omitempty"`
	// FuturesAccounts are the "provider/account" keys liquidation_distance judges.
	FuturesAccounts []string `json:"futures_accounts,omitempty"`
	// StaleSources are the sources data_stale watches.
	StaleSources []StaleSpec `json:"stale_sources,omitempty"`
}

// SpecDef is a trigger spec as a policy file writes it.
type SpecDef struct {
	ID     string          `json:"id,omitempty"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Origin string          `json:"origin,omitempty"`
}

// StaleSpec is one source data_stale watches: how often it normally speaks, and
// whether its silence matters only while a futures position is open.
type StaleSpec struct {
	Name                 string `json:"name"`
	ExpectedEverySeconds int    `json:"expected_every_seconds"`
	Market               string `json:"market,omitempty"`
	OnlyWithFuturesOpen  bool   `json:"only_with_futures_open,omitempty"`
}

// LoadPolicy reads a policy: CurrentPolicy, or a JSON file (unknown fields refused,
// so a typo is not a silent no-op).
func LoadPolicy(name string) (Policy, error) {
	if name == "" || name == CurrentPolicy {
		return Policy{BuiltinPortfolio: true}, nil
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return Policy{}, fmt.Errorf("replay: policy: %w", err)
	}
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("replay: policy %s: %w", name, err)
	}
	return p, nil
}

// specs are the specs to install, the builtin portfolio set first.
func (p Policy) specs() ([]triggers.Spec, error) {
	var out []triggers.Spec
	if p.BuiltinPortfolio {
		for _, kind := range triggers.PortfolioKinds {
			params, err := triggers.ValidatePortfolioParams(kind, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, triggers.Spec{
				ID: triggers.PortfolioSpecIDPrefix + kind, Kind: kind, Mode: triggers.ModeLive,
				Origin: triggers.PortfolioSpecOrigin, Params: params,
			})
		}
	}
	for i, d := range p.Specs {
		s := triggers.Spec{ID: d.ID, Kind: d.Kind, Mode: d.Mode, Params: d.Params, Origin: d.Origin}
		if s.ID == "" {
			s.ID = fmt.Sprintf("spec-%d", i+1)
		}
		if s.Mode == "" {
			s.Mode = triggers.ModeLive
		}
		if s.Origin == "" {
			s.Origin = "replay"
		}
		out = append(out, s)
	}
	return out, nil
}

func (p Policy) attention() config.AttentionConfig {
	a := config.DefaultConfig().Proactive.Attention
	if p.Attention != nil {
		a = *p.Attention
	}
	a.Mode = config.ProactiveModeLive
	return a
}

func (p Policy) profile() intent.Profile {
	if p.Profile != nil {
		return *p.Profile
	}
	return intent.DefaultProfile()
}

func (p Policy) lang() func() string { return func() string { return p.Language } }

// factories are the trigger kinds a replay can run: the ones that read the world model
// alone. A kind that needs the owner's databases or a source that is not recorded (dca_due,
// time_at, news_relevant, external, goal_discrepancy) is unknown here, and a spec of it is
// refused with the registry's "unknown trigger kind".
func (p Policy) factories() map[string]triggers.Factory {
	cross := triggers.CrossConfig{Language: p.lang()}
	every := time.Duration(p.SnapshotMinutes) * time.Minute
	accounts := append([]string(nil), p.FuturesAccounts...)
	pcfg := triggers.PortfolioConfig{
		Language: p.lang(), SnapshotEvery: every,
		FuturesAccounts: func() []string { return accounts },
		Targets:         func() map[string]float64 { return p.profile().AllocationTargets },
	}
	if len(p.StaleSources) > 0 {
		pcfg.StaleSources = func() []triggers.StaleSource {
			out := make([]triggers.StaleSource, 0, len(p.StaleSources))
			for _, s := range p.StaleSources {
				ss := triggers.StaleSource{
					Name: s.Name, ExpectedEvery: time.Duration(s.ExpectedEverySeconds) * time.Second,
					Market: calendar.Market(strings.ToLower(s.Market)),
				}
				if s.OnlyWithFuturesOpen {
					ss.Active = triggers.AnyFuturesPosition(pcfg.FuturesAccounts)
				}
				out = append(out, ss)
			}
			return out
		}
	}
	f := triggers.PortfolioFactories(pcfg)
	f[triggers.KindPriceCross] = triggers.PriceCrossFactory(cross)
	f[triggers.KindIndicatorCross] = triggers.IndicatorCrossFactory(cross)
	f[triggers.KindDNHealth] = triggers.DNHealthFactory(triggers.DNHealthConfig{Language: p.lang()})
	return f
}
