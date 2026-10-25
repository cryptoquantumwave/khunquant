package tools

import (
	"errors"
	"fmt"
)

// DefaultAlertCooldownMinutes is the minimum gap between two firings of a
// recurring alert when the payload does not carry an explicit cooldown.
const DefaultAlertCooldownMinutes = 60

// Allowed alert condition values.
const (
	AlertConditionAbove = "above"
	AlertConditionBelow = "below"
)

// PriceAlertPayload is encoded as JSON in CronPayload.Message for price alerts.
// It is shared by the creation tool and the gateway cron handler so a new field
// can never be silently dropped when the handler re-marshals the payload.
type PriceAlertPayload struct {
	ProviderID string  `json:"provider"`
	Account    string  `json:"account"`
	Symbol     string  `json:"symbol"`
	Condition  string  `json:"condition"` // "above" | "below"
	Threshold  float64 `json:"threshold"`
	AlertMsg   string  `json:"alert_msg"`
	Recurring  bool    `json:"recurring"`

	// CooldownMinutes is the minimum gap between firings of a recurring alert.
	// 0 (absent) means DefaultAlertCooldownMinutes.
	CooldownMinutes int `json:"cooldown_minutes,omitempty"`
	// LastFiredMS is the unix-millisecond time of the last successful delivery.
	LastFiredMS int64 `json:"last_fired_ms,omitempty"`
	// ConsecutiveErrors counts back-to-back data errors; reset on a good evaluation.
	ConsecutiveErrors int `json:"consecutive_errors,omitempty"`
}

// IndicatorAlertPayload is encoded as JSON in CronPayload.Message for indicator alerts.
// See PriceAlertPayload for why it is shared.
type IndicatorAlertPayload struct {
	ProviderID string  `json:"provider"`
	Account    string  `json:"account"`
	Symbol     string  `json:"symbol"`
	Timeframe  string  `json:"timeframe"`
	Indicator  string  `json:"indicator"` // RSI | MACD | SMA | EMA (legacy SMA20/EMA9 accepted)
	Period     int     `json:"period"`    // lookback period; 0 = use default
	Condition  string  `json:"condition"` // "above" | "below"
	Threshold  float64 `json:"threshold"`
	AlertMsg   string  `json:"alert_msg"`
	Recurring  bool    `json:"recurring"`

	// CooldownMinutes, LastFiredMS and ConsecutiveErrors: see PriceAlertPayload.
	CooldownMinutes   int   `json:"cooldown_minutes,omitempty"`
	LastFiredMS       int64 `json:"last_fired_ms,omitempty"`
	ConsecutiveErrors int   `json:"consecutive_errors,omitempty"`
}

// EffectiveAlertCooldownMinutes returns the stored cooldown, or the default when
// the payload predates the field (or stores 0).
func EffectiveAlertCooldownMinutes(stored int) int {
	if stored <= 0 {
		return DefaultAlertCooldownMinutes
	}
	return stored
}

// ValidAlertCondition reports whether condition is "above" or "below".
func ValidAlertCondition(condition string) bool {
	return condition == AlertConditionAbove || condition == AlertConditionBelow
}

// ValidAlertIndicatorFamily reports whether family (already normalised to upper
// case, digits stripped) is one the alert handler can compute.
func ValidAlertIndicatorFamily(family string) bool {
	switch family {
	case "RSI", "MACD", "SMA", "EMA":
		return true
	}
	return false
}

// parseAlertCooldown reads the optional cooldown_minutes argument. It returns
// 0 when the argument is absent, and an error when it is present but below 1.
func parseAlertCooldown(args map[string]any) (int, error) {
	v, ok := args["cooldown_minutes"]
	if !ok || v == nil {
		return 0, nil
	}
	var n int
	switch x := v.(type) {
	case float64:
		n = int(x)
		if float64(n) != x {
			return 0, fmt.Errorf("cooldown_minutes must be a whole number (got %v)", x)
		}
	case int:
		n = x
	case int64:
		n = int(x)
	default:
		return 0, errors.New("cooldown_minutes must be an integer >= 1")
	}
	if n < 1 {
		return 0, fmt.Errorf("cooldown_minutes must be >= 1 (got %d)", n)
	}
	return n, nil
}
