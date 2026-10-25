package deltaneutral

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Read-only views of the exchange. The monitor never places, cancels or changes an
// order: the resolver hands it whole providers, and it type-asserts each to the few
// read methods below, so nothing it holds could trade even by mistake.
type (
	futuresReader interface {
		FetchFuturesPositions(ctx context.Context, symbols []string) ([]ccxt.Position, error)
		FetchFuturesMarkPrice(ctx context.Context, symbol string) (float64, error)
		LoadFuturesMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error)
		FetchFuturesFundingRate(ctx context.Context, symbol string) (ccxt.FundingRate, error)
		FetchPublicFundingRateHistory(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingRateHistory, error)
	}
	tickerReader interface {
		FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error)
	}
	balanceReader interface {
		GetBalances(ctx context.Context) ([]broker.Balance, error)
	}
	earnReader interface {
		FetchFlexibleEarnPositions(ctx context.Context) ([]broker.EarnPosition, error)
		FetchFlexibleEarnProducts(ctx context.Context, asset string) ([]broker.EarnProduct, error)
		FetchFlexibleEarnRateHistory(ctx context.Context, productID, asset string, since *int64, limit int) ([]broker.EarnRatePoint, error)
	}
)

// ProviderResolver returns the provider of an account: whatever the broker registry
// builds for (providerID, account). The monitor uses only its read methods.
type ProviderResolver interface {
	Provider(providerID, account string) (any, error)
}

// ProviderResolverFunc adapts a function to ProviderResolver.
type ProviderResolverFunc func(providerID, account string) (any, error)

// Provider calls f.
func (f ProviderResolverFunc) Provider(providerID, account string) (any, error) {
	return f(providerID, account)
}

// BrokerResolver resolves providers through the broker registry with cfg, the way
// every other part of the gateway does.
func BrokerResolver(cfg *config.Config) ProviderResolver {
	return ProviderResolverFunc(func(providerID, account string) (any, error) {
		return broker.CreateProviderForAccount(providerID, account, cfg)
	})
}

// Monitor reads the live state of both legs of a plan and evaluates its health. It
// is the deterministic core of the delta-neutral monitor, shared by the legacy cron
// job and by the plan-health source: it fetches, then calls Evaluate, and does
// nothing else (it saves nothing and sends nothing).
type Monitor struct {
	// Providers resolves the spot and futures providers of a plan.
	Providers ProviderResolver
	// Clock is the monitor's clock (UTC is applied); nil means time.Now.
	Clock func() time.Time

	// Booked, when set, returns what the plan has booked so far in funding received
	// and fees paid, in USDT (positive is income). With it the monitor can judge the
	// plan's profit and loss against its profit target and drawdown limit; without
	// it the profit and loss is unknown and neither is judged.
	Booked func(ctx context.Context, planID int64) (usdt float64, ok bool)

	// HistoryTTL, when positive, keeps the long histories a check reads (a year of
	// funding rates, a year of earn rates) for that long and reuses them, instead of
	// paging through the exchange on every check. The legacy cron job leaves it zero
	// and reads them every run, as it always did; the plan-health source, which checks
	// every few minutes, sets it. The windows are recomputed from the kept points on
	// every check, so only the newest settlement can be up to HistoryTTL late.
	HistoryTTL time.Duration

	mu    sync.Mutex
	cache map[string]cachedHistory
}

// maxCachedHistories bounds the histories a Monitor keeps.
const maxCachedHistories = 64

type cachedHistory struct {
	at      time.Time
	funding []ccxt.FundingRateHistory
	earn    []broker.EarnRatePoint
}

// cached returns the kept history under key, if it is younger than HistoryTTL.
func (m *Monitor) cached(key string, now time.Time) (cachedHistory, bool) {
	if m.HistoryTTL <= 0 {
		return cachedHistory{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.cache[key]
	if !ok || now.Sub(h.at) >= m.HistoryTTL || now.Before(h.at) {
		return cachedHistory{}, false
	}
	return h, true
}

func (m *Monitor) keep(key string, h cachedHistory) {
	if m.HistoryTTL <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		m.cache = map[string]cachedHistory{}
	}
	if _, exists := m.cache[key]; !exists && len(m.cache) >= maxCachedHistories {
		oldest, oldestAt := "", h.at
		for k, v := range m.cache {
			if oldest == "" || v.at.Before(oldestAt) {
				oldest, oldestAt = k, v.at
			}
		}
		delete(m.cache, oldest)
	}
	m.cache[key] = h
}

// MonitorResult is one check: the evaluation and the snapshot to persist (the
// evaluation's own, completed with the yield figures and stamped).
type MonitorResult struct {
	Eval     HealthEvaluation
	Snapshot MonitorSnapshot
}

// ErrNoProviders is returned by Check on a Monitor with no resolver.
var ErrNoProviders = errors.New("deltaneutral: monitor has no provider resolver")

// Monitorable reports whether a plan is to be checked now, and if not why (the
// reason is a short fixed phrase). A disabled plan, a plan not yet opened (draft,
// ready) and one permanently closed are skipped: a prepared plan has no legs on the
// exchange, so a check would report 100% delta drift. A failed plan normally holds no
// position and is skipped too, but one that still has open exposure stays monitored
// (unwind skips failed plans, so the monitor is the only thing watching it); if the
// exposure check itself errors, it is monitored.
func Monitorable(ctx context.Context, plan *Plan, exposure ExposureChecker) (bool, string) {
	if !plan.Enabled {
		return false, "plan disabled"
	}
	switch plan.Status {
	case PlanStatusClosed, PlanStatusArchived, PlanStatusDraft, PlanStatusReady:
		return false, "plan status not monitorable"
	case PlanStatusFailed:
		hasExposure, err := exposure.HasOpenExposure(ctx, plan.ID)
		if err != nil {
			logger.WarnCF("dn-monitor", "Could not check open exposure of failed plan; monitoring it anyway", map[string]any{
				"plan_id": plan.ID, "error": err.Error(),
			})
		} else if !hasExposure {
			return false, "plan status not monitorable"
		}
	}
	return true, ""
}

// ExposureChecker says whether a plan still holds a position. *Store implements it.
type ExposureChecker interface {
	HasOpenExposure(ctx context.Context, planID int64) (bool, error)
}

// Check reads both legs of plan and evaluates its health. It returns an error only
// when the monitor cannot run at all; a leg that cannot be read is part of the result
// (a data_unavailable breach), not an error.
func (m *Monitor) Check(ctx context.Context, plan Plan) (MonitorResult, error) {
	if m == nil || m.Providers == nil {
		return MonitorResult{}, ErrNoProviders
	}
	planID := plan.ID
	clock := m.Clock
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()

	// Fetch live state for futures leg.
	futuresState := FuturesState{}
	var futureFundingRate *ccxt.FundingRate      // Store funding rate if available.
	var fundingHistory []ccxt.FundingRateHistory // Trailing funding history for windowed averages.
	futureFetcher, err := m.Providers.Provider(plan.FuturesProvider, plan.FuturesAccount)
	if err != nil {
		logger.WarnCF("dn-monitor", "Failed to create futures provider", map[string]any{
			"plan_id": planID, "provider": plan.FuturesProvider, "error": err.Error(),
		})
		futuresState.Available = false
	} else {
		futuresProv, ok := futureFetcher.(futuresReader)
		if !ok {
			logger.WarnCF("dn-monitor", "Provider does not support futures", map[string]any{
				"plan_id": planID, "provider": plan.FuturesProvider,
			})
			futuresState.Available = false
		} else {
			// Fetch positions.
			positions, err := futuresProv.FetchFuturesPositions(ctx, []string{plan.FuturesSymbol})
			if err != nil {
				logger.WarnCF("dn-monitor", "Failed to fetch futures positions", map[string]any{
					"plan_id": planID, "error": err.Error(),
				})
				futuresState.Available = false
			} else {
				futuresState.Available = true
				// Extract position data: the position of this plan's symbol and side. (The
				// monitor used to take positions[0], whatever it was: on an account that
				// holds the opposite side too, or another contract, it watched the wrong one.)
				if pos, found := matchPosition(positions, plan); found {
					if pos.Contracts != nil {
						futuresState.Contracts = *pos.Contracts
					}
					if pos.ContractSize != nil && *pos.ContractSize > 0 {
						futuresState.ContractSize = *pos.ContractSize
					}
					if pos.MarkPrice != nil && pos.ContractSize != nil {
						futuresState.NotionalUSDT = *pos.ContractSize * futuresState.Contracts * *pos.MarkPrice
					}
					if pos.UnrealizedPnl != nil {
						futuresState.UnrealizedPnLUSDT = *pos.UnrealizedPnl
					}
					if pos.LiquidationPrice != nil {
						futuresState.LiquidationPrice = *pos.LiquidationPrice
					}
					if pos.MarginRatio != nil {
						futuresState.MarginRatioPct = *pos.MarginRatio * 100 // Convert to percentage
					}
				}
			}

			// Fetch mark price (also gets latest mark price).
			markPrice, err := futuresProv.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
			if err != nil {
				logger.WarnCF("dn-monitor", "Failed to fetch futures mark price", map[string]any{
					"plan_id": planID, "error": err.Error(),
				})
				futuresState.Available = false
			} else {
				futuresState.MarkPrice = markPrice
			}

			// OKX (and some other exchanges) do not include ContractSize in the
			// position response, leaving NotionalUSDT = 0. Recompute from market
			// metadata when contracts and mark price are available.
			if futuresState.NotionalUSDT == 0 && futuresState.Contracts > 0 && futuresState.MarkPrice > 0 {
				if markets, mktErr := futuresProv.LoadFuturesMarkets(ctx); mktErr == nil {
					if mkt, ok := markets[plan.FuturesSymbol]; ok {
						if mkt.ContractSize != nil && *mkt.ContractSize > 0 {
							futuresState.NotionalUSDT = futuresState.Contracts * *mkt.ContractSize * futuresState.MarkPrice
							futuresState.ContractSize = *mkt.ContractSize
						}
					}
				}
			}

			// Fetch funding rate.
			fr, err := futuresProv.FetchFuturesFundingRate(ctx, plan.FuturesSymbol)
			if err != nil {
				logger.WarnCF("dn-monitor", "Failed to fetch funding rate", map[string]any{
					"plan_id": planID, "error": err.Error(),
				})
				// Don't set Available=false here; funding info is handled separately.
			} else {
				futureFundingRate = &fr
			}

			// Fetch trailing funding history (≥1y) for windowed combined-APY averages.
			// limit>100 enables ccxt pagination; ~1100 records cover 365d at 8h cadence
			// (3/day → ~366d). Non-fatal: leaves fundingHistory nil when unavailable.
			fundingKey := "funding|" + plan.FuturesProvider + "|" + plan.FuturesAccount + "|" + plan.FuturesSymbol
			if h, ok := m.cached(fundingKey, now); ok {
				fundingHistory = h.funding
			} else if hist, histErr := futuresProv.FetchPublicFundingRateHistory(ctx, plan.FuturesSymbol, nil, 1100); histErr == nil {
				fundingHistory = hist
				m.keep(fundingKey, cachedHistory{at: now, funding: hist})
			} else {
				logger.WarnCF("dn-monitor", "Failed to fetch funding history (windows)", map[string]any{
					"plan_id": planID, "error": histErr.Error(),
				})
			}
		}
	}

	// earnAPYPct holds the best flexible-earn APY (%) for the base asset.
	// Set inside the earn-provider block below; 0 when unavailable.
	earnAPYPct := 0.0
	// Trailing earn (flexible-savings) window averages (%) and their point counts;
	// captured inside the earn-provider block below. 0 when unavailable.
	var earn90dPct, earn180dPct, earn365dPct float64
	var earn90dN, earn180dN, earn365dN int
	var bestEarnProductID, bestEarnProductType string

	// Fetch live state for spot leg.
	spotState := SpotState{}
	spotFetcher, err := m.Providers.Provider(plan.SpotProvider, plan.SpotAccount)
	if err != nil {
		logger.WarnCF("dn-monitor", "Failed to create spot provider", map[string]any{
			"plan_id": planID, "provider": plan.SpotProvider, "error": err.Error(),
		})
		spotState.Available = false
	} else {
		spotMD, ok := spotFetcher.(tickerReader)
		if !ok {
			logger.WarnCF("dn-monitor", "Provider does not support market data", map[string]any{
				"plan_id": planID, "provider": plan.SpotProvider,
			})
			spotState.Available = false
		} else {
			ticker, err := spotMD.FetchTicker(ctx, plan.SpotSymbol)
			if err != nil {
				logger.WarnCF("dn-monitor", "Failed to fetch spot ticker", map[string]any{
					"plan_id": planID, "error": err.Error(),
				})
				spotState.Available = false
			} else {
				spotState.Available = true
				if ticker.Last != nil {
					spotState.Price = *ticker.Last
				}

				// Parse base currency from spot symbol: "CHZ/USDT" → "CHZ"
				baseCur := plan.SpotSymbol
				if idx := strings.Index(plan.SpotSymbol, "/"); idx > 0 {
					baseCur = plan.SpotSymbol[:idx]
				}

				totalQty := 0.0
				fetchedAny := false

				// 1. Real spot / trading-account balance.
				if pp, ok := spotFetcher.(balanceReader); ok {
					if balances, balErr := pp.GetBalances(ctx); balErr == nil {
						for _, b := range balances {
							if b.Asset == baseCur {
								totalQty += b.Free
								fetchedAny = true
							}
						}
					} else {
						logger.WarnCF("dn-monitor", "Failed to fetch spot balances", map[string]any{
							"plan_id": planID, "error": balErr.Error(),
						})
					}
				}

				// 2. Earn / savings balance for the same asset.
				// OKX and Binance both implement EarnProvider; CHZ in Flexible Earn still
				// backs the spot leg of the hedge, so it must count toward spotState.
				if ep, ok := spotFetcher.(earnReader); ok {
					if positions, epErr := ep.FetchFlexibleEarnPositions(ctx); epErr == nil {
						for _, p := range positions {
							if p.Asset == baseCur {
								totalQty += p.Amount
								fetchedAny = true
							}
						}
					} else {
						logger.WarnCF("dn-monitor", "Failed to fetch earn positions", map[string]any{
							"plan_id": planID, "error": epErr.Error(),
						})
					}

					// Fetch earn products to capture the best APY for the base asset.
					// Non-fatal: earnAPYPct stays 0 when unavailable.
					baseCurUpper := strings.ToUpper(baseCur)
					if products, prodErr := ep.FetchFlexibleEarnProducts(ctx, baseCurUpper); prodErr == nil {
						for _, prod := range products {
							if strings.ToUpper(prod.Asset) == baseCurUpper {
								apyPct := prod.APY * 100
								if apyPct > earnAPYPct {
									earnAPYPct = apyPct
									bestEarnProductID = prod.ProductID
									bestEarnProductType = prod.Type
								}
							}
						}
					} else {
						logger.WarnCF("dn-monitor", "Failed to fetch earn products (APY)", map[string]any{
							"plan_id": planID, "error": prodErr.Error(),
						})
					}

					// Trailing earn-rate averages (90d/180d/365d). staking-defi has a flat
					// APY with no rate-history endpoint, so skip it. since=now-364d with a
					// generous limit triggers adapter pagination + caching. Non-fatal.
					if earnAPYPct > 0 && bestEarnProductType != "staking-defi" {
						earnSince := now.Add(-364 * 24 * time.Hour).UnixMilli()
						earnKey := "earn|" + plan.SpotProvider + "|" + plan.SpotAccount + "|" + bestEarnProductID + "|" + baseCurUpper
						var points []broker.EarnRatePoint
						var rhErr error
						if h, ok := m.cached(earnKey, now); ok {
							points = h.earn
						} else if points, rhErr = ep.FetchFlexibleEarnRateHistory(ctx, bestEarnProductID, baseCurUpper, &earnSince, 9000); rhErr == nil {
							m.keep(earnKey, cachedHistory{at: now, earn: points})
						}
						if rhErr == nil {
							earn90dPct, earn90dN = EarnWindowMeanPct(points, 90*24*time.Hour, now)
							earn180dPct, earn180dN = EarnWindowMeanPct(points, 180*24*time.Hour, now)
							earn365dPct, earn365dN = EarnWindowMeanPct(points, 365*24*time.Hour, now)
						} else {
							logger.WarnCF("dn-monitor", "Failed to fetch earn rate history (windows)", map[string]any{
								"plan_id": planID, "error": rhErr.Error(),
							})
						}
					}
				}

				// Fall back to plan notional in two cases:
				//  a) Both API calls failed (APIs down) — avoid false drift alarm.
				//  b) Balance found but < 1% of expected notional — spot is likely
				//     subscribed to an earn product whose API is not accessible
				//     (e.g. OKX Simple Earn Flexible, which is not exposed by CCXT).
				//     Use plan notional as proxy so the monitor doesn't false-alarm.
				valueUSDT := totalQty * spotState.Price
				earnGap := plan.SpotNotionalUSDT > 0 &&
					valueUSDT < plan.SpotNotionalUSDT*0.01

				if fetchedAny && !earnGap {
					spotState.Quantity = totalQty
					spotState.ValueUSDT = valueUSDT
				} else {
					if fetchedAny && earnGap {
						logger.WarnCF("dn-monitor", "Spot balance near-zero vs plan notional — assuming spot leg is in an inaccessible earn product; using plan notional for drift", map[string]any{
							"plan_id": planID, "real_value_usdt": valueUSDT, "plan_notional_usdt": plan.SpotNotionalUSDT,
						})
					}
					spotState.Quantity = plan.SpotNotionalUSDT / spotState.Price
					spotState.ValueUSDT = plan.SpotNotionalUSDT
				}
			}
		}
	}

	// Build funding info from futures fetching.
	fundingInfo := FundingInfo{Available: false}
	if futureFundingRate != nil && futuresState.Available {
		fundingInfo.Available = true
		if futureFundingRate.FundingRate != nil {
			fundingInfo.CurrentRate = *futureFundingRate.FundingRate
			// Estimate next funding payment: position notional * funding_rate. Contracts is a
			// contract COUNT, so the notional (contracts * contract size * mark) is used, not
			// contracts * mark. NotionalUSDT stays 0 when the contract size is unknown.
			fundingInfo.EstimatedNextUSDT = futuresState.NotionalUSDT * *futureFundingRate.FundingRate
		}
		if futureFundingRate.FundingTimestamp != nil {
			fundingInfo.NextFundingTime = time.UnixMilli(int64(*futureFundingRate.FundingTimestamp)).UTC()
		}
		// The last settlements, from the history fetched above: without them a run of
		// negative rates can never be told from a single one, and funding_reversal never
		// fires.
		fundingInfo.RecentRates = RecentFundingRates(fundingHistory, plan.RiskPolicy.FundingReversalCycles+1)
	}

	// Compute price-basis spreads (safe when either price is zero — helpers return 0).
	entrySpreadPct := EntrySpreadPct(futuresState.MarkPrice, spotState.Price)
	exitSpreadPct := ExitSpreadPct(spotState.Price, futuresState.MarkPrice)

	// Build evaluation input.
	input := EvaluationInput{
		Plan:           plan,
		SpotState:      spotState,
		FuturesState:   futuresState,
		FundingInfo:    fundingInfo,
		EntrySpreadPct: entrySpreadPct,
		ExitSpreadPct:  exitSpreadPct,
		Now:            now,
	}
	input.PnLUSDT, input.PnLKnown = m.planPnL(ctx, plan, spotState, futuresState)

	// Run deterministic health evaluation.
	eval := Evaluate(input)

	// Compute and attach yield metrics.
	var fundingInterval *string
	if futureFundingRate != nil {
		fundingInterval = futureFundingRate.Interval
		if futureFundingRate.FundingRate != nil {
			eval.Snapshot.FundingAPYPct = AnnualizeFundingRatePct(*futureFundingRate.FundingRate, fundingInterval)
		}
	}
	eval.Snapshot.EarnAPYPct = earnAPYPct
	eval.Snapshot.CombinedAPYPct = eval.Snapshot.FundingAPYPct + earnAPYPct

	// Trailing earn windows (3M/6M/12M): fall back to the current earn rate when a
	// window has no rate-history points so the matched combined math is always valid.
	eval.Snapshot.Earn90dAPYPct = earnAPYPct
	eval.Snapshot.Earn180dAPYPct = earnAPYPct
	eval.Snapshot.Earn365dAPYPct = earnAPYPct
	if earn90dN > 0 {
		eval.Snapshot.Earn90dAPYPct = earn90dPct
	}
	if earn180dN > 0 {
		eval.Snapshot.Earn180dAPYPct = earn180dPct
	}
	if earn365dN > 0 {
		eval.Snapshot.Earn365dAPYPct = earn365dPct
	}

	// Matched-window combined APY = funding window avg (annualised) + earn window avg.
	// Each funding window falls back to the current funding APY when no history exists.
	fundingWindowAPY := func(window time.Duration) float64 {
		if mean, n := FundingWindowMeanRate(fundingHistory, window, now); n > 0 {
			return AnnualizeFundingRatePct(mean, fundingInterval)
		}
		return eval.Snapshot.FundingAPYPct
	}
	// Persist the funding windows too (OKX funding history caps at ~3 months, so its
	// 180d/365d collapse to the 90d value; Binance retains >1y and stays distinct).
	eval.Snapshot.Funding90dAPYPct = fundingWindowAPY(90 * 24 * time.Hour)
	eval.Snapshot.Funding180dAPYPct = fundingWindowAPY(180 * 24 * time.Hour)
	eval.Snapshot.Funding365dAPYPct = fundingWindowAPY(365 * 24 * time.Hour)
	eval.Snapshot.Combined90dAPYPct = eval.Snapshot.Funding90dAPYPct + eval.Snapshot.Earn90dAPYPct
	eval.Snapshot.Combined180dAPYPct = eval.Snapshot.Funding180dAPYPct + eval.Snapshot.Earn180dAPYPct
	eval.Snapshot.Combined365dAPYPct = eval.Snapshot.Funding365dAPYPct + eval.Snapshot.Earn365dAPYPct

	eval.Snapshot.PlanID = planID
	eval.Snapshot.CreatedAt = now
	return MonitorResult{Eval: eval, Snapshot: eval.Snapshot}, nil
}
