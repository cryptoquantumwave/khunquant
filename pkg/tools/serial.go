package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultSerialBaud      = 115200
	defaultSerialDataBits  = 8
	defaultSerialStopBits  = 1
	defaultSerialTimeoutMS = 1000
	maxSerialPayloadBytes  = 4096
	maxSerialReadBytes     = 4096
)

type SerialTool struct{}

type serialPortInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type serialConfig struct {
	Port     string
	Baud     int
	DataBits int
	Parity   string
	StopBits int
}

func NewSerialTool() *SerialTool {
	return &SerialTool{}
}

func (t *SerialTool) Name() string {
	return NameSerial
}

func (t *SerialTool) Description() string {
	return "Interact with host serial ports. Actions: list (enumerate ports), read (receive bytes), write (send bytes with explicit confirmation). Supports Linux, macOS, and Windows."
}

func (t *SerialTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "read", "write"},
				"description": "Action to perform: list available serial ports, read bytes from a port, or write bytes to a port.",
			},
			"port": map[string]any{
				"type":        "string",
				"description": "Serial port path or name, for example /dev/ttyUSB0, /dev/cu.usbserial-0001, or COM3. Required for read/write.",
			},
			"baud": map[string]any{
				"type":        "integer",
				"description": "Baud rate. Default: 115200.",
			},
			"data_bits": map[string]any{
				"type":        "integer",
				"description": "Data bits. Supported values: 5, 6, 7, 8. Default: 8.",
			},
			"parity": map[string]any{
				"type":        "string",
				"enum":        []string{"none", "even", "odd"},
				"description": "Parity mode. Default: none.",
			},
			"stop_bits": map[string]any{
				"type":        "integer",
				"description": "Stop bits. Supported values: 1, 2. Default: 1.",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "Read/write timeout in milliseconds. Default: 1000.",
			},
			"length": map[string]any{
				"type":        "integer",
				"description": "Number of bytes to read. Required for read. Range: 1-4096.",
			},
			"data": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "Bytes to write, each in range 0-255. Required for write unless text is provided.",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "UTF-8 text to write. Required for write if data is omitted.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true for write operations.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *SerialTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, ok := args["action"].(string)
	if !ok || strings.TrimSpace(action) == "" {
		return ErrorResult("action is required")
	}

	switch action {
	case "list":
		return t.list()
	case "read":
		return t.read(args)
	case "write":
		return t.write(args)
	default:
		return ErrorResult(fmt.Sprintf("unknown action: %s (valid: list, read, write)", action))
	}
}

func (t *SerialTool) list() *ToolResult {
	ports, err := serialListPorts()
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to list serial ports: %v", err))
	}
	if len(ports) == 0 {
		return SilentResult("No serial ports found on this host.")
	}

	result, _ := json.MarshalIndent(map[string]any{
		"ports": ports,
		"count": len(ports),
	}, "", "  ")
	return SilentResult(string(result))
}

func (t *SerialTool) read(args map[string]any) *ToolResult {
	cfg, errResult := parseSerialConfig(args)
	if errResult != nil {
		return errResult
	}

	length := 0
	if v, ok := args["length"].(float64); ok {
		length = int(v)
	}
	if length < 1 || length > maxSerialReadBytes {
		return ErrorResult(fmt.Sprintf("length is required for read (1-%d)", maxSerialReadBytes))
	}

	timeout, errResult := parseSerialTimeout(args)
	if errResult != nil {
		return errResult
	}

	data, err := serialRead(cfg, length, timeout)
	if err != nil {
		return ErrorResult(fmt.Sprintf("serial read failed on %s: %v", cfg.Port, err))
	}

	return SilentResult(formatSerialPayload("read", cfg, data, timeout))
}

func (t *SerialTool) write(args map[string]any) *ToolResult {
	confirm, _ := args["confirm"].(bool)
	if !confirm {
		return ErrorResult(
			"write operations require confirm: true. Please confirm with the user before sending bytes to a serial device.",
		)
	}

	cfg, errResult := parseSerialConfig(args)
	if errResult != nil {
		return errResult
	}
	timeout, errResult := parseSerialTimeout(args)
	if errResult != nil {
		return errResult
	}
	payload, errResult := parseSerialWritePayload(args)
	if errResult != nil {
		return errResult
	}

	written, err := serialWrite(cfg, payload, timeout)
	if err != nil {
		return ErrorResult(fmt.Sprintf("serial write failed on %s: %v", cfg.Port, err))
	}

	result, _ := json.MarshalIndent(map[string]any{
		"action":     "write",
		"port":       cfg.Port,
		"baud":       cfg.Baud,
		"data_bits":  cfg.DataBits,
		"parity":     cfg.Parity,
		"stop_bits":  cfg.StopBits,
		"timeout_ms": timeout.Milliseconds(),
		"written":    written,
		"payload":    serialPayloadSummary(payload),
	}, "", "  ")
	return SilentResult(string(result))
}

func parseSerialConfig(args map[string]any) (serialConfig, *ToolResult) {
	port, ok := args["port"].(string)
	port = strings.TrimSpace(port)
	if !ok || port == "" {
		return serialConfig{}, ErrorResult("port is required (for example /dev/ttyUSB0, /dev/cu.usbserial-0001, or COM3)")
	}

	cfg := serialConfig{
		Port:     port,
		Baud:     defaultSerialBaud,
		DataBits: defaultSerialDataBits,
		Parity:   "none",
		StopBits: defaultSerialStopBits,
	}

	if v, ok := args["baud"].(float64); ok {
		cfg.Baud = int(v)
	}
	if cfg.Baud < 50 || cfg.Baud > 4000000 {
		return serialConfig{}, ErrorResult("baud must be between 50 and 4000000")
	}

	if v, ok := args["data_bits"].(float64); ok {
		cfg.DataBits = int(v)
	}
	switch cfg.DataBits {
	case 5, 6, 7, 8:
	default:
		return serialConfig{}, ErrorResult("data_bits must be one of 5, 6, 7, or 8")
	}

	if v, ok := args["parity"].(string); ok && strings.TrimSpace(v) != "" {
		cfg.Parity = strings.ToLower(strings.TrimSpace(v))
	}
	switch cfg.Parity {
	case "none", "even", "odd":
	default:
		return serialConfig{}, ErrorResult(`parity must be one of "none", "even", or "odd"`)
	}

	if v, ok := args["stop_bits"].(float64); ok {
		cfg.StopBits = int(v)
	}
	if cfg.StopBits != 1 && cfg.StopBits != 2 {
		return serialConfig{}, ErrorResult("stop_bits must be 1 or 2")
	}

	return cfg, nil
}

func parseSerialTimeout(args map[string]any) (time.Duration, *ToolResult) {
	timeoutMS := defaultSerialTimeoutMS
	if v, ok := args["timeout_ms"].(float64); ok {
		timeoutMS = int(v)
	}
	if timeoutMS < 1 || timeoutMS > 60000 {
		return 0, ErrorResult("timeout_ms must be between 1 and 60000")
	}
	return time.Duration(timeoutMS) * time.Millisecond, nil
}

func parseSerialWritePayload(args map[string]any) ([]byte, *ToolResult) {
	if text, ok := args["text"].(string); ok && text != "" {
		if !utf8.ValidString(text) {
			return nil, ErrorResult("text must be valid UTF-8")
		}
		if len(text) > maxSerialPayloadBytes {
			return nil, ErrorResult(fmt.Sprintf("text payload too large: maximum %d bytes", maxSerialPayloadBytes))
		}
		return []byte(text), nil
	}

	dataRaw, ok := args["data"].([]any)
	if !ok || len(dataRaw) == 0 {
		return nil, ErrorResult("write requires either text or data")
	}
	if len(dataRaw) > maxSerialPayloadBytes {
		return nil, ErrorResult(fmt.Sprintf("data too long: maximum %d bytes", maxSerialPayloadBytes))
	}

	data := make([]byte, len(dataRaw))
	for i, v := range dataRaw {
		f, ok := v.(float64)
		if !ok {
			return nil, ErrorResult(fmt.Sprintf("data[%d] is not a valid byte value", i))
		}
		b := int(f)
		if b < 0 || b > 255 {
			return nil, ErrorResult(fmt.Sprintf("data[%d] = %d is out of byte range (0-255)", i, b))
		}
		data[i] = byte(b)
	}

	return data, nil
}

func formatSerialPayload(action string, cfg serialConfig, data []byte, timeout time.Duration) string {
	result, _ := json.MarshalIndent(map[string]any{
		"action":     action,
		"port":       cfg.Port,
		"baud":       cfg.Baud,
		"data_bits":  cfg.DataBits,
		"parity":     cfg.Parity,
		"stop_bits":  cfg.StopBits,
		"timeout_ms": timeout.Milliseconds(),
		"payload":    serialPayloadSummary(data),
	}, "", "  ")
	return string(result)
}

func serialPayloadSummary(data []byte) map[string]any {
	hexValues := make([]string, len(data))
	intValues := make([]int, len(data))
	for i, b := range data {
		hexValues[i] = fmt.Sprintf("0x%02x", b)
		intValues[i] = int(b)
	}

	summary := map[string]any{
		"length": len(data),
		"bytes":  intValues,
		"hex":    hexValues,
	}
	if utf8.Valid(data) {
		summary["text"] = string(data)
	}
	return summary
}
