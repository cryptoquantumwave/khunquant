package sources

import (
	"context"
	"errors"
	"fmt"
)

// Errors ReadBars returns when it declines to read; neither says anything about the
// symbol, only that the source will not read now.
var (
	// ErrReadBudget: the provider's read budget is spent, or down to the half ReadBars
	// leaves for the poll.
	ErrReadBudget = errors.New("market: read budget spent")
	// ErrBackedOff: the exchange pushed back and the source is leaving it alone.
	ErrBackedOff = errors.New("market: provider is backed off")
)

// ReadBars reads the newest limit bars of one series once, for a caller that is not a
// trigger (the hindsight job in pkg/feedback). It goes through exactly what the poll
// goes through: the reader the gateway resolves for (provider, account), which is the
// read-only, cached view with the account's market_data permission checked; the
// provider's read budget (one token a read, never the trading limiter, and never the
// half the poll needs for the prices triggers watch); and the provider's backoff, which
// it respects but never starts.
// since is not passed, so the broker cache can answer. The bars are cleaned like the
// poll's (a bad number or time drops the bar), oldest first, the newest possibly still
// forming. It sets up no series and emits no event.
func (s *MarketSource) ReadBars(ctx context.Context, provider, account, symbol, timeframe string, limit int) ([]MarketBar, error) {
	if s == nil || s.readers == nil {
		return nil, ErrUnsupported
	}
	limit = min(max(limit, 1), maxFetchBars)
	s.mu.Lock()
	backedOff := s.clock.Now().Before(s.providerLocked(provider).until)
	s.mu.Unlock()
	if backedOff {
		return nil, ErrBackedOff
	}
	reader, err := s.readers.Reader(provider, account)
	if err != nil {
		s.noteSkip(provider, account, err)
		return nil, err
	}
	// A caller that is not a trigger leaves half the budget for the poll, which reads the
	// prices the protective triggers watch.
	if s.limiter.Left(provider) <= s.limiter.perMin/2 || !s.limiter.Allow(provider) {
		s.skipLimiter.Add(1)
		return nil, ErrReadBudget
	}
	s.requests.Add(1)
	s.ohlcvReqs.Add(1)
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	data, err := reader.FetchOHLCV(cctx, symbol, timeframe, nil, limit)
	cancel()
	if err != nil {
		s.errCount.Add(1)
		if isRateLimitErr(err) {
			s.rateLimited.Add(1)
			err = fmt.Errorf("%w: %w", ErrRateLimited, err)
		}
		return nil, fmt.Errorf("market: read bars: %w", err)
	}
	bars, bad := cleanBars(data)
	s.droppedBad.Add(uint64(bad))
	return bars, nil
}
