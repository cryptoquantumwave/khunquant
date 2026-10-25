package bus

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// legacyOutbound is OutboundMessage exactly as it was before the proactive
// metadata was added. Plain replies must serialise to the same bytes.
type legacyOutbound struct {
	Channel          string            `json:"channel"`
	ChatID           string            `json:"chat_id"`
	Content          string            `json:"content"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	ContextUsage     *ContextUsage     `json:"context_usage,omitempty"`
	Kind             string            `json:"kind,omitempty"`
	ModelName        string            `json:"model_name,omitempty"`
	ToolCalls        []VisibleToolCall `json:"tool_calls,omitempty"`
}

func TestOutboundMessage_PlainReplyJSONUnchanged(t *testing.T) {
	cases := map[string]struct {
		now    OutboundMessage
		legacy legacyOutbound
	}{
		"minimal": {
			OutboundMessage{Channel: "telegram", ChatID: "1", Content: "hi"},
			legacyOutbound{Channel: "telegram", ChatID: "1", Content: "hi"},
		},
		"fully populated": {
			OutboundMessage{
				Channel: "pico", ChatID: "2", Content: "x", ReplyToMessageID: "9",
				ContextUsage: &ContextUsage{UsedTokens: 1, TotalTokens: 2, CompressAtTokens: 3, UsedPercent: 50},
				Kind:         "tool_calls", ModelName: "m",
				ToolCalls: []VisibleToolCall{{ID: "c1", Type: "function", Function: &VisibleToolCallFunction{Name: "n"}}},
			},
			legacyOutbound{
				Channel: "pico", ChatID: "2", Content: "x", ReplyToMessageID: "9",
				ContextUsage: &ContextUsage{UsedTokens: 1, TotalTokens: 2, CompressAtTokens: 3, UsedPercent: 50},
				Kind:         "tool_calls", ModelName: "m",
				ToolCalls: []VisibleToolCall{{ID: "c1", Type: "function", Function: &VisibleToolCallFunction{Name: "n"}}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(tc.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(tc.legacy)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("plain reply JSON changed:\n got  %s\n want %s", got, want)
			}
			for _, field := range []string{"severity", "dedup_key", "intervention_id", "actions", "disable_notification"} {
				if strings.Contains(string(got), field) {
					t.Errorf("plain reply serialises %q: %s", field, got)
				}
			}
		})
	}
}

func TestOutboundMessage_ProactiveMetadataRoundTrips(t *testing.T) {
	in := OutboundMessage{
		Channel: "telegram", ChatID: "1", Content: "alert",
		Severity: proactive.SeverityCritical, DedupKey: "pnl:BTC", InterventionID: "iv-1",
		Actions:             []Action{{ID: "approve", Label: "Approve", Style: "primary", Payload: "p", TextCommand: "/approve 7F3K"}},
		DisableNotification: true,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"severity":"critical"`, `"dedup_key":"pnl:BTC"`, `"intervention_id":"iv-1"`,
		`"actions":[`, `"text_command":"/approve 7F3K"`, `"disable_notification":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}

	var out OutboundMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Severity != proactive.SeverityCritical || out.InterventionID != "iv-1" || len(out.Actions) != 1 ||
		out.Actions[0] != in.Actions[0] || !out.DisableNotification {
		t.Fatalf("round trip lost data: %+v", out)
	}
}

func TestOutboundMessage_IsProactive(t *testing.T) {
	// Severity's zero value is a real severity (info), so it cannot mark a
	// message as proactive; the intervention ID does.
	if (OutboundMessage{Content: "hi"}).IsProactive() {
		t.Error("an ordinary reply must not be proactive")
	}
	if (OutboundMessage{Severity: proactive.SeverityWarning}).IsProactive() {
		t.Error("a severity alone must not make a message proactive")
	}
	info := OutboundMessage{InterventionID: "iv-1"}
	if !info.IsProactive() {
		t.Error("an info-severity intervention must be proactive")
	}
	// An info intervention omits severity from JSON and still reads back as info.
	raw, _ := json.Marshal(info)
	var back OutboundMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Severity != proactive.SeverityInfo || !back.IsProactive() {
		t.Errorf("info intervention did not round-trip: %+v", back)
	}
}
