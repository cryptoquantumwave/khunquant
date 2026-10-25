package config

import (
	"encoding/json"
	"testing"
)

func TestDefaultConfig_LossGuardDefaults(t *testing.T) {
	tr := DefaultConfig().TradingRisk
	if tr.LossDayTimezone != "UTC" {
		t.Errorf("LossDayTimezone default = %q, want UTC", tr.LossDayTimezone)
	}
	if tr.EquitySampleMinutes != 5 {
		t.Errorf("EquitySampleMinutes default = %d, want 5", tr.EquitySampleMinutes)
	}
}

func TestTradingRiskConfig_LossGuardJSONKeys(t *testing.T) {
	var tr TradingRiskConfig
	if err := json.Unmarshal([]byte(`{"loss_day_timezone":"Asia/Bangkok","equity_sample_minutes":10}`), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.LossDayTimezone != "Asia/Bangkok" || tr.EquitySampleMinutes != 10 {
		t.Errorf("unexpected values: %+v", tr)
	}
}
