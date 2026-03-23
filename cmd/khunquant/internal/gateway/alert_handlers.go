package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/khunquant/khunquant/pkg/bus"
	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/cron"
	"github.com/khunquant/khunquant/pkg/logger"
	"github.com/khunquant/khunquant/pkg/providers/broker"
	"github.com/khunquant/khunquant/pkg/ta"
)

// alertPricePayload mirrors tools.priceAlertPayload (unexported there).
type alertPricePayload struct {
	ProviderID string  `json:"provider"`
	Account    string  `json:"account"`
	Symbol     string  `json:"symbol"`
	Condition  string  `json:"condition"` // "above" | "below"
	Threshold  float64 `json:"threshold"`
	AlertMsg   string  `json:"alert_msg"`
	Recurring  bool    `json:"recurring"`
}

// alertIndicatorPayload mirrors tools.indicatorAlertPayload (unexported there).
type alertIndicatorPayload struct {
	ProviderID string  `json:"provider"`
	Account    string  `json:"account"`
	Symbol     string  `json:"symbol"`
	Timeframe  string  `json:"timeframe"`
	Indicator  string  `json:"indicator"` // RSI | MACD | SMA20 | EMA9
	Condition  string  `json:"condition"` // "above" | "below"
	Threshold  float64 `json:"threshold"`
	AlertMsg   string  `json:"alert_msg"`
	Recurring  bool    `json:"recurring"`
}

// handlePriceAlertJob executes a price alert cron job directly in code,
// bypassing the LLM. It fetches the live ticker, evaluates the condition,
// sends a notification via the message bus, and removes non-recurring jobs.
func handlePriceAlertJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	cronService *cron.CronService,
	msgBus *bus.MessageBus,
) (string, error) {
	var payload alertPricePayload
	if err := json.Unmarshal([]byte(job.Payload.Message), &payload); err != nil {
		return "", fmt.Errorf("price alert: decode payload: %w", err)
	}

	p, err := broker.CreateProviderForAccount(payload.ProviderID, payload.Account, cfg)
	if err != nil {
		return "", fmt.Errorf("price alert: create provider %q: %w", payload.ProviderID, err)
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return "", fmt.Errorf("price alert: provider %q does not support market data", payload.ProviderID)
	}

	ticker, err := md.FetchTicker(ctx, payload.Symbol)
	if err != nil {
		return "", fmt.Errorf("price alert: FetchTicker %s: %w", payload.Symbol, err)
	}

	if ticker.Last == nil {
		return "no price available", nil
	}

	price := *ticker.Last
	triggered := false
	switch payload.Condition {
	case "above":
		triggered = price > payload.Threshold
	case "below":
		triggered = price < payload.Threshold
	}

	if !triggered {
		return "condition not met", nil
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

	// Send via bus if we have routing info.
	channel := job.Payload.Channel
	chatID := job.Payload.To
	if channel != "" && chatID != "" {
		if err := msgBus.PublishOutbound(ctx, bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: notification,
		}); err != nil {
			logger.ErrorCF("alert", "Failed to deliver price alert", map[string]any{
				"job_id": job.ID, "error": err.Error(),
			})
		}
	} else {
		logger.WarnCF("alert", "Price alert fired but no channel/chatID stored in job", map[string]any{
			"job_id": job.ID, "symbol": payload.Symbol,
		})
	}

	// Remove one-shot job.
	if !payload.Recurring {
		cronService.RemoveJob(job.ID)
	}

	return "alert fired", nil
}

// handleIndicatorAlertJob executes an indicator alert cron job directly in code.
// It fetches OHLCV data, computes the indicator, evaluates the condition,
// and notifies the user without going through the LLM.
func handleIndicatorAlertJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	cronService *cron.CronService,
	msgBus *bus.MessageBus,
) (string, error) {
	var payload alertIndicatorPayload
	if err := json.Unmarshal([]byte(job.Payload.Message), &payload); err != nil {
		return "", fmt.Errorf("indicator alert: decode payload: %w", err)
	}

	p, err := broker.CreateProviderForAccount(payload.ProviderID, payload.Account, cfg)
	if err != nil {
		return "", fmt.Errorf("indicator alert: create provider %q: %w", payload.ProviderID, err)
	}

	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return "", fmt.Errorf("indicator alert: provider %q does not support market data", payload.ProviderID)
	}

	timeframe := payload.Timeframe
	if timeframe == "" {
		timeframe = "1h"
	}

	candles, err := md.FetchOHLCV(ctx, payload.Symbol, timeframe, nil, 100)
	if err != nil {
		return "", fmt.Errorf("indicator alert: FetchOHLCV: %w", err)
	}
	if len(candles) < 20 {
		return "not enough data", nil
	}

	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}

	var value float64
	var hasValue bool
	switch payload.Indicator {
	case "RSI":
		vals := ta.RSI(closes, 14)
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
	case "SMA20":
		vals := ta.SMA(closes, 20)
		if len(vals) > 0 {
			value = vals[len(vals)-1]
			hasValue = true
		}
	case "EMA9":
		vals := ta.EMA(closes, 9)
		if len(vals) > 0 {
			value = vals[len(vals)-1]
			hasValue = true
		}
	}

	if !hasValue {
		return "insufficient data for indicator", nil
	}

	triggered := false
	switch payload.Condition {
	case "above":
		triggered = value > payload.Threshold
	case "below":
		triggered = value < payload.Threshold
	}

	if !triggered {
		return "condition not met", nil
	}

	notification := payload.AlertMsg
	if notification == "" {
		notification = fmt.Sprintf(
			"🔔 Indicator Alert: %s %s(%s) is %s %.4g (current: %.4g) on %s",
			payload.Symbol, payload.Indicator, timeframe,
			payload.Condition, payload.Threshold, value,
			payload.ProviderID,
		)
	} else {
		notification = fmt.Sprintf(
			"%s\n%s %s(%s): %.4g",
			notification, payload.Symbol, payload.Indicator, timeframe, value,
		)
	}

	channel := job.Payload.Channel
	chatID := job.Payload.To
	if channel != "" && chatID != "" {
		if err := msgBus.PublishOutbound(ctx, bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: notification,
		}); err != nil {
			logger.ErrorCF("alert", "Failed to deliver indicator alert", map[string]any{
				"job_id": job.ID, "error": err.Error(),
			})
		}
	} else {
		logger.WarnCF("alert", "Indicator alert fired but no channel/chatID stored in job", map[string]any{
			"job_id": job.ID, "symbol": payload.Symbol,
		})
	}

	if !payload.Recurring {
		cronService.RemoveJob(job.ID)
	}

	return "alert fired", nil
}
