package triggers

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// The portfolio kinds (MR-212). They watch what the owner holds: a held asset moving
// unusually for that asset, the portfolio's drawdown, the allocation against the
// owner's targets, the distance of a futures position from liquidation, and a data
// source going quiet. They notify and never act: no candidate carries an action.
const (
	KindMoveVsVol           = "move_vs_vol"
	KindDrawdown            = "drawdown"
	KindAllocationDrift     = "allocation_drift"
	KindLiquidationDistance = "liquidation_distance"
	KindDataStale           = "data_stale"

	// PortfolioSpecOrigin is the origin of the builtin specs of these kinds.
	PortfolioSpecOrigin = "builtin:portfolio"
	// PortfolioSpecIDPrefix prefixes the ID of each builtin spec ("pf-move_vs_vol").
	PortfolioSpecIDPrefix = "pf-"
)

// PortfolioKinds are the kinds of this change, in a fixed order.
var PortfolioKinds = []string{KindMoveVsVol, KindDrawdown, KindAllocationDrift, KindLiquidationDistance, KindDataStale}

const (
	// defaultSnapshotEvery is the portfolio snapshot period when the gateway gives none.
	defaultSnapshotEvery = 15 * time.Minute
	// snapshotAgeFactor: a snapshot older than this many periods is not acted on.
	snapshotAgeFactor = 3
	// defaultFuturesMaxAge is how old a futures sample may be when it is judged: three
	// polls of an account with positions (every two minutes) and a minute of slack.
	defaultFuturesMaxAge = 7 * time.Minute

	// maxPortfolioParamBytes bounds a kind's parameters (they are small).
	maxTierList = 5
)

// PortfolioConfig is what the portfolio kinds are given by the gateway. Every func is
// cheap, does no I/O that can block the pipeline for long, and may be nil.
type PortfolioConfig struct {
	// Language returns "th" for Thai, anything else for English. Nil means English.
	Language func() string
	// Stats receives the counters. Nil counts nothing.
	Stats *PortfolioStats
	// SnapshotEvery is the period of the portfolio source. Zero means 15 minutes.
	SnapshotEvery time.Duration
	// Targets returns the owner's allocation targets in percent by class ("crypto",
	// "th_equity", "us_equity", "cash"). Nil or empty: allocation_drift judges nothing.
	Targets func() map[string]float64
	// FuturesAccounts returns the "provider/account" keys of the futures accounts to
	// judge. A tracked event's Diff does not say which account it is about, and the
	// view cannot list accounts, so the gateway names them. Nil: none.
	FuturesAccounts func() []string
	// StaleSources are the sources whose silence data_stale reports. Nil: none.
	StaleSources func() []StaleSource
	// MarketOpen says whether a market is open. Nil means calendar.IsOpen.
	MarketOpen func(m calendar.Market, t time.Time) bool
}

// StaleSource describes one perception source data_stale watches.
type StaleSource struct {
	// Name is the source's name in the world model ("portfolio", "futures").
	Name string
	// ExpectedEvery is how often it normally speaks. Zero: not watched.
	ExpectedEvery time.Duration
	// Market is the market that has to be open for its silence to matter.
	Market calendar.Market
	// Active says whether anything depends on the source right now (the futures
	// source matters only while a position is open). Nil means yes.
	Active func(v worldmodel.View) bool
}

// AnyFuturesPosition returns an Active func that is true while any of the accounts
// has an open position in the view. The futures source says nothing while an account
// is flat, so its silence matters only when there is something to watch.
func AnyFuturesPosition(accounts func() []string) func(v worldmodel.View) bool {
	return func(v worldmodel.View) bool {
		if accounts == nil {
			return false
		}
		for _, a := range accounts() {
			if ps, ok := v.FuturesPositions(a); ok && len(ps) > 0 {
				return true
			}
		}
		return false
	}
}

// Snapshot returns the counters by name, for the stats route: counts only.
func (s *PortfolioStats) Snapshot() map[string]uint64 {
	if s == nil {
		return nil
	}
	return map[string]uint64{
		"snapshot_stale": s.SnapshotStale.Load(), "snapshot_untrusted": s.SnapshotUntrusted.Load(),
		"snapshot_invalid": s.SnapshotInvalid.Load(), "snapshot_not_latest": s.SnapshotNotLatest.Load(),
		"move_no_reference": s.MoveNoReference.Load(), "move_floor_only": s.MoveFloorOnly.Load(),
		"move_market_closed": s.MoveMarketClosed.Load(), "move_unknown_class": s.MoveUnknownClass.Load(),
		"move_fired":           s.MoveFired.Load(),
		"drawdown_no_baseline": s.DrawdownNoBaseline.Load(), "drawdown_inconsistent": s.DrawdownInconsistent.Load(),
		"drawdown_source_degraded": s.DrawdownSourceDegraded.Load(), "drawdown_fired": s.DrawdownFired.Load(),
		"drawdown_rebased": s.DrawdownRebased.Load(), "rebase_notices": s.RebaseNotices.Load(),
		"drift_no_targets": s.DriftNoTargets.Load(), "drift_bad_targets": s.DriftBadTargets.Load(),
		"drift_fired": s.DriftFired.Load(),
		"liq_stale":   s.LiqStale.Load(), "liq_degraded": s.LiqDegraded.Load(), "liq_untrusted": s.LiqUntrusted.Load(),
		"liq_no_side": s.LiqNoSide.Load(), "liq_no_mark": s.LiqNoMark.Load(), "liq_no_liq_price": s.LiqNoLiqPrice.Load(),
		"liq_bad_margin": s.LiqBadMargin.Load(), "liq_evaluated": s.LiqEvaluated.Load(), "liq_fired": s.LiqFired.Load(),
		"stale_fired": s.StaleFired.Load(), "stale_recovered": s.StaleRecovered.Load(),
	}
}

// PortfolioStats count what the portfolio kinds did and did not act on, so that a
// silence has a number behind it. Counts only: no symbol, no amount.
type PortfolioStats struct {
	// Snapshot gates, shared by move_vs_vol, drawdown and allocation_drift.
	SnapshotStale, SnapshotUntrusted, SnapshotInvalid, SnapshotNotLatest atomic.Uint64

	// move_vs_vol.
	MoveNoReference, MoveFloorOnly, MoveMarketClosed, MoveUnknownClass, MoveFired atomic.Uint64
	// drawdown.
	DrawdownNoBaseline, DrawdownInconsistent, DrawdownSourceDegraded, DrawdownFired, DrawdownRebased, RebaseNotices atomic.Uint64
	// allocation_drift.
	DriftNoTargets, DriftBadTargets, DriftFired atomic.Uint64
	// liquidation_distance.
	LiqStale, LiqDegraded, LiqUntrusted, LiqNoSide, LiqNoMark, LiqNoLiqPrice, LiqBadMargin, LiqEvaluated, LiqFired atomic.Uint64
	// data_stale.
	StaleFired, StaleRecovered atomic.Uint64
}

func (c PortfolioConfig) stats() *PortfolioStats {
	if c.Stats == nil {
		return &PortfolioStats{}
	}
	return c.Stats
}

func (c PortfolioConfig) snapshotEvery() time.Duration {
	if c.SnapshotEvery <= 0 {
		return defaultSnapshotEvery
	}
	return c.SnapshotEvery
}

func (c PortfolioConfig) snapshotMaxAge() time.Duration { return snapshotAgeFactor * c.snapshotEvery() }

func (c PortfolioConfig) isThai() bool { return CrossConfig{Language: c.Language}.isThai() }

func (c PortfolioConfig) open(m calendar.Market, t time.Time) bool {
	if c.MarketOpen != nil {
		return c.MarketOpen(m, t)
	}
	return calendar.IsOpen(m, t)
}

// snapshotOf is the portfolio snapshot THIS event brought, when it can be acted on:
// the view's stamp equals the diff's time (a snapshot restored from a checkpoint keeps
// its old stamp, and so does a late one the model dropped), it is no older than three
// periods, came from an exchange-level source, and holds a positive finite total.
// Anything else changes nothing and is counted.
func (c PortfolioConfig) snapshotOf(v worldmodel.View, d worldmodel.Diff) (worldmodel.PortfolioView, bool) {
	pf, ok := v.Portfolio()
	if !ok || !pf.At.Equal(d.At) {
		c.stats().SnapshotNotLatest.Add(1)
		return pf, false
	}
	if !v.Fresh(pf.At, c.snapshotMaxAge()) {
		c.stats().SnapshotStale.Add(1)
		return pf, false
	}
	if pf.Trust < proactive.TrustExchange {
		c.stats().SnapshotUntrusted.Add(1)
		return pf, false
	}
	if !finite(pf.TotalUSD) || pf.TotalUSD <= 0 {
		c.stats().SnapshotInvalid.Add(1)
		return pf, false
	}
	return pf, true
}

// ---- parameters --------------------------------------------------------------

// MoveParams are the parameters of move_vs_vol. Floors are in percent.
type MoveParams struct {
	// K is the multiple of the asset's own hourly volatility a move has to exceed.
	K float64 `json:"k"`
	// FloorCryptoPct, FloorUSPct and FloorTHPct are the class floors: a move below
	// them is never reported however quiet the asset is.
	FloorCryptoPct float64 `json:"floor_crypto_pct"`
	FloorUSPct     float64 `json:"floor_us_pct"`
	FloorTHPct     float64 `json:"floor_th_pct"`
	// MinWeightPct and MinValueUSD select the holdings that are judged: weight at or
	// over the first, or value at or over the second.
	MinWeightPct float64 `json:"min_weight_pct"`
	MinValueUSD  float64 `json:"min_value_usd"`
	// ProtectiveMovePct and ProtectiveWeightPct: a move this large in a holding this
	// heavy is a protective Critical.
	ProtectiveMovePct   float64 `json:"protective_move_pct"`
	ProtectiveWeightPct float64 `json:"protective_weight_pct"`
	// MinSigmaReturns is how many hourly returns the asset's volatility needs before
	// it applies; until then only the class floor does.
	MinSigmaReturns int `json:"min_sigma_returns"`
	// AfterCloseMinutes: an equity is still judged this long after its market closed.
	AfterCloseMinutes int `json:"after_close_minutes"`
}

// DefaultMoveParams are the spec's defaults.
func DefaultMoveParams() MoveParams {
	return MoveParams{
		K: 3, FloorCryptoPct: 5, FloorUSPct: 3, FloorTHPct: 3, MinWeightPct: 1, MinValueUSD: 50,
		ProtectiveMovePct: 15, ProtectiveWeightPct: 10, MinSigmaReturns: 12, AfterCloseMinutes: 30,
	}
}

// Validate checks the ranges.
func (p MoveParams) Validate() error {
	in := func(name string, v, lo, hi float64) error {
		if !finite(v) || v < lo || v > hi {
			return fmt.Errorf("%s must be a number from %v to %v", name, lo, hi)
		}
		return nil
	}
	for _, c := range []error{
		in("k", p.K, 0.5, 20),
		in("floor_crypto_pct", p.FloorCryptoPct, 0.1, 100),
		in("floor_us_pct", p.FloorUSPct, 0.1, 100),
		in("floor_th_pct", p.FloorTHPct, 0.1, 100),
		in("min_weight_pct", p.MinWeightPct, 0, 100),
		in("min_value_usd", p.MinValueUSD, 0, 1e9),
		in("protective_move_pct", p.ProtectiveMovePct, 1, 100),
		in("protective_weight_pct", p.ProtectiveWeightPct, 0, 100),
		in("min_sigma_returns", float64(p.MinSigmaReturns), 4, 24),
		in("after_close_minutes", float64(p.AfterCloseMinutes), 0, 240),
	} {
		if c != nil {
			return c
		}
	}
	return nil
}

// DrawdownParams are the parameters of drawdown: the tiers, in percent below the
// day-start baseline and below the 30-day peak, ascending.
type DrawdownParams struct {
	DayTiers  []float64 `json:"day_tiers_pct"`
	PeakTiers []float64 `json:"peak_tiers_pct"`
	// PeakRepeatDays: a drawdown from the 30-day peak that persists is said on entering
	// a tier and on escalation, and otherwise again only after this many days. (The day
	// reference starts a new baseline every day and keeps its 24-hour window.)
	PeakRepeatDays int `json:"peak_repeat_days"`
	// RebaseNoticeCount: this many re-bases of the baseline in one owner-local day
	// raise the "unreliable today" notice.
	RebaseNoticeCount int `json:"rebase_notice_count"`
}

// DefaultDrawdownParams are the spec's defaults.
func DefaultDrawdownParams() DrawdownParams {
	return DrawdownParams{DayTiers: []float64{5, 10, 15}, PeakTiers: []float64{10, 20}, PeakRepeatDays: 7, RebaseNoticeCount: 3}
}

func validTiers(name string, ts []float64) error {
	if len(ts) == 0 || len(ts) > maxTierList {
		return fmt.Errorf("%s needs 1 to %d tiers", name, maxTierList)
	}
	prev := 0.0
	for _, t := range ts {
		if !finite(t) || t <= prev || t > 100 {
			return fmt.Errorf("%s must be ascending numbers above 0 and at most 100", name)
		}
		prev = t
	}
	return nil
}

// Validate checks the tiers.
func (p DrawdownParams) Validate() error {
	if err := validTiers("day_tiers_pct", p.DayTiers); err != nil {
		return err
	}
	if err := validTiers("peak_tiers_pct", p.PeakTiers); err != nil {
		return err
	}
	if p.PeakRepeatDays < 1 || p.PeakRepeatDays > 90 {
		return fmt.Errorf("peak_repeat_days must be from 1 to 90")
	}
	if p.RebaseNoticeCount < 2 || p.RebaseNoticeCount > 100 {
		return fmt.Errorf("rebase_notice_count must be from 2 to 100")
	}
	return nil
}

// DriftParams are the parameters of allocation_drift.
type DriftParams struct {
	// ThresholdPP is the gap, in percentage points, between a class's weight and its
	// target that is worth a line.
	ThresholdPP float64 `json:"threshold_pp"`
	// HysteresisPP: the gap has to fall this far under the threshold before the class
	// counts as back on target.
	HysteresisPP float64 `json:"hysteresis_pp"`
	// MinTotalUSD: a portfolio smaller than this is not judged.
	MinTotalUSD float64 `json:"min_total_usd"`
	// RepeatDays: a drift that persists is said again after this many days.
	RepeatDays int `json:"repeat_days"`
}

// DefaultDriftParams are the spec's defaults.
func DefaultDriftParams() DriftParams {
	return DriftParams{ThresholdPP: 10, HysteresisPP: 2, MinTotalUSD: 100, RepeatDays: 7}
}

// Validate checks the ranges.
func (p DriftParams) Validate() error {
	switch {
	case !finite(p.ThresholdPP) || p.ThresholdPP < 1 || p.ThresholdPP > 100:
		return fmt.Errorf("threshold_pp must be from 1 to 100")
	case !finite(p.HysteresisPP) || p.HysteresisPP < 0 || p.HysteresisPP >= p.ThresholdPP:
		return fmt.Errorf("hysteresis_pp must be from 0 up to threshold_pp")
	case !finite(p.MinTotalUSD) || p.MinTotalUSD < 0 || p.MinTotalUSD > 1e12:
		return fmt.Errorf("min_total_usd must be a non-negative number")
	case p.RepeatDays < 1 || p.RepeatDays > 90:
		return fmt.Errorf("repeat_days must be from 1 to 90")
	}
	return nil
}

// LiquidationParams are the parameters of liquidation_distance.
type LiquidationParams struct {
	// CriticalPct and WarningPct: a position closer to liquidation than this (percent
	// of the mark price) is Critical (protective) or Warning.
	CriticalPct float64 `json:"critical_pct"`
	WarningPct  float64 `json:"warning_pct"`
	// MarginRatioCriticalPct: a margin ratio over this is Critical.
	MarginRatioCriticalPct float64 `json:"margin_ratio_critical_pct"`
	// HysteresisPct: the distance has to recover this far past a boundary (and the
	// margin ratio fall this far under its line) before the tier steps down.
	HysteresisPct float64 `json:"hysteresis_pct"`
	// MaxAgeMinutes: how old a futures sample may be when it is judged.
	MaxAgeMinutes int `json:"max_age_minutes"`
	// WarningRepeatHours: a Warning that persists is said again after this long.
	WarningRepeatHours int `json:"warning_repeat_hours"`
}

// DefaultLiquidationParams are the spec's defaults.
func DefaultLiquidationParams() LiquidationParams {
	return LiquidationParams{
		CriticalPct: 15, WarningPct: 25, MarginRatioCriticalPct: 80, HysteresisPct: 2,
		MaxAgeMinutes: int(defaultFuturesMaxAge / time.Minute), WarningRepeatHours: 6,
	}
}

// Validate checks the ranges and the order of the tiers.
func (p LiquidationParams) Validate() error {
	switch {
	case !finite(p.CriticalPct) || p.CriticalPct <= 0 || p.CriticalPct >= 100:
		return fmt.Errorf("critical_pct must be above 0 and under 100")
	case !finite(p.WarningPct) || p.WarningPct <= p.CriticalPct || p.WarningPct >= 100:
		return fmt.Errorf("warning_pct must be above critical_pct and under 100")
	case !finite(p.MarginRatioCriticalPct) || p.MarginRatioCriticalPct <= 0 || p.MarginRatioCriticalPct > 100:
		return fmt.Errorf("margin_ratio_critical_pct must be above 0 and at most 100")
	case !finite(p.HysteresisPct) || p.HysteresisPct < 0 || p.HysteresisPct >= p.WarningPct-p.CriticalPct:
		return fmt.Errorf("hysteresis_pct must be from 0 up to warning_pct minus critical_pct")
	case p.MaxAgeMinutes < 3 || p.MaxAgeMinutes > 60:
		return fmt.Errorf("max_age_minutes must be from 3 to 60")
	case p.WarningRepeatHours < 1 || p.WarningRepeatHours > 72:
		return fmt.Errorf("warning_repeat_hours must be from 1 to 72")
	}
	return nil
}

// StaleParams are the parameters of data_stale.
type StaleParams struct {
	// Factor: a source is quiet when it has said nothing for this many times its
	// expected period.
	Factor float64 `json:"factor"`
}

// DefaultStaleParams are the spec's defaults.
func DefaultStaleParams() StaleParams { return StaleParams{Factor: 3} }

// Validate checks the range.
func (p StaleParams) Validate() error {
	if !finite(p.Factor) || p.Factor < 2 || p.Factor > 50 {
		return fmt.Errorf("factor must be a number from 2 to 50")
	}
	return nil
}

// decodeParams decodes a kind's parameters over its defaults (a field left out keeps
// its default) and validates the result. Empty parameters give the defaults.
func decodeParams[T interface{ Validate() error }](raw json.RawMessage, def T) (T, error) {
	p := def
	if len(strings.TrimSpace(string(raw))) > 0 && strings.TrimSpace(string(raw)) != "null" {
		if err := decodeStrict(raw, &p); err != nil {
			return def, err
		}
	}
	if err := p.Validate(); err != nil {
		return def, err
	}
	return p, nil
}

// ValidatePortfolioParams checks raw as the parameters of kind (one of PortfolioKinds)
// and returns them as the canonical JSON of the full, defaulted parameter set. The
// gateway uses it for the config's proactive.triggers.params.<kind>: on an error it
// logs a warning and uses the defaults, so an invalid block never disables the kind.
func ValidatePortfolioParams(kind string, raw json.RawMessage) (json.RawMessage, error) {
	var v any
	var err error
	switch kind {
	case KindMoveVsVol:
		v, err = decodeParams(raw, DefaultMoveParams())
	case KindDrawdown:
		v, err = decodeParams(raw, DefaultDrawdownParams())
	case KindAllocationDrift:
		v, err = decodeParams(raw, DefaultDriftParams())
	case KindLiquidationDistance:
		v, err = decodeParams(raw, DefaultLiquidationParams())
	case KindDataStale:
		v, err = decodeParams(raw, DefaultStaleParams())
	default:
		return nil, fmt.Errorf("%s is not a portfolio trigger kind", kind)
	}
	if err != nil {
		return nil, err
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		return nil, merr
	}
	return b, nil
}

// PortfolioFactories returns the factories of the five portfolio kinds.
func PortfolioFactories(cfg PortfolioConfig) map[string]Factory {
	cfg.Stats = cfg.stats()
	return map[string]Factory{
		KindMoveVsVol:           moveFactory(cfg),
		KindDrawdown:            drawdownFactory(cfg),
		KindAllocationDrift:     driftFactory(cfg),
		KindLiquidationDistance: liquidationFactory(cfg),
		KindDataStale:           staleFactory(cfg),
	}
}

// ---- shared helpers ------------------------------------------------------------

// ledger is the escalate-only memory of a situation: the highest tier said and when.
// Inside the window a tier is said only if it is higher than the last; once the window
// has passed the ledger is forgotten and the situation can be said again.
type ledgerEntry struct {
	Tier int   `json:"t"`
	AtMS int64 `json:"at"`
}

// escalates reports whether tier is worth saying given the entry for the situation:
// it is higher than the last tier said inside window, or nothing was said inside it.
func (e ledgerEntry) escalates(tier int, now time.Time, window time.Duration) bool {
	if tier <= 0 {
		return false
	}
	if e.AtMS == 0 || now.Sub(time.UnixMilli(e.AtMS)) >= window || now.Before(time.UnixMilli(e.AtMS)) {
		return true
	}
	return tier > e.Tier
}

// pruneLedger drops the entries whose window has passed.
func pruneLedger(m map[string]ledgerEntry, now time.Time, window time.Duration) {
	for k, e := range m {
		if now.Sub(time.UnixMilli(e.AtMS)) >= window {
			delete(m, k)
		}
	}
}

func marshalState(st *State, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if string(raw) != string(st.Raw) {
		st.Raw, st.Dirty = raw, true
	}
}

// baseAssetOf is the upper-case base asset of a symbol ("BTC/USDT:USDT" gives
// "BTC") or of a plain asset, when it is letters and digits; otherwise "".
func baseAssetOf(s string) string {
	base, _, _ := strings.Cut(s, "/")
	base = strings.ToUpper(strings.TrimSpace(base))
	if base == "" || len(base) > 20 {
		return ""
	}
	for i := 0; i < len(base); i++ {
		c := base[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return ""
		}
	}
	return base
}

// cleanKeyToken makes a name safe to sit between ':' in a dedup key.
func cleanKeyToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 64; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '/', c == '_', c == '-', c == '.', c == '@':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func clamp01(v float64) float64 {
	switch {
	case !finite(v) || v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

func pctOf(part, whole float64) float64 { return part / whole * 100 }

func usd(v float64) string { return fmtNumber(math.Round(v*100)/100) + " USD" }

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
