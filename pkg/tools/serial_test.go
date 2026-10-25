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

func TestValidateSerialPort(t *testing.T) {
	for _, port := range []string{
		"/dev/ttyUSB0", "/dev/cu.usbserial-0001", "/dev/serial/by-id/usb-FTDI_A1", "COM3", "com12", `\\.\COM10`,
	} {
		if err := validateSerialPort(port); err != nil {
			t.Errorf("%q rejected: %v", port, err)
		}
	}
	for _, port := range []string{
		"/etc/passwd",
		"/dev/../etc/passwd",
		"/dev/ttyUSB0/../../home/me/.ssh/id_rsa",
		"/dev//ttyUSB0",
		"/dev/",
		"ttyUSB0",
		"~/.bashrc",
		"COM3x",
		`\\.\PhysicalDrive0`,
	} {
		if err := validateSerialPort(port); err == nil {
			t.Errorf("%q accepted as a serial device", port)
		}
	}
}

func TestParseSerialConfigRejectsNonDevicePort(t *testing.T) {
	if _, errResult := parseSerialConfig(map[string]any{"port": "/home/me/.bashrc"}); errResult == nil {
		t.Fatal("a regular file was accepted as the serial port")
	}
}
