package broker_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestCheckLeverage_ReduceAlwaysAllowed(t *testing.T) {
	for _, allow := range []bool{false, true} {
		cfg := &config.Config{TradingRisk: config.TradingRiskConfig{AllowLeverage: allow}}
		if err := broker.CheckLeverage(cfg, "close futures position", broker.RiskReduce); err != nil {
			t.Errorf("allow_leverage=%v: a risk-reducing action must always pass, got: %v", allow, err)
		}
	}
}

func TestCheckLeverage_IncreaseRequiresOptIn(t *testing.T) {
	off := &config.Config{TradingRisk: config.TradingRiskConfig{AllowLeverage: false}}
	if err := broker.CheckLeverage(off, "open futures position", broker.RiskIncrease); err == nil {
		t.Error("RiskIncrease must be refused when allow_leverage=false")
	}
	on := &config.Config{TradingRisk: config.TradingRiskConfig{AllowLeverage: true}}
	if err := broker.CheckLeverage(on, "open futures position", broker.RiskIncrease); err != nil {
		t.Errorf("RiskIncrease must pass when allow_leverage=true, got: %v", err)
	}
	// The zero value is RiskIncrease: forgetting to classify fails closed.
	var zero broker.RiskDirection
	if err := broker.CheckLeverage(off, "x", zero); err == nil {
		t.Error("the zero RiskDirection must behave as RiskIncrease")
	}
}
