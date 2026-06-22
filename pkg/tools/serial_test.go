package tools

import (
	"testing"
	"time"
)

func TestParseSerialConfig(t *testing.T) {
	cfg, errResult := parseSerialConfig(map[string]any{
		"port":      "COM3",
		"baud":      float64(9600),
		"data_bits": float64(7),
		"parity":    "even",
		"stop_bits": float64(2),
	})
	if errResult != nil {
		t.Fatalf("parseSerialConfig() unexpected error = %v", errResult.ForLLM)
	}

	if cfg.Port != "COM3" || cfg.Baud != 9600 || cfg.DataBits != 7 || cfg.Parity != "even" || cfg.StopBits != 2 {
		t.Fatalf("parseSerialConfig() = %#v", cfg)
	}
}

func TestParseSerialConfigRejectsInvalidParity(t *testing.T) {
	_, errResult := parseSerialConfig(map[string]any{
		"port":   "/dev/ttyUSB0",
		"parity": "mark",
	})
	if errResult == nil {
		t.Fatal("expected invalid parity to fail")
	}
}

func TestParseSerialTimeout(t *testing.T) {
	timeout, errResult := parseSerialTimeout(map[string]any{
		"timeout_ms": float64(2500),
	})
	if errResult != nil {
		t.Fatalf("parseSerialTimeout() unexpected error = %v", errResult.ForLLM)
	}
	if timeout != 2500*time.Millisecond {
		t.Fatalf("timeout = %v, want 2500ms", timeout)
	}
}

func TestParseSerialWritePayloadSupportsText(t *testing.T) {
	data, errResult := parseSerialWritePayload(map[string]any{
		"text": "AT\r\n",
	})
	if errResult != nil {
		t.Fatalf("parseSerialWritePayload() unexpected error = %v", errResult.ForLLM)
	}
	if string(data) != "AT\r\n" {
		t.Fatalf("payload = %q, want %q", string(data), "AT\r\n")
	}
}

func TestParseSerialWritePayloadRejectsOutOfRangeByte(t *testing.T) {
	_, errResult := parseSerialWritePayload(map[string]any{
		"data": []any{float64(256)},
	})
	if errResult == nil {
		t.Fatal("expected payload validation failure")
	}
}
