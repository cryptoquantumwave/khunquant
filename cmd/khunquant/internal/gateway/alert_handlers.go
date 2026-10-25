package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/ta"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const (
	// alertPublishTimeout bounds every outbound publish made by an alert handler.
	// Cron runs due jobs serially, so an unbounded publish on a full bus buffer
	// would stall every other job.
	alertPublishTimeout = 5 * time.Second

	// alertBrokenNoticeThreshold is the number of consecutive data errors after
	// which the user is told, exactly once per error streak, that the alert is
	// not working.
	alertBrokenNoticeThreshold = 3
)

// alertNow is the clock used for recurring-alert cooldowns. Tests replace it.
var alertNow = time.Now

// updateAlertPayload re-reads the job right before writing, decodes its payload
// into T, applies fn, and replaces only Payload.Message. Re-reading (rather than
// writing back the handler's copy) avoids clobbering a concurrent edit.
func updateAlertPayload[T any](cs *cron.CronService, jobID string, fn func(*T)) (T, error) {
	var p T
	fresh := cs.GetJob(jobID)
	if fresh == nil {
		return p, fmt.Errorf("alert job %s no longer exists", jobID)
	}
	if err := json.Unmarshal([]byte(fresh.Payload.Message), &p); err != nil {
		return p, fmt.Errorf("decode alert payload: %w", err)
	}
	fn(&p)
	raw, err := json.Marshal(p)
	if err != nil {
		return p, fmt.Errorf("encode alert payload: %w", err)
	}
	fresh.Payload.Message = string(raw)
	if err := cs.UpdateJob(fresh); err != nil {
		return p, fmt.Errorf("update alert job: %w", err)
	}
	return p, nil
}

// deliverAlertMessage publishes content to the job's channel/chat with a bounded
// timeout. A nil return means the message was enqueued on the bus. It returns an
// error when the job has no routing or the publish fails.
func deliverAlertMessage(ctx context.Context, msgBus *bus.MessageBus, job *cron.CronJob, content string) error {
	channel := job.Payload.Channel
	chatID := job.Payload.To
	if channel == "" || chatID == "" {
		return fmt.Errorf("no delivery channel/chat_id stored on the job (channel=%q chat_id=%q)", channel, chatID)
	}
	pubCtx, cancel := context.WithTimeout(ctx, alertPublishTimeout)
	defer cancel()
	return msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: channel,
		ChatID:  chatID,
		Content: content,
	})
}

// alertDataFailure records one data error for an alert and, when the streak
// reaches alertBrokenNoticeThreshold exactly, tells the user once. bump persists
// the incremented counter and returns the new count. The returned error wraps
// cause so cron records LastStatus="error".
func alertDataFailure(
	ctx context.Context,
	kind string,
	job *cron.CronJob,
	msgBus *bus.MessageBus,
	providerID, symbol string,
	cause error,
	bump func() (int, error),
) error {
	count, err := bump()
	if err != nil {
		logger.WarnCF("alert", "Failed to persist alert error counter", map[string]any{
			"job_id": job.ID, "error": err.Error(),
		})
	} else if count == alertBrokenNoticeThreshold {
		notice := fmt.Sprintf("Alert '%s' cannot fetch %s from %s: %s. It will keep retrying.",
			job.Name, symbol, providerID, cause)
		if derr := deliverAlertMessage(ctx, msgBus, job, notice); derr != nil {
			logger.WarnCF("alert", "Failed to deliver broken-alert notice", map[string]any{
				"job_id": job.ID, "error": derr.Error(),
			})
		}
	}
	return fmt.Errorf("%s: %w", kind, cause)
}

// alertInCooldown reports whether a recurring alert fired too recently to fire again.
func alertInCooldown(recurring bool, lastFiredMS int64, cooldownMinutes int, now time.Time) bool {
	if !recurring || lastFiredMS <= 0 {
		return false
	}
	cooldown := time.Duration(tools.EffectiveAlertCooldownMinutes(cooldownMinutes)) * time.Minute
	return now.UnixMilli()-lastFiredMS < cooldown.Milliseconds()
}

// handlePriceAlertJob executes a price alert cron job directly in code,
// bypassing the LLM. It fetches the live ticker, evaluates the condition and
// sends a notification via the message bus. A one-shot job is removed only after
// the notification was enqueued; a recurring job is rate-limited by its cooldown.
func handlePriceAlertJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	cronService *cron.CronService,
	msgBus *bus.MessageBus,
) (string, error) {
	const kind = "price alert"

	var payload tools.PriceAlertPayload
	if err := json.Unmarshal([]byte(job.Payload.Message), &payload); err != nil {
		return "", fmt.Errorf("%s: decode payload: %w", kind, err)
	}

	fail := func(cause error) (string, error) {
		return "", alertDataFailure(ctx, kind, job, msgBus, payload.ProviderID, payload.Symbol, cause, func() (int, error) {
			p, err := updateAlertPayload(cronService, job.ID, func(p *tools.PriceAlertPayload) { p.ConsecutiveErrors++ })
			return p.ConsecutiveErrors, err
		})
	}

	if !tools.ValidAlertCondition(payload.Condition) {
		return fail(fmt.Errorf("invalid condition %q in stored alert (allowed: above, below)", payload.Condition))
	}

	p, err := broker.CreateProviderForAccount(payload.ProviderID, payload.Account, cfg)
	if err != nil {
		return fail(fmt.Errorf("create provider %q: %w", payload.ProviderID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return fail(fmt.Errorf("provider %q does not support market data", payload.ProviderID))
	}

	ticker, err := md.FetchTicker(ctx, payload.Symbol)
	if err != nil {
		return fail(fmt.Errorf("FetchTicker %s: %w", payload.Symbol, err))
	}
	if ticker.Last == nil {
		return fail(errors.New("no price available"))
	}

	// The data was fetched and the alert is evaluable: the error streak is over.
	resetAlertErrors(cronService, job.ID, payload.ConsecutiveErrors, func(p *tools.PriceAlertPayload) { p.ConsecutiveErrors = 0 })

	price := *ticker.Last
	triggered := false
	switch payload.Condition {
	case tools.AlertConditionAbove:
		triggered = price > payload.Threshold
	case tools.AlertConditionBelow:
		triggered = price < payload.Threshold
	}

	if !triggered {
		return "condition not met", nil
	}

	now := alertNow()
	if alertInCooldown(payload.Recurring, payload.LastFiredMS, payload.CooldownMinutes, now) {
		return "condition met; in cooldown", nil
	}

	// Build notification text.
	notification := payload.AlertMsg
	if notification == "" {
		notification = fmt.Sprintf(
			"🔔 Price Alert: %s is %s %.8g (current: %.8g) on %s",
			payload.Symbol, payload.Condition, payload.Threshold, price, payload.ProviderID,
		)
	} else {
		notification = fmt.Sprintf(
			"%s\n%s %.8g (current: %.8g)",
			notification, payload.Symbol, payload.Threshold, price,
		)
	}

	if err := deliverAlertMessage(ctx, msgBus, job, notification); err != nil {
		// Keep the job (one-shot or recurring) so the next tick retries.
		logger.ErrorCF("alert", "Failed to deliver price alert; job kept for retry", map[string]any{
			"job_id": job.ID, "error": err.Error(),
		})
		return "", fmt.Errorf("%s: deliver notification: %w", kind, err)
	}

	legacyAlertFired.Add(1) // delivered: counted for the shadow period's daily comparison
	if !payload.Recurring {
		cronService.RemoveJob(job.ID)
		return "alert fired", nil
	}

	if _, err := updateAlertPayload(cronService, job.ID, func(p *tools.PriceAlertPayload) {
		p.LastFiredMS = now.UnixMilli()
	}); err != nil {
		return "", fmt.Errorf("%s: alert delivered but cooldown state not saved: %w", kind, err)
	}
	return "alert fired", nil
}

// resetAlertErrors clears a non-zero error streak. current is the count the
// handler observed at the start of the tick; a zero count needs no write.
func resetAlertErrors[T any](cs *cron.CronService, jobID string, current int, fn func(*T)) {
	if current == 0 {
		return
	}
	if _, err := updateAlertPayload(cs, jobID, fn); err != nil {
		logger.WarnCF("alert", "Failed to reset alert error counter", map[string]any{
			"job_id": jobID, "error": err.Error(),
		})
	}
}

// handleIndicatorAlertJob executes an indicator alert cron job directly in code.
// It fetches OHLCV data, computes the indicator, evaluates the condition,
// and notifies the user without going through the LLM. Delivery, one-shot
// removal, cooldown and broken-alert handling match handlePriceAlertJob.
func handleIndicatorAlertJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	cronService *cron.CronService,
	msgBus *bus.MessageBus,
) (string, error) {
	const kind = "indicator alert"

	var payload tools.IndicatorAlertPayload
	if err := json.Unmarshal([]byte(job.Payload.Message), &payload); err != nil {
		return "", fmt.Errorf("%s: decode payload: %w", kind, err)
	}

	fail := func(cause error) (string, error) {
		return "", alertDataFailure(ctx, kind, job, msgBus, payload.ProviderID, payload.Symbol, cause, func() (int, error) {
			p, err := updateAlertPayload(cronService, job.ID, func(p *tools.IndicatorAlertPayload) { p.ConsecutiveErrors++ })
			return p.ConsecutiveErrors, err
		})
	}

	if !tools.ValidAlertCondition(payload.Condition) {
		return fail(fmt.Errorf("invalid condition %q in stored alert (allowed: above, below)", payload.Condition))
	}

	// Normalize indicator family and period, including legacy "SMA20"/"EMA9" strings.
	ind := strings.ToUpper(payload.Indicator)
	period := payload.Period
	switch ind {
	case "SMA20":
		ind, period = "SMA", 20
	case "EMA9":
		ind, period = "EMA", 9
	}
	if !tools.ValidAlertIndicatorFamily(ind) {
		return fail(fmt.Errorf("unknown indicator %q in stored alert (allowed: RSI, MACD, SMA, EMA)", payload.Indicator))
	}
	if period <= 0 {
		switch ind {
		case "RSI":
			period = 14
		case "SMA", "EMA":
			period = 20
		}
	}
	if ind != "MACD" && period < 2 {
		return fail(fmt.Errorf("invalid period %d for %s in stored alert (must be >= 2)", period, ind))
	}

	p, err := broker.CreateProviderForAccount(payload.ProviderID, payload.Account, cfg)
	if err != nil {
		return fail(fmt.Errorf("create provider %q: %w", payload.ProviderID, err))
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return fail(fmt.Errorf("provider %q does not support market data", payload.ProviderID))
	}

	timeframe := payload.Timeframe
	if timeframe == "" {
		timeframe = "1h"
	}

	// Request enough bars for a reliable computation: period*3, at least 100, at most 500.
	limit := period * 3
	if limit < 100 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	candles, err := md.FetchOHLCV(ctx, payload.Symbol, timeframe, nil, limit)
	if err != nil {
		return fail(fmt.Errorf("FetchOHLCV %s: %w", payload.Symbol, err))
	}
	required := period + 1
	if len(candles) < required {
		return fail(fmt.Errorf(
			"not enough data: have %d bars, need >= %d for %s(%d) — exchange history may be too shallow for this period/timeframe",
			len(candles), required, ind, period))
	}

	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}

	var value float64
	var hasValue bool
	switch ind {
	case "RSI":
		vals := ta.RSI(closes, period)
		if len(vals) > 0 {
			value = vals[len(vals)-1]
			hasValue = true
		}
	case "MACD":
		result := ta.MACD(closes, 12, 26, 9)
		if result != nil && len(result.MACD) > 0 {
			value = result.MACD[len(result.MACD)-1]
			hasValue = true
		}
	case "SMA":
		vals := ta.SMA(closes, period)
		if len(vals) > 0 {
			value = vals[len(vals)-1]
			hasValue = true
		}
	case "EMA":
		vals := ta.EMA(closes, period)
		if len(vals) > 0 {
			value = vals[len(vals)-1]
			hasValue = true
		}
	}

	if !hasValue {
		return fail(fmt.Errorf("insufficient data to compute %s", ind))
	}

	// The indicator was computed: the error streak is over.
	resetAlertErrors(cronService, job.ID, payload.ConsecutiveErrors, func(p *tools.IndicatorAlertPayload) { p.ConsecutiveErrors = 0 })

	triggered := false
	switch payload.Condition {
	case tools.AlertConditionAbove:
		triggered = value > payload.Threshold
	case tools.AlertConditionBelow:
		triggered = value < payload.Threshold
	}

	if !triggered {
		return "condition not met", nil
	}

	now := alertNow()
	if alertInCooldown(payload.Recurring, payload.LastFiredMS, payload.CooldownMinutes, now) {
		return "condition met; in cooldown", nil
	}

	indLabel := ind
	if ind != "MACD" {
		indLabel = fmt.Sprintf("%s(%d)", ind, period)
	}
	notification := payload.AlertMsg
	if notification == "" {
		notification = fmt.Sprintf(
			"🔔 Indicator Alert: %s %s(%s) is %s %.4g (current: %.4g) on %s",
			payload.Symbol, indLabel, timeframe,
			payload.Condition, payload.Threshold, value,
			payload.ProviderID,
		)
	} else {
		notification = fmt.Sprintf(
			"%s\n%s %s(%s): %.4g",
			notification, payload.Symbol, indLabel, timeframe, value,
		)
	}

	if err := deliverAlertMessage(ctx, msgBus, job, notification); err != nil {
		// Keep the job (one-shot or recurring) so the next tick retries.
		logger.ErrorCF("alert", "Failed to deliver indicator alert; job kept for retry", map[string]any{
			"job_id": job.ID, "error": err.Error(),
		})
		return "", fmt.Errorf("%s: deliver notification: %w", kind, err)
	}

	legacyAlertFired.Add(1) // delivered: counted for the shadow period's daily comparison
	if !payload.Recurring {
		cronService.RemoveJob(job.ID)
		return "alert fired", nil
	}

	if _, err := updateAlertPayload(cronService, job.ID, func(p *tools.IndicatorAlertPayload) {
		p.LastFiredMS = now.UnixMilli()
	}); err != nil {
		return "", fmt.Errorf("%s: alert delivered but cooldown state not saved: %w", kind, err)
	}
	return "alert fired", nil
}
