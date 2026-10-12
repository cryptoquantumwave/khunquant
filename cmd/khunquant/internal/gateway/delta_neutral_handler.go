package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/fees"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// handleDeltaNeutralMonitorJob is the cron dispatcher for delta-neutral monitor plans (job name prefix "dn:").
//
// It fetches live state for both legs (spot and futures), evaluates health deterministically in pure Go,
// saves a snapshot, and escalates to the LLM if thresholds are breached.
// Alerts are always delivered via the message bus if a breach occurs, even if cronTool is nil.
func handleDeltaNeutralMonitorJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	dnStore *deltaneutral.Store,
	cronTool *tools.CronTool,
	msgBus *bus.MessageBus,
) (string, error) {
	// Parse plan ID from "dn:<id>:<name>".
	parts := strings.SplitN(job.Name, ":", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("dn-monitor: malformed job name %q", job.Name)
	}
	planID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("dn-monitor: parse plan_id from %q: %w", job.Name, err)
	}

	plan, err := dnStore.GetPlan(ctx, planID)
	if err != nil {
		logger.DebugCF("dn-monitor", "Plan not found, skipping", map[string]any{
			"plan_id": planID, "job": job.Name, "error": err.Error(),
		})
		return "plan not found", nil
	}

	// Skip if plan is not monitorable.
	if !plan.Enabled {
		logger.DebugCF("dn-monitor", "Plan disabled, skipping", map[string]any{"plan_id": planID})
		return "plan disabled", nil
	}

	// Skip plans that have no open position yet or are permanently closed.
	// 'ready' plans have been prepared but not opened — no legs exist on the exchange,
	// so a full monitor run would produce 100% delta drift and bogus alerts.
	if plan.Status == deltaneutral.PlanStatusClosed ||
		plan.Status == deltaneutral.PlanStatusArchived ||
		plan.Status == deltaneutral.PlanStatusDraft ||
		plan.Status == deltaneutral.PlanStatusReady {
		logger.DebugCF("dn-monitor", "Plan not monitorable (status: "+plan.Status+"), skipping", map[string]any{"plan_id": planID})
		return "plan status not monitorable", nil
	}
	// A failed plan normally holds no position and is skipped — but a plan that still has
	// open exposure must stay monitored (unwind skips failed plans, so the monitor is the
	// only thing watching it). If the exposure check itself errors, keep monitoring.
	if plan.Status == deltaneutral.PlanStatusFailed {
		hasExposure, expErr := dnStore.HasOpenExposure(ctx, planID)
		if expErr != nil {
			logger.WarnCF("dn-monitor", "Could not check open exposure of failed plan; monitoring it anyway", map[string]any{
				"plan_id": planID, "error": expErr.Error(),
			})
		} else if !hasExposure {
			logger.DebugCF("dn-monitor", "Plan not monitorable (status: failed, no open exposure), skipping", map[string]any{"plan_id": planID})
			return "plan status not monitorable", nil
		}
	}

	now := time.Now().UTC()

	// Fetch live state for futures leg.
	futuresState := deltaneutral.FuturesState{}
	var futureFundingRate *ccxt.FundingRate      // Store funding rate if available.
	var fundingHistory []ccxt.FundingRateHistory // Trailing funding history for windowed averages.
	futureFetcher, err := broker.CreateProviderForAccount(plan.FuturesProvider, plan.FuturesAccount, cfg)
	if err != nil {
		logger.WarnCF("dn-monitor", "Failed to create futures provider", map[string]any{
			"plan_id": planID, "provider": plan.FuturesProvider, "error": err.Error(),
		})
		futuresState.Available = false
	} else {
		futuresProv, ok := futureFetcher.(broker.FuturesProvider)
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
				// Extract position data (assume first position matches the symbol).
				if len(positions) > 0 {
					pos := positions[0]
					if pos.Contracts != nil {
						futuresState.Contracts = *pos.Contracts
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
			if hist, histErr := futuresProv.FetchPublicFundingRateHistory(ctx, plan.FuturesSymbol, nil, 1100); histErr == nil {
				fundingHistory = hist
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
	spotState := deltaneutral.SpotState{}
	spotFetcher, err := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, cfg)
	if err != nil {
		logger.WarnCF("dn-monitor", "Failed to create spot provider", map[string]any{
			"plan_id": planID, "provider": plan.SpotProvider, "error": err.Error(),
		})
		spotState.Available = false
	} else {
		spotMD, ok := spotFetcher.(broker.MarketDataProvider)
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
				if pp, ok := spotFetcher.(broker.PortfolioProvider); ok {
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
				if ep, ok := spotFetcher.(broker.EarnProvider); ok {
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
						if points, rhErr := ep.FetchFlexibleEarnRateHistory(ctx, bestEarnProductID, baseCurUpper, &earnSince, 9000); rhErr == nil {
							earn90dPct, earn90dN = deltaneutral.EarnWindowMeanPct(points, 90*24*time.Hour, now)
							earn180dPct, earn180dN = deltaneutral.EarnWindowMeanPct(points, 180*24*time.Hour, now)
							earn365dPct, earn365dN = deltaneutral.EarnWindowMeanPct(points, 365*24*time.Hour, now)
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
	fundingInfo := deltaneutral.FundingInfo{Available: false}
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
		// For now, keep RecentRates empty; in production, you'd fetch history.
	}

	// Compute price-basis spreads (safe when either price is zero — helpers return 0).
	entrySpreadPct := deltaneutral.EntrySpreadPct(futuresState.MarkPrice, spotState.Price)
	exitSpreadPct := deltaneutral.ExitSpreadPct(spotState.Price, futuresState.MarkPrice)

	// Build evaluation input.
	input := deltaneutral.EvaluationInput{
		Plan:           *plan,
		SpotState:      spotState,
		FuturesState:   futuresState,
		FundingInfo:    fundingInfo,
		EntrySpreadPct: entrySpreadPct,
		ExitSpreadPct:  exitSpreadPct,
		Now:            now,
	}

	// Run deterministic health evaluation.
	eval := deltaneutral.Evaluate(input)

	// Compute and attach yield metrics.
	var fundingInterval *string
	if futureFundingRate != nil {
		fundingInterval = futureFundingRate.Interval
		if futureFundingRate.FundingRate != nil {
			eval.Snapshot.FundingAPYPct = deltaneutral.AnnualizeFundingRatePct(*futureFundingRate.FundingRate, fundingInterval)
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
		if mean, n := deltaneutral.FundingWindowMeanRate(fundingHistory, window, now); n > 0 {
			return deltaneutral.AnnualizeFundingRatePct(mean, fundingInterval)
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

	// Always save snapshot.
	eval.Snapshot.PlanID = planID
	eval.Snapshot.CreatedAt = now
	snapID, err := dnStore.SaveSnapshot(ctx, &eval.Snapshot)
	if err != nil {
		logger.ErrorCF("dn-monitor", "Failed to save snapshot", map[string]any{
			"plan_id": planID, "error": err.Error(),
		})
		// Continue; don't fail the job.
	}

	go refreshPlanFees(context.Background(), plan, dnStore, cfg)

	// If no breach, return early.
	if !eval.ThresholdBreached {
		logger.DebugCF("dn-monitor", "No breach detected", map[string]any{
			"plan_id": planID,
			"health":  eval.Snapshot.HealthLabel,
			"score":   eval.Snapshot.HealthScore,
		})
		return "ok: no breach", nil
	}

	// Check alert cooldown — skip alert + LLM if all breach codes are still silenced.
	silences, _ := dnStore.GetActiveAlertSilences(ctx, planID)
	unsilenced := filterUnsilencedCodes(eval.BreachCodes, silences)
	if len(unsilenced) == 0 {
		logger.DebugCF("dn-monitor", "All breach codes within cooldown, skipping alert", map[string]any{
			"plan_id": planID, "codes": eval.BreachCodes,
		})
		return "silenced: cooldown active for all breach codes", nil
	}

	// Breach detected: build and save alert.
	logger.WarnCF("dn-monitor", "Threshold breached", map[string]any{
		"plan_id":  planID,
		"codes":    eval.BreachCodes,
		"severity": eval.Severity,
	})

	alertCode := "breach"
	if len(eval.BreachCodes) > 0 {
		alertCode = eval.BreachCodes[0]
	}

	alertMsg := fmt.Sprintf("Delta-neutral plan %d (%s) health alert: %v", planID, plan.Name, eval.BreachCodes)

	alert := &deltaneutral.Alert{
		PlanID:            planID,
		SnapshotID:        &snapID,
		TriggeredAt:       now,
		Severity:          eval.Severity,
		Code:              alertCode,
		Message:           alertMsg,
		RecommendedAction: eval.RecommendedAction,
		AgentInvoked:      false,
		DeliveredChannel:  plan.NotifyChannel,
		DeliveredChatID:   plan.NotifyChatID,
		CreatedAt:         now,
	}

	if _, err := dnStore.SaveAlert(ctx, alert); err != nil {
		logger.ErrorCF("dn-monitor", "Failed to save alert", map[string]any{
			"plan_id": planID, "error": err.Error(),
		})
		// Continue; deliver it anyway.
	}

	// Deliver alert via message bus if a channel is configured.
	if plan.NotifyChannel != "" {
		// Use a short timeout to avoid blocking the cron job.
		deliverCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := msgBus.PublishOutbound(deliverCtx, bus.OutboundMessage{
			Channel: plan.NotifyChannel,
			ChatID:  plan.NotifyChatID,
			Content: alertMsg,
		})
		if err != nil {
			logger.WarnCF("dn-monitor", "Failed to deliver alert via message bus", map[string]any{
				"plan_id": planID, "channel": plan.NotifyChannel, "error": err.Error(),
			})
		} else {
			logger.DebugCF("dn-monitor", "Alert delivered", map[string]any{
				"plan_id": planID, "channel": plan.NotifyChannel,
			})
		}
	}

	// Auto-silence non-critical breach codes for the plan's AlertCooldownDuration so the
	// same condition doesn't invoke the LLM again until the window expires. Critical codes
	// (liquidation_distance_low, margin_critical, data_unavailable) are NEVER silenced —
	// they can keep worsening under the same code, so they must re-alert on every tick.
	var silenceable []string
	for _, c := range eval.BreachCodes {
		if !deltaneutral.IsCriticalBreachCode(c) {
			silenceable = append(silenceable, c)
		}
	}
	if len(silenceable) > 0 {
		silenceUntil := time.Now().Add(parseSilenceDuration(plan.RiskPolicy.AlertCooldownDuration))
		if siErr := dnStore.UpsertAlertSilences(ctx, planID, silenceable, silenceUntil); siErr != nil {
			logger.WarnCF("dn-monitor", "Failed to set alert silences", map[string]any{
				"plan_id": planID, "error": siErr.Error(),
			})
		}
	}

	// If cronTool is available, escalate to LLM for agent explanation.
	if cronTool != nil {
		return cronTool.ExecuteJob(ctx, job)
	}

	// Alert delivered but no agent explanation.
	return "breach alerted (no agent)", nil
}

// filterUnsilencedCodes returns codes not present in the active silences map.
func filterUnsilencedCodes(codes []string, silences map[string]time.Time) []string {
	var out []string
	for _, code := range codes {
		if _, silenced := silences[code]; !silenced {
			out = append(out, code)
		}
	}
	return out
}

// parseSilenceDuration converts a cooldown duration string to time.Duration.
// Valid values: "1h", "4h", "8h", "1d", "3d". Defaults to 1h.
func parseSilenceDuration(s string) time.Duration {
	switch s {
	case "4h":
		return 4 * time.Hour
	case "8h":
		return 8 * time.Hour
	case "1d":
		return 24 * time.Hour
	case "3d":
		return 3 * 24 * time.Hour
	default:
		return time.Hour
	}
}

// refreshPlanFees saves a fee snapshot for the plan. Skips if the last fetch was
// within 30 minutes or if the plan has not yet been executed (OpenedAt == nil).
//
// Trading fee source: our own execution leg records (fee_usdt column), summed per plan.
// This is immune to OKX's two failure modes:
//   - positions-history fee = full lifetime fee of that position, not window-scoped
//   - cross-margin positions accumulate fee across add/reduce cycles from before plan.OpenedAt
//
// Funding fee source: OKX positions API — funding fees are not captured in order
// responses so the exchange API is the only source.
func refreshPlanFees(ctx context.Context, plan *deltaneutral.Plan, store *deltaneutral.Store, cfg *config.Config) {
	if plan.OpenedAt == nil {
		return
	}

	const staleness = 30 * time.Minute
	if last, err := store.GetLatestPlanFeeSnapshot(ctx, plan.ID); err == nil && last != nil {
		if time.Since(last.FetchedAt) < staleness {
			return
		}
	}

	// Trading fee: sum fee_usdt from all filled execution legs for this plan.
	// Negative values are costs (fees paid); positive are rebates.
	tradingFee, err := store.SumPlanExecutionFees(ctx, plan.ID)
	if err != nil {
		logger.WarnCF("dn-fees", "sum execution fees failed", map[string]any{"plan_id": plan.ID, "error": err.Error()})
		return
	}

	// Funding fee: fetch from exchange — the only reliable source.
	// Non-fatal: snapshot is saved with fundingFee=0 if the provider is unsupported.
	var fundingFee float64
	fetcher, fetcherErr := fees.NewFeesFetcher(plan.FuturesProvider, plan.FuturesAccount, cfg)
	if fetcherErr == nil {
		if pf, pfErr := fetcher.FetchFuturesPositionFees(ctx, fees.FetchFeesRequest{
			FuturesSymbol: plan.FuturesSymbol,
			Since:         *plan.OpenedAt,
			Until:         time.Now().UTC(),
		}); pfErr == nil {
			fundingFee = pf.FundingFeeUSDT
		} else {
			logger.WarnCF("dn-fees", "funding fee fetch failed", map[string]any{"plan_id": plan.ID, "error": pfErr.Error()})
		}
	}

	now := time.Now().UTC()
	until := now
	if _, err := store.SavePlanFeeSnapshot(ctx, &deltaneutral.PlanFeeSnapshot{
		PlanID:         plan.ID,
		TradingFeeUSDT: tradingFee,
		FundingFeeUSDT: fundingFee,
		PeriodStart:    plan.OpenedAt,
		PeriodEnd:      &until,
		FetchedAt:      now,
		CreatedAt:      now,
	}); err != nil {
		logger.WarnCF("dn-fees", "save fee snapshot failed", map[string]any{"plan_id": plan.ID, "error": err.Error()})
	}
}
