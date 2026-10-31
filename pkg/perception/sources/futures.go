package sources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const (
	// FuturesName is the futures source's name.
	FuturesName = "futures"

	defaultFuturesActiveEvery = 2 * time.Minute
	defaultFuturesFlatEvery   = 15 * time.Minute
	defaultFuturesCallTimeout = 30 * time.Second
	// defaultCatalogueTTL is how long a market catalogue (contract sizes) is
	// trusted: long enough to cost one request a few times a day, short enough that
	// a market listed later is found.
	defaultCatalogueTTL = 6 * time.Hour

	// defaultFuturesOpenFailCap bounds the backoff of an account that had open
	// positions when it started failing.
	defaultFuturesOpenFailCap = 5 * time.Minute
)

// FuturesPosition is one open futures position in a FuturesPositionsPayload. A
// number the exchange did not report is zero with its Known flag false: it is
// never a guess, and a consumer must read the flag before it acts on the number.
type FuturesPosition struct {
	Symbol string `json:"symbol"` // CCXT market symbol, "BTC/USDT:USDT"
	// Side is "long" or "short", or "" when the exchange did not say.
	Side string `json:"side"`

	// Contracts is the size of the position as a CONTRACT COUNT (always positive):
	// 135 contracts of CHZ/USDT:USDT on OKX is 1350 CHZ.
	Contracts float64 `json:"contracts"`
	// ContractSize is the base-currency size of one contract (OKX CHZ: 10, OKX
	// BTC: 0.01, Binance USDT-M: 1).
	ContractSize      float64 `json:"contract_size"`
	ContractSizeKnown bool    `json:"contract_size_known"`

	// MarkPrice and LiqPrice are in the contract's quote currency (USDT).
	MarkPrice      float64 `json:"mark_price"`
	MarkPriceKnown bool    `json:"mark_price_known"`
	LiqPrice       float64 `json:"liq_price"`
	LiqPriceKnown  bool    `json:"liq_price_known"`

	// MarginRatio is as the exchange reports it, a fraction (0.8 is 80% used).
	MarginRatio      float64 `json:"margin_ratio"`
	MarginRatioKnown bool    `json:"margin_ratio_known"`

	// UnrealizedPnl is in the settlement currency (USDT); it may be negative.
	UnrealizedPnl      float64 `json:"unrealized_pnl"`
	UnrealizedPnlKnown bool    `json:"unrealized_pnl_known"`

	// NotionalUSD is Contracts x ContractSize x MarkPrice, known only when the
	// contract size, the mark price and a USD-like quote are all known.
	NotionalUSD   float64 `json:"notional_usd"`
	NotionalKnown bool    `json:"notional_known"`
}

// FuturesPositionsPayload is the Payload of a perception.futures.positions event:
// every open position of one account at one moment. An empty Positions means the
// account is flat; it is sent when the account becomes flat, so that a consumer
// sees the close.
type FuturesPositionsPayload struct {
	Provider  string            `json:"provider"`
	Account   string            `json:"account"`
	At        time.Time         `json:"at"`
	Positions []FuturesPosition `json:"positions"`
}

// FuturesProviderFunc returns the futures provider of an account, or an error when
// it cannot be built. A provider that is not a broker.FuturesProvider yields
// ErrNoFutures.
type FuturesProviderFunc func(ctx context.Context, providerID, account string) (broker.FuturesProvider, error)

// ErrNoFutures says that an account's provider has no futures API.
var ErrNoFutures = errors.New("provider has no futures API")

// FuturesOptions configures a Futures source. Zero values take the defaults.
type FuturesOptions struct {
	Cfg      *config.Config
	Accounts []broker.AccountRef // default broker.ListConfiguredAccounts(Cfg)
	Provider FuturesProviderFunc // default broker.CreateProviderForAccount
	// Permit says whether an account may be read. Default: the account has the
	// trade scope (broker.CheckPermission), as the spec asks.
	Permit func(providerID, account string) error
	Clock  perception.Clock

	ActiveEvery  time.Duration // account with positions; default 2 minutes
	FlatEvery    time.Duration // account without; default 15 minutes
	CallTimeout  time.Duration // one account's reads; default 30 seconds
	CatalogueTTL time.Duration // default 6 hours
	OpenFailCap  time.Duration // longest backoff of an account that had positions; default 5 minutes
}

// FuturesStats are the source's counters since it was built.
type FuturesStats struct {
	Polls   uint64 // accounts polled
	Emitted uint64 // positions events emitted
	Failed  uint64 // polls that failed
	Dropped uint64 // samples dropped for a non-finite or negative number, or no symbol
	Skipped uint64 // accounts left out: no trade scope, or no futures API

	Degraded uint64 // degraded signals sent for an account that had positions and could not be read
}

// DefaultFuturesProvider builds an account's futures provider from the broker
// registry.
func DefaultFuturesProvider(cfg *config.Config) FuturesProviderFunc {
	return func(_ context.Context, providerID, account string) (broker.FuturesProvider, error) {
		p, err := broker.CreateProviderForAccount(providerID, account, cfg)
		if err != nil {
			return nil, err
		}
		fp, ok := p.(broker.FuturesProvider)
		if !ok {
			return nil, ErrNoFutures
		}
		return fp, nil
	}
}

// Futures polls the open futures positions of every account that may be read, and
// emits them for the liquidation and exposure triggers.
//
// An account with positions is read every ActiveEvery (2 minutes), one without
// every FlatEvery (15), and a failing one backs off the same way. Each read is one
// FetchFuturesPositions call (plus one market-catalogue load per CatalogueTTL when
// a position does not carry its contract size). It never places, cancels or changes
// an order, and it does not use the order rate limiter, which is the order budget.
type Futures struct {
	cfg      *config.Config
	accounts []broker.AccountRef
	provider FuturesProviderFunc
	permit   func(providerID, account string) error
	clock    perception.Clock

	active, flat, timeout, catalogueTTL, openFailCap time.Duration

	health healthTracker

	mu       sync.Mutex // serialises polls: Start's loop and Reconcile
	state    []*futuresAccount
	inflight perception.InFlight

	polls, emitted, failed, dropped, skipped, degraded atomic.Uint64
}

type futuresAccount struct {
	ref broker.AccountRef
	key string // "provider/account", for subjects and logs

	skip      bool
	checked   bool
	fp        broker.FuturesProvider
	resolver  *broker.ContractSizeResolver
	resolvedA time.Time

	due          time.Time
	flat         bool // the last sample was empty
	reportedFlat bool // an empty sample has been emitted since the last position
	hadPositions bool // the last good sample held open positions
	fails        int
}

// NewFutures builds the source.
func NewFutures(o FuturesOptions) *Futures {
	f := &Futures{
		cfg: o.Cfg, accounts: o.Accounts, provider: o.Provider, permit: o.Permit, clock: o.Clock,
		active: o.ActiveEvery, flat: o.FlatEvery, timeout: o.CallTimeout, catalogueTTL: o.CatalogueTTL,
		openFailCap: o.OpenFailCap,
	}
	if f.accounts == nil && o.Cfg != nil {
		f.accounts = broker.ListConfiguredAccounts(o.Cfg)
	}
	if f.provider == nil {
		f.provider = DefaultFuturesProvider(o.Cfg)
	}
	if f.permit == nil {
		cfg := o.Cfg
		f.permit = func(providerID, account string) error {
			return broker.CheckPermission(cfg, providerID, account, config.ScopeTrade)
		}
	}
	if f.clock == nil {
		f.clock = perception.RealClock{}
	}
	if f.active <= 0 {
		f.active = defaultFuturesActiveEvery
	}
	if f.flat <= 0 {
		f.flat = defaultFuturesFlatEvery
	}
	if f.timeout <= 0 {
		f.timeout = defaultFuturesCallTimeout
	}
	if f.openFailCap <= 0 {
		f.openFailCap = defaultFuturesOpenFailCap
	}
	if f.catalogueTTL <= 0 {
		f.catalogueTTL = defaultCatalogueTTL
	}
	for _, ref := range f.accounts {
		f.state = append(f.state, &futuresAccount{ref: ref, key: ref.ProviderID + "/" + ref.Account})
	}
	f.health.expected = f.active
	return f
}

// Name is "futures".
func (f *Futures) Name() string { return FuturesName }

// Health reports the failure streak and the last failure.
func (f *Futures) Health() perception.SourceHealth { return f.health.report() }

// Stats returns the counters.
func (f *Futures) Stats() FuturesStats {
	return FuturesStats{
		Polls: f.polls.Load(), Emitted: f.emitted.Load(), Failed: f.failed.Load(),
		Dropped: f.dropped.Load(), Skipped: f.skipped.Load(), Degraded: f.degraded.Load(),
	}
}

// Start polls every due account until ctx is done. A panic is the Manager's to
// recover.
func (f *Futures) Start(ctx context.Context, out perception.Emitter) error {
	for {
		next, anyPolled := f.pollDue(ctx, out)
		if ctx.Err() != nil {
			return nil
		}
		if !anyPolled {
			// Nothing may be polled: wait to be stopped rather than spin.
			<-ctx.Done()
			return nil
		}
		select {
		case <-f.clock.After(max(next.Sub(f.clock.Now()), 0)):
		case <-ctx.Done():
			return nil
		}
	}
}

// Idle is true when no account is being polled (perception.Idler). The next due poll is
// a timer, not busy.
func (f *Futures) Idle() bool { return f.inflight.Idle() }

// Reconcile reads every account now.
func (f *Futures) Reconcile(ctx context.Context, out perception.Emitter) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var errs []error
	for _, a := range f.state {
		if ctx.Err() != nil {
			break
		}
		if !f.admit(ctx, a) {
			continue
		}
		if err := f.poll(ctx, out, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pollDue polls the accounts that are due and returns the earliest next due time
// and whether any account can be polled at all.
func (f *Futures) pollDue(ctx context.Context, out perception.Emitter) (next time.Time, anyPolled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.state {
		if ctx.Err() != nil {
			return next, true
		}
		if !f.admit(ctx, a) {
			continue
		}
		anyPolled = true
		if !a.due.After(f.clock.Now()) {
			_ = f.poll(ctx, out, a)
		}
		if next.IsZero() || a.due.Before(next) {
			next = a.due
		}
	}
	return next, anyPolled
}

// admit decides once whether an account may be polled: it needs the trade scope
// and a provider with a futures API. A refusal is one INFO line and the account is
// left alone from then on. An error building the provider is not a refusal (it may
// pass): the account stays admitted and the poll reports it.
func (f *Futures) admit(ctx context.Context, a *futuresAccount) bool {
	if a.skip {
		return false
	}
	if a.checked {
		return true
	}
	if err := f.permit(a.ref.ProviderID, a.ref.Account); err != nil {
		a.skip, a.checked = true, true
		f.skipped.Add(1)
		logger.InfoCF("perception", "Futures positions not read: the account may not trade", map[string]any{"account": a.key})
		return false
	}
	fp, err := f.provider(ctx, a.ref.ProviderID, a.ref.Account)
	switch {
	case errors.Is(err, ErrNoFutures):
		a.skip, a.checked = true, true
		f.skipped.Add(1)
		logger.InfoCF("perception", "Futures positions not read: the provider has no futures API", map[string]any{"account": a.key})
		return false
	case err != nil:
		return true // checked stays false: the poll tries again and reports the error
	}
	a.fp, a.checked = fp, true
	return true
}

// poll reads one account and schedules its next read.
func (f *Futures) poll(ctx context.Context, out perception.Emitter, a *futuresAccount) error {
	defer f.inflight.Enter()()
	f.polls.Add(1)
	now := f.clock.Now()

	fp := a.fp
	if fp == nil {
		var err error
		if fp, err = f.provider(ctx, a.ref.ProviderID, a.ref.Account); err != nil {
			return f.pollFailed(out, a, now, "provider unavailable")
		}
		a.fp, a.checked = fp, true
	}

	cctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	positions, err := fp.FetchFuturesPositions(cctx, nil)
	if ctx.Err() != nil {
		return nil // stopping
	}
	if err != nil {
		logger.WarnCF("perception", "Futures positions read failed", map[string]any{"account": a.key, "error": err.Error()})
		return f.pollFailed(out, a, now, "positions read failed")
	}

	if a.resolver == nil || now.Sub(a.resolvedA) >= f.catalogueTTL {
		a.resolver, a.resolvedA = broker.NewContractSizeResolver(fp), now
	}
	rows, ok := f.rows(cctx, a, positions)
	if ctx.Err() != nil {
		return nil
	}
	if !ok {
		f.dropped.Add(1)
		logger.WarnCF("perception", "Futures sample dropped: a position has a non-finite or negative number, or no symbol",
			map[string]any{"account": a.key})
		return f.pollFailed(out, a, now, "sample dropped: bad number")
	}

	a.fails = 0
	f.health.ok()
	a.flat = len(rows) == 0
	a.hadPositions = !a.flat
	if a.flat {
		a.due = now.Add(f.flat)
		if a.reportedFlat {
			return nil // still flat, and the consumer already knows
		}
		a.reportedFlat = true
	} else {
		a.due = now.Add(f.active)
		a.reportedFlat = false
	}
	f.emitted.Add(1)
	out.EmitLatest(events.Event{
		Kind:     events.KindPerceptionFuturesPositions,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: FuturesName},
		Severity: events.SeverityInfo,
		Payload:  FuturesPositionsPayload{Provider: a.ref.ProviderID, Account: a.ref.Account, At: now, Positions: rows},
		Attrs:    map[string]any{"subject": a.key},
		Trust:    events.TrustPtr(proactive.TrustExchange),
	})
	return nil
}

// pollFailed records a failure and backs the account off: its normal interval,
// doubled for each consecutive failure, never beyond the flat interval, and for an
// account whose last good sample held positions never beyond OpenFailCap (5 minutes),
// because that is the account to look at again soon.
//
// An account that had open positions and cannot now be read is not allowed to go
// quiet: "positions open, exchange unreadable" is the worst time to be blind, so the
// pipeline is told (the same degraded signal the portfolio source sends, with the
// account's name and a fixed reason, no exchange text). It is sent on every failed
// poll; the engine coalesces by subject. A flat or never-read account stays
// log-and-health only.
func (f *Futures) pollFailed(out perception.Emitter, a *futuresAccount, now time.Time, msg string) error {
	f.failed.Add(1)
	f.health.fail(now, a.key+": "+msg)
	a.fails++
	base, limit := f.active, f.flat
	if a.flat {
		base = f.flat
	}
	if a.hadPositions {
		limit = min(limit, f.openFailCap)
	}
	a.due = now.Add(min(base<<min(a.fails, 6), max(limit, base)))
	if a.hadPositions {
		f.degraded.Add(1)
		out.EmitLatest(events.Event{
			Kind:     events.KindPerceptionSourceHealth,
			Time:     now,
			Source:   events.Source{Component: "perception", Name: FuturesName},
			Severity: events.SeverityWarn,
			Payload: SourceHealthPayload{
				Source: FuturesName, State: "degraded", FailedAccounts: []string{a.key},
				Reason: "futures positions unreadable; the last sample showed open positions",
			},
			Attrs: map[string]any{"subject": a.key},
			Trust: events.TrustPtr(proactive.TrustSystem),
		})
	}
	return fmt.Errorf("futures %s: %s", a.key, msg)
}

// rows converts the exchange's positions. ok is false when any position cannot be
// reported faithfully; the whole sample is then dropped, because a list with a
// position missing reads as one that was closed.
func (f *Futures) rows(ctx context.Context, a *futuresAccount, positions []ccxt.Position) (rows []FuturesPosition, ok bool) {
	rows = []FuturesPosition{}
	for _, p := range positions {
		if p.Contracts == nil || *p.Contracts == 0 {
			continue // flat
		}
		if p.Symbol == nil || strings.TrimSpace(*p.Symbol) == "" || !finite(*p.Contracts) {
			return nil, false
		}
		symbol := strings.TrimSpace(*p.Symbol)
		row := FuturesPosition{Symbol: symbol, Contracts: abs(*p.Contracts)}

		if p.Side != nil {
			switch s := strings.ToLower(strings.TrimSpace(*p.Side)); s {
			case "long", "short":
				row.Side = s
			}
		}
		if row.Side == "" && *p.Contracts < 0 {
			row.Side = "short"
		}
		if p.MarkPrice != nil {
			if !nonNegative(*p.MarkPrice) {
				return nil, false
			}
			row.MarkPrice, row.MarkPriceKnown = *p.MarkPrice, *p.MarkPrice > 0
		}
		if p.LiquidationPrice != nil {
			if !nonNegative(*p.LiquidationPrice) {
				return nil, false
			}
			row.LiqPrice, row.LiqPriceKnown = *p.LiquidationPrice, *p.LiquidationPrice > 0 // some venues send 0 for "none"
		}
		if p.MarginRatio != nil {
			if !nonNegative(*p.MarginRatio) {
				return nil, false
			}
			row.MarginRatio, row.MarginRatioKnown = *p.MarginRatio, true
		}
		if p.UnrealizedPnl != nil {
			if !finite(*p.UnrealizedPnl) {
				return nil, false
			}
			row.UnrealizedPnl, row.UnrealizedPnlKnown = *p.UnrealizedPnl, true
		}
		if cs, found := a.resolver.Size(ctx, p, symbol); found && finite(cs) && cs > 0 {
			row.ContractSize, row.ContractSizeKnown = cs, true
		}
		if row.ContractSizeKnown && row.MarkPriceKnown && usdLikeContract(symbol) {
			if n := broker.PositionNotionalUSD(row.Contracts, row.ContractSize, row.MarkPrice); n > 0 && finite(n) {
				row.NotionalUSD, row.NotionalKnown = n, true
			}
		}
		rows = append(rows, row)
	}
	return rows, true
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// usdLikeContract reports whether a CCXT contract symbol ("BTC/USDT:USDT") is quoted
// and settled in a USD-like currency, so that Contracts x size x mark is USD. An
// inverse contract ("BTC/USD:BTC") is not.
func usdLikeContract(symbol string) bool {
	slash := strings.IndexByte(symbol, '/')
	colon := strings.IndexByte(symbol, ':')
	if slash < 0 || colon < slash {
		return false
	}
	quote, settle := symbol[slash+1:colon], symbol[colon+1:]
	if i := strings.IndexByte(settle, '-'); i >= 0 { // dated futures: "BTC/USDT:USDT-260925"
		settle = settle[:i]
	}
	return usdLike(quote) && usdLike(settle)
}

func usdLike(c string) bool {
	switch strings.ToUpper(c) {
	case "USD", "USDT", "USDC", "BUSD", "FDUSD", "TUSD", "DAI", "USDP":
		return true
	}
	return false
}
