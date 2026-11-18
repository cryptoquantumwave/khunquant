package api

import (
	"net/http"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// This file serves the sub-system settings on the Proactive page: perception,
// triggers, approvals, cognition and news. It follows the modes route exactly:
// strict decode, load -> change ONLY cfg.Proactive -> validate -> config.SaveConfig
// under proactive.cfgMu, then the new status. The gateway reloads config.json by
// itself (it polls the file every 2 s and rebuilds its services), so a saved
// change is live in a few seconds with no gateway restart.
//
// Nothing here carries a credential: every field is a switch, a number, the
// triggers mode or a model NAME. The route can reach no other config section
// (DisallowUnknownFields) and never returns one.

// Codes in proactiveSubsystemsView.Notes. The page turns each into a sentence in
// the owner's language; the server only says which requirement is unmet.
const (
	noteProactiveOff        = "proactive_off"
	noteNewsNeedsCognition  = "news_needs_cognition"
	noteNewsNeedsTriggers   = "news_needs_triggers"
	noteNewsNeedsWebSearch  = "news_needs_web_search"
	noteTriggersNeedPercept = "triggers_need_perception"
)

type perceptionView struct {
	Enabled             bool `json:"enabled"`
	MarketPollSeconds   int  `json:"market_poll_seconds"`
	ReadsPerMinuteLimit int  `json:"reads_per_minute_limit"`
	SnapshotMinutes     int  `json:"snapshot_minutes"`
}

type triggersView struct {
	Mode string `json:"mode"`
}

type approvalsView struct {
	Enabled       bool `json:"enabled"`
	ExpiryMinutes int  `json:"expiry_minutes"`
}

type cognitionView struct {
	Enabled          bool   `json:"enabled"`
	MaxConcurrentLLM int    `json:"max_concurrent_llm"`
	WakesPerHour     int    `json:"wakes_per_hour"`
	TokensPerDay     int    `json:"tokens_per_day"`
	ProactiveModel   string `json:"proactive_model"`
}

type newsView struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	MaxAssets       int  `json:"max_assets"`
	MaxParsesPerRun int  `json:"max_parses_per_run"`
	MaxParsesPerDay int  `json:"max_parses_per_day"`
	RunSeconds      int  `json:"run_seconds"`
}

type subsystemsConfiguredView struct {
	Perception perceptionView `json:"perception"`
	Triggers   triggersView   `json:"triggers"`
	Approvals  approvalsView  `json:"approvals"`
	Cognition  cognitionView  `json:"cognition"`
	News       newsView       `json:"news"`
}

// subsystemsEffectiveView is what Effective() applies: each switch AND-ed with the
// master switch (and news with cognition), and the triggers mode as downgraded.
type subsystemsEffectiveView struct {
	Perception   bool   `json:"perception"`
	TriggersMode string `json:"triggers_mode"`
	Approvals    bool   `json:"approvals"`
	Cognition    bool   `json:"cognition"`
	News         bool   `json:"news"`
}

// subsystemsLimitsView carries the two limits the page shows next to a field, so the
// numbers live in the config package only. Every other bound is enforced by the
// route and quoted back in its error.
type subsystemsLimitsView struct {
	NewsMinIntervalMinutes int `json:"news_min_interval_minutes"`
	NewsMaxAssetsCap       int `json:"news_max_assets_cap"`
}

type proactiveSubsystemsView struct {
	Configured subsystemsConfiguredView `json:"configured"`
	Effective  subsystemsEffectiveView  `json:"effective"`
	Limits     subsystemsLimitsView     `json:"limits"`
	// Notes lists unmet requirements, as codes, for a switch the owner turned on.
	Notes []string `json:"notes"`
}

func buildSubsystemsView(p config.ProactiveConfig) proactiveSubsystemsView {
	eff := p.Effective()
	v := proactiveSubsystemsView{
		Configured: subsystemsConfiguredView{
			Perception: perceptionView{
				Enabled:             p.Perception.Enabled,
				MarketPollSeconds:   p.Perception.MarketPollSeconds,
				ReadsPerMinuteLimit: p.Perception.ReadsPerMinuteLimit,
				SnapshotMinutes:     p.Perception.SnapshotMinutes,
			},
			Triggers:  triggersView{Mode: modeOr(p.Triggers.Mode, config.ProactiveModeOff)},
			Approvals: approvalsView{Enabled: p.Approvals.Enabled, ExpiryMinutes: p.Approvals.ExpiryMinutes},
			Cognition: cognitionView{
				Enabled:          p.Cognition.Enabled,
				MaxConcurrentLLM: p.Cognition.MaxConcurrentLLM,
				WakesPerHour:     p.Cognition.WakesPerHour,
				TokensPerDay:     p.Cognition.TokensPerDay,
				ProactiveModel:   p.Cognition.ProactiveModel,
			},
			News: newsView{
				Enabled:         p.News.Enabled,
				IntervalMinutes: p.News.IntervalMinutes,
				MaxAssets:       p.News.MaxAssets,
				MaxParsesPerRun: p.News.MaxParsesPerRun,
				MaxParsesPerDay: p.News.MaxParsesPerDay,
				RunSeconds:      p.News.RunSeconds,
			},
		},
		Effective: subsystemsEffectiveView{
			Perception:   eff.Perception,
			TriggersMode: eff.TriggersMode,
			Approvals:    eff.Approvals,
			Cognition:    eff.Cognition,
			News:         eff.News,
		},
		Limits: subsystemsLimitsView{
			NewsMinIntervalMinutes: config.NewsMinIntervalMinutes,
			NewsMaxAssetsCap:       config.NewsMaxAssetsCap,
		},
		Notes: []string{},
	}

	anyOn := p.Perception.Enabled || p.Approvals.Enabled || p.Cognition.Enabled || p.News.Enabled ||
		v.Configured.Triggers.Mode != config.ProactiveModeOff
	if !p.Enabled && anyOn {
		v.Notes = append(v.Notes, noteProactiveOff)
	}
	// Only requirements the master switch is not already the reason for.
	if p.Enabled {
		if p.News.Enabled && !eff.Cognition {
			v.Notes = append(v.Notes, noteNewsNeedsCognition)
		}
		if p.News.Enabled && eff.TriggersMode == config.ProactiveModeOff {
			v.Notes = append(v.Notes, noteNewsNeedsTriggers)
		}
		if eff.TriggersMode != config.ProactiveModeOff && !eff.Perception {
			v.Notes = append(v.Notes, noteTriggersNeedPercept)
		}
	}
	return v
}

// proactiveStatusFor is the status the proactive routes answer: the part that
// depends on the proactive section alone, plus what needs the rest of the config
// (the web-search requirement of news, the paper-trading flag). It reads those two
// as booleans and returns nothing else from outside the proactive section.
func proactiveStatusFor(cfg *config.Config) proactiveStatusResponse {
	resp := buildProactiveStatus(cfg.Proactive)
	resp.PaperTradingMode = cfg.TradingRisk.PaperTradingMode
	if cfg.Proactive.Enabled && cfg.Proactive.News.Enabled && !cfg.WebSearchAvailable() {
		resp.Subsystems.Notes = append(resp.Subsystems.Notes, noteNewsNeedsWebSearch)
	}
	return resp
}

// subsystemsRequest is every setting the route may change. A nil group or field is
// left as it is. Decoded with DisallowUnknownFields.
type subsystemsRequest struct {
	Perception *struct {
		Enabled             *bool `json:"enabled"`
		MarketPollSeconds   *int  `json:"market_poll_seconds"`
		ReadsPerMinuteLimit *int  `json:"reads_per_minute_limit"`
		SnapshotMinutes     *int  `json:"snapshot_minutes"`
	} `json:"perception"`
	Triggers *struct {
		Mode *string `json:"mode"`
	} `json:"triggers"`
	Approvals *struct {
		Enabled       *bool `json:"enabled"`
		ExpiryMinutes *int  `json:"expiry_minutes"`
	} `json:"approvals"`
	Cognition *struct {
		Enabled          *bool   `json:"enabled"`
		MaxConcurrentLLM *int    `json:"max_concurrent_llm"`
		WakesPerHour     *int    `json:"wakes_per_hour"`
		TokensPerDay     *int    `json:"tokens_per_day"`
		ProactiveModel   *string `json:"proactive_model"`
	} `json:"cognition"`
	News *struct {
		Enabled         *bool `json:"enabled"`
		IntervalMinutes *int  `json:"interval_minutes"`
		MaxAssets       *int  `json:"max_assets"`
		MaxParsesPerRun *int  `json:"max_parses_per_run"`
		MaxParsesPerDay *int  `json:"max_parses_per_day"`
		RunSeconds      *int  `json:"run_seconds"`
	} `json:"news"`
}

// setInt copies *src into both the section being changed and the probe the edit
// checks run on, and reports whether src was given.
func setInt(src *int, dst ...*int) bool {
	if src == nil {
		return false
	}
	for _, d := range dst {
		*d = *src
	}
	return true
}

func setBool(dst *bool, src *bool) bool {
	if src == nil {
		return false
	}
	*dst = *src
	return true
}

// apply merges the request into c and reports how many fields it set. A group's
// edit checks (the config package's own: floors, caps, model-name rules) run on a
// probe made of the shipped defaults overlaid with only the fields the request
// names, so a value already in the file that is out of range (written by hand)
// does not block an unrelated save, and the message names only what was sent.
func (r *subsystemsRequest) apply(c *config.ProactiveConfig) (int, error) {
	n := 0
	probe := config.DefaultProactiveConfig()
	var problems []error
	count := func(set ...bool) {
		for _, s := range set {
			if s {
				n++
			}
		}
	}

	if g := r.Perception; g != nil {
		p := &probe.Perception
		count(setBool(&c.Perception.Enabled, g.Enabled),
			setInt(g.MarketPollSeconds, &c.Perception.MarketPollSeconds, &p.MarketPollSeconds),
			setInt(g.ReadsPerMinuteLimit, &c.Perception.ReadsPerMinuteLimit, &p.ReadsPerMinuteLimit),
			setInt(g.SnapshotMinutes, &c.Perception.SnapshotMinutes, &p.SnapshotMinutes))
		if g.MarketPollSeconds != nil || g.ReadsPerMinuteLimit != nil || g.SnapshotMinutes != nil {
			problems = append(problems, config.ValidatePerceptionEdit(*p))
		}
	}
	if g := r.Triggers; g != nil && g.Mode != nil {
		mode := strings.ToLower(strings.TrimSpace(*g.Mode))
		if mode == "" {
			return 0, inputErrorf("triggers.mode must not be empty")
		}
		c.Triggers.Mode = mode // ValidateProactive judges the value
		count(true)
	}
	if g := r.Approvals; g != nil {
		p := &probe.Approvals
		count(setBool(&c.Approvals.Enabled, g.Enabled),
			setInt(g.ExpiryMinutes, &c.Approvals.ExpiryMinutes, &p.ExpiryMinutes))
		if g.ExpiryMinutes != nil {
			problems = append(problems, config.ValidateApprovalsEdit(*p))
		}
	}
	if g := r.Cognition; g != nil {
		p := &probe.Cognition
		count(setBool(&c.Cognition.Enabled, g.Enabled),
			setInt(g.MaxConcurrentLLM, &c.Cognition.MaxConcurrentLLM, &p.MaxConcurrentLLM),
			setInt(g.WakesPerHour, &c.Cognition.WakesPerHour, &p.WakesPerHour),
			setInt(g.TokensPerDay, &c.Cognition.TokensPerDay, &p.TokensPerDay))
		if g.ProactiveModel != nil {
			c.Cognition.ProactiveModel = strings.TrimSpace(*g.ProactiveModel)
			p.ProactiveModel = c.Cognition.ProactiveModel // inner spaces are still refused
			count(true)
		}
		if g.MaxConcurrentLLM != nil || g.WakesPerHour != nil || g.TokensPerDay != nil || g.ProactiveModel != nil {
			problems = append(problems, config.ValidateCognitionEdit(*p))
		}
	}
	if g := r.News; g != nil {
		p := &probe.News
		count(setBool(&c.News.Enabled, g.Enabled),
			setInt(g.IntervalMinutes, &c.News.IntervalMinutes, &p.IntervalMinutes),
			setInt(g.MaxAssets, &c.News.MaxAssets, &p.MaxAssets),
			setInt(g.MaxParsesPerRun, &c.News.MaxParsesPerRun, &p.MaxParsesPerRun),
			setInt(g.MaxParsesPerDay, &c.News.MaxParsesPerDay, &p.MaxParsesPerDay),
			setInt(g.RunSeconds, &c.News.RunSeconds, &p.RunSeconds))
		if g.IntervalMinutes != nil || g.MaxAssets != nil || g.MaxParsesPerRun != nil ||
			g.MaxParsesPerDay != nil || g.RunSeconds != nil {
			problems = append(problems, config.ValidateNewsEdit(*p))
		}
	}

	var msgs []string
	for _, e := range problems {
		if e != nil {
			msgs = append(msgs, strings.ReplaceAll(e.Error(), "\n", "; "))
		}
	}
	if len(msgs) > 0 {
		return 0, inputErrorf("settings not saved: %s", strings.Join(msgs, "; "))
	}
	return n, nil
}

func (h *Handler) handleProactiveSubsystemsPut(w http.ResponseWriter, r *http.Request) {
	if !allowStateChange(w, r) {
		return
	}
	var req subsystemsRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := h.updateProactiveConfig(func(c *config.ProactiveConfig) error {
		merged := *c
		n, err := req.apply(&merged)
		if err != nil {
			return err
		}
		if n == 0 {
			return inputErrorf("nothing to change: send at least one field of perception, triggers, approvals, cognition or news")
		}
		if err := config.ValidateProactive(merged); err != nil {
			return inputErrorf("settings not saved: %v", err)
		}
		*c = merged
		return nil
	})
	if err != nil {
		writeProactiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proactiveStatusFor(cfg))
}
