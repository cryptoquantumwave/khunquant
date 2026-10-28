package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// startHindsight starts the daily hindsight job (feedback.Service.StartHindsight) when
// the recorder runs and there is a market source to read prices through. It adds no
// exchange client: prices come from the perception market source's own reader, its
// cache and its read budget. Portfolio totals come from the automatic snapshots, read
// only. Without a market source (perception off) there is nothing to measure against
// and the job does not start. Its snapshot handle is closed by stop.
func (rt *feedbackRuntime) startHindsight(d assemblyDeps, pr *perceptionRuntime) {
	if pr == nil || pr.market == nil {
		logger.InfoCF("feedback", "Hindsight is off: there is no market data source", nil)
		return
	}
	accounts := broker.ListConfiguredAccounts(d.Cfg)
	opts := feedback.HindsightOptions{Prices: marketPrices{src: pr.market, accounts: accounts}, Clock: d.Shared.clk()}
	if len(accounts) > 0 {
		if store, err := snapshot.NewStore(agent.DefaultAgentWorkspace(d.Cfg)); err != nil {
			logger.WarnCF("feedback", "Hindsight will not measure the portfolio: the snapshot store could not be opened",
				map[string]any{"error": err.Error()})
		} else {
			rt.snapshots = store
			opts.Portfolio = snapshotValuations{store: store}
		}
	}
	rt.svc.StartHindsight(opts)
}

// marketPrices reads 1h bars through the market source.
type marketPrices struct {
	src      *sources.MarketSource
	accounts []broker.AccountRef
}

// HourlyBars implements feedback.PriceSource. The alert's own provider and account are
// tried when it named them; otherwise each configured account in turn (the first that
// may read market data and has the symbol answers). A read the source declined for now
// is reported as feedback.ErrDeferred, so that the job tries again tomorrow rather than
// recording nothing.
func (m marketPrices) HourlyBars(ctx context.Context, provider, account, symbol string, limit int) ([]feedback.Bar, error) {
	var tries []broker.AccountRef
	for _, a := range m.accounts {
		if (provider == "" || a.ProviderID == provider) && (account == "" || a.Account == account) {
			tries = append(tries, a)
		}
	}
	lastErr, deferred := errors.New("no configured account to read through"), false
	for _, a := range tries {
		bars, err := m.src.ReadBars(ctx, a.ProviderID, a.Account, symbol, "1h", limit)
		if err == nil && len(bars) > 0 {
			out := make([]feedback.Bar, len(bars))
			for i, b := range bars {
				out[i] = feedback.Bar{Open: time.UnixMilli(b.OpenTime), Close: b.Close}
			}
			return out, nil
		}
		if err == nil {
			err = errors.New("no bars")
		}
		lastErr = err
		deferred = deferred || errors.Is(err, sources.ErrReadBudget) || errors.Is(err, sources.ErrBackedOff) ||
			errors.Is(err, sources.ErrRateLimited) || ctx.Err() != nil
	}
	if deferred {
		return nil, fmt.Errorf("%w: %v", feedback.ErrDeferred, lastErr)
	}
	return nil, lastErr
}

// snapshotValuations reads automatic snapshots, and nothing else, from the snapshot
// store. It has no way to take one.
type snapshotValuations struct{ store *snapshot.Store }

// AutoValuation implements feedback.PortfolioSource: the latest automatic snapshot taken
// in [at-within, at], with the total in its own quote.
func (v snapshotValuations) AutoValuation(ctx context.Context, at time.Time, within time.Duration) (feedback.Valuation, bool, error) {
	until, since := at.UTC(), at.Add(-within).UTC()
	snaps, err := v.store.QuerySnapshots(ctx, snapshot.QueryFilter{Label: snapshot.LabelAuto, Since: &since, Until: &until, Limit: 5})
	if err != nil {
		return feedback.Valuation{}, false, err
	}
	for _, s := range snaps { // newest first
		if snapshot.IsAuto(s.Label, s.Note) && !s.TakenAt.After(at) {
			return feedback.Valuation{Total: s.TotalValue, Quote: s.Quote, At: s.TakenAt}, true, nil
		}
	}
	return feedback.Valuation{}, false, nil
}
