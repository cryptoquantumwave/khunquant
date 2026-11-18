package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/reflection"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// This file wires the portfolio triggers (move_vs_vol, drawdown, allocation_drift,
// liquidation_distance, data_stale). They read the world model only; the gateway gives
// them the few facts the model cannot: the owner's language and allocation targets,
// the configured futures accounts, and the cadence of the sources whose silence
// data_stale reports. They notify and never act.

const (
	// portfolioTargetsTTL is how long the owner's allocation targets are cached: the
	// profile is read from the local database at most this often, on the first
	// portfolio snapshot after it.
	portfolioTargetsTTL     = 5 * time.Minute
	portfolioTargetsTimeout = 2 * time.Second
	// futuresActiveEvery is how often the futures source reads an account that has
	// positions.
	futuresActiveEvery = 2 * time.Minute
)

// portfolioShared returns the process-wide counters of the portfolio triggers.
func (r *sharedResources) portfolioShared() *triggers.PortfolioStats {
	if r == nil {
		return &triggers.PortfolioStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.portfolioStats == nil {
		r.portfolioStats = &triggers.PortfolioStats{}
	}
	return r.portfolioStats
}

// portfolioTriggerFactories are the five portfolio kinds for this generation.
func portfolioTriggerFactories(d assemblyDeps) map[string]triggers.Factory {
	return triggers.PortfolioFactories(portfolioConfig(d))
}

func portfolioConfig(d assemblyDeps) triggers.PortfolioConfig {
	cfg := triggers.PortfolioConfig{
		Language: ownerLanguage(d),
		Stats:    d.Shared.portfolioShared(),
		Targets:  ownerTargets(d),
	}
	if d.Cfg == nil {
		return cfg
	}
	cfg.SnapshotEvery = time.Duration(d.Cfg.Proactive.Perception.SnapshotMinutes) * time.Minute
	accounts := futuresAccountKeys(d.Cfg)
	cfg.FuturesAccounts = func() []string { return accounts }
	cfg.StaleSources = func() []triggers.StaleSource {
		// The market source reads for any price alert or watch whether or not an account is
		// named here, so it is listed on its own: the demands decide whether it matters.
		// It speaks once per poll (a price alert never asks for less than
		// PriceCrossPollInterval) and is Active only while a crypto price is demanded. It
		// also reads equity venues; their market hours are not crypto's and the world model
		// keeps one health record for the whole source, so only crypto demands make it
		// Active.
		out := []triggers.StaleSource{{
			Name: "market", ExpectedEvery: marketExpectedEvery(d.Cfg), Market: calendar.Crypto,
			Active: triggers.AnyCryptoMarketDemand(demandsOf(d)),
			// The source reports a provider degraded once, after sources.HealthAfterFailures
			// failed polls, then backs off and says nothing more until the provider
			// answers: one report already is an outage, and a second never comes.
			UnreadableAfter: 1,
		}}
		if len(accounts) == 0 {
			return out // no exchange account: neither money source exists
		}
		return append(out,
			triggers.StaleSource{Name: "portfolio", ExpectedEvery: cfg.SnapshotEvery, Market: calendar.Crypto},
			triggers.StaleSource{
				Name: "futures", ExpectedEvery: futuresActiveEvery, Market: calendar.Crypto,
				Active: triggers.AnyFuturesPosition(cfg.FuturesAccounts),
			})
	}
	return cfg
}

// marketExpectedEvery is how often the market source normally speaks for a price alert: its
// poll interval, or the interval the alerts ask for when that is longer (the source never
// reads a series more often than either).
func marketExpectedEvery(cfg *config.Config) time.Duration {
	poll := time.Duration(cfg.Proactive.Perception.MarketPollSeconds) * time.Second
	if poll <= 0 {
		poll = sources.DefaultMarketPoll
	}
	return max(poll, triggers.PriceCrossPollInterval)
}

// lateDemands is the demand source of the triggers registry, handed out before the
// registry exists (the factories that need it are built first). It answers with no
// demands until set.
type lateDemands struct{ src atomic.Pointer[perception.DemandSource] }

func (l *lateDemands) set(s perception.DemandSource) { l.src.Store(&s) }

// Demands implements perception.DemandSource.
func (l *lateDemands) Demands() []perception.Demand {
	if p := l.src.Load(); p != nil && *p != nil {
		return (*p).Demands()
	}
	return nil
}

// demandsOf is the demand source portfolioConfig reads: the registry's, once it exists.
func demandsOf(d assemblyDeps) perception.DemandSource {
	if d.Demands != nil {
		return d.Demands
	}
	if d.lateDemands != nil {
		return d.lateDemands
	}
	return nil
}

// futuresAccountKeys are the "provider/account" keys the futures source reads (it
// reads every configured account that has the futures API and the trade scope; an
// account it skips never produces an event, so naming it here costs nothing).
func futuresAccountKeys(cfg *config.Config) []string {
	var keys []string
	for _, a := range broker.ListConfiguredAccounts(cfg) {
		keys = append(keys, a.ProviderID+"/"+a.Account)
	}
	return keys
}

// ownerTargets returns the owner's allocation targets from the profile, read from the
// local database at most every portfolioTargetsTTL. Any failure gives no targets (the
// drift trigger then judges nothing). The profile changes by the owner's own hand, so a
// change is seen within the TTL; the read is one small local query, bounded by a
// timeout, made on the first snapshot after the TTL.
func ownerTargets(d assemblyDeps) func() map[string]float64 {
	var (
		mu     sync.Mutex
		store  *intent.Store
		opened bool
		at     time.Time
		cached map[string]float64
	)
	return func() map[string]float64 {
		mu.Lock()
		defer mu.Unlock()
		now := d.nowFn()()
		if !at.IsZero() && now.Sub(at) < portfolioTargetsTTL {
			return cached
		}
		at = now
		cached = nil
		if d.Cfg == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), portfolioTargetsTimeout)
		defer cancel()
		if !opened {
			opened = true
			if db, err := proactiveDB(d); err == nil {
				store, _ = openIntentStore(ctx, d, db)
			}
		}
		if store == nil {
			return nil
		}
		p, err := store.Profile(ctx)
		if err != nil {
			return nil
		}
		cached = p.AllocationTargets
		return cached
	}
}

// ensurePortfolioSpecs makes the registry hold one builtin spec per portfolio kind this
// build can run, with the parameters the config asks for (proactive.triggers.params.
// <kind>) over the kind's defaults. A block that does not validate is logged and the
// kind runs on its defaults: an invalid block never switches an alert off and never
// crashes. It is idempotent: a spec whose canonical parameters already equal the
// wanted ones is left alone (an update would clear its state and re-fire its tiers
// on every reload), and a mode the owner set on it is kept.
//
// New specs are live in their own mode; the global triggers mode caps them (shadow by
// default), so they deliver only when the owner has made triggers and attention live.
func ensurePortfolioSpecs(ctx context.Context, reg *triggers.Registry, cfg *config.Config, have map[string]triggers.Factory) {
	ensurePortfolioSpecsCounted(ctx, reg, cfg, have, nil)
}

// ensurePortfolioSpecsCounted is ensurePortfolioSpecs that also reports, in stats (nil is
// fine), how many specs it left alone because an approved reflection tuned them.
func ensurePortfolioSpecsCounted(ctx context.Context, reg *triggers.Registry, cfg *config.Config, have map[string]triggers.Factory, stats *triggers.PortfolioStats) {
	var tuned uint64
	defer func() {
		if stats != nil {
			stats.SpecsReflectionTuned.Store(tuned)
		}
	}()
	for _, kind := range triggers.PortfolioKinds {
		if have[kind] == nil {
			continue
		}
		want := wantedPortfolioParams(kind, cfg)
		id := triggers.PortfolioSpecIDPrefix + kind
		cur, err := reg.List(ctx, triggers.ListFilter{Kind: kind, Origin: triggers.PortfolioSpecOrigin})
		if err != nil {
			continue
		}
		var existing *triggers.Spec
		for i := range cur {
			if cur[i].ID == id {
				existing = &cur[i]
			}
		}
		switch {
		case existing == nil:
			_, err = reg.Add(ctx, triggers.Spec{
				ID: id, Kind: kind, Mode: triggers.ModeLive, Origin: triggers.PortfolioSpecOrigin, Params: want,
			})
		case strings.HasSuffix(existing.Origin, reflection.ProvenanceSuffix):
			// The owner approved a reflection's change to its parameters: not put back.
			tuned++
			logger.InfoCF("perception", "A portfolio trigger keeps the parameters an approved reflection set; "+
				"proactive.triggers.params does not override them", map[string]any{"trigger": existing.ID})
			continue
		default:
			if got, cerr := triggers.ValidatePortfolioParams(kind, existing.Params); cerr == nil && bytes.Equal(got, want) &&
				bytes.Equal(existing.Params, want) {
				continue
			}
			next := *existing
			next.Params = want
			err = reg.Update(ctx, next)
		}
		if err != nil {
			logger.WarnCF("perception", "A portfolio trigger could not be registered",
				map[string]any{"kind": kind, "error": err.Error()})
		}
	}
}

// wantedPortfolioParams are the canonical parameters of kind: the config's block over
// the defaults, or the defaults alone when the block is absent or does not validate.
func wantedPortfolioParams(kind string, cfg *config.Config) json.RawMessage {
	var raw json.RawMessage
	if cfg != nil {
		raw = cfg.Proactive.Triggers.Params[kind]
	}
	if len(raw) > 0 {
		p, err := triggers.ValidatePortfolioParams(kind, raw)
		if err == nil {
			return p
		}
		logger.WarnCF("perception", "proactive.triggers.params block ignored; the defaults apply",
			map[string]any{"kind": kind, "error": err.Error()})
	}
	p, _ := triggers.ValidatePortfolioParams(kind, nil)
	return p
}
