package labjournal

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummaryPerKind(t *testing.T) {
	for _, tc := range []struct {
		kind, subject, payload string
		want                   []string // every one must appear
	}{
		{"chat.in", "dm:owner", `{"as":"owner","text":"hello there"}`, []string{"owner", "hello there"}},
		{"chat.out", "dm:owner", `{"text":"BTC crossed","delivered":true,"severity":"warning"}`, []string{"dm:owner", "BTC crossed", "severity=warning"}},
		{"chat.out", "dm:owner", `{"text":"x","delivered":false,"error":"channel down"}`, []string{"not delivered", "channel down"}},
		{"chat.state", "", `{"state":"down"}`, []string{"down"}},
		{"llm.call", "user", `{"turn":"user","rule":"greet","text":"hi"}`, []string{"turn=user", "rule=greet", "hi"}},
		{"llm.complete", "news", `{"caller":"news","rule":"r1","text":"ok"}`, []string{"news", "rule=r1", "ok"}},
		{"workflow.step", "approval.order", `{"event":{"payload":{"step":"place","status":"done"}}}`, []string{"approval.order", "place", "done"}},
		{"proposal", "ABC123", `{"event":{"kind":"approval.decided","payload":{"status":"approved"}}}`, []string{"ABC123", "approved"}},
		{"policy.decision", "create_order", `{"event":{"payload":{"verdict":"allow","tier":"trade"}}}`, []string{"create_order", "allow", "tier=trade"}},
		{"event.trigger.candidate", "BTC", `{"event":{"severity":"notice"}}`, []string{"trigger.candidate", "BTC", "notice"}},
		{"exchange.req", "okx", `{"venue":"okx","method":"GET","path":"/api/v5/x","status":200}`, []string{"okx", "GET", "/api/v5/x", "200"}},
		{"exchange.order", "okx", `{"venue":"okx","event":"fill","side":"buy","amount":1.5,"symbol":"BTC/USDT","status":"filled"}`, []string{"fill", "buy", "1.5", "BTC/USDT", "filled"}},
		{"exchange.funding", "okx", `{"venue":"okx","symbol":"BTC/USDT:USDT","side":"long","rate":0.0001,"payment":-0.5}`, []string{"BTC/USDT:USDT", "payment=-0.5"}},
		{"fault.fired", "okx", `{"rule_id":"f1","effect":"timeout","venue":"okx","method":"POST","path":"/order"}`, []string{"rule=f1", "timeout", "POST"}},
		{"web.req", "x.com", `{"method":"GET","url":"https://x.com/a","status":200}`, []string{"GET", "https://x.com/a", "200"}},
		{"crash", "kill", `{"point":"after_send","workflow_id":"w1"}`, []string{"kill", "point=after_send", "workflow=w1"}},
		{"settle", "", `{"ok":false,"cut":true,"wall_ms":25}`, []string{"ok=false", "cut=true"}},
		{"tool.call", "web_fetch", `{"name":"web_fetch","origin":"user","is_error":true}`, []string{"web_fetch", "(error)"}},
	} {
		got := Summary(Entry{Kind: tc.kind, Subject: tc.subject, Payload: json.RawMessage(tc.payload)})
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: Summary = %q, missing %q", tc.kind, got, w)
			}
		}
	}
}

func TestSummaryEdges(t *testing.T) {
	if got := Summary(Entry{Kind: "some.unknown", Payload: json.RawMessage(`{"a":1}`)}); got != "" {
		t.Errorf("unknown kind = %q, want empty", got)
	}
	for _, p := range []string{``, `null`, `"text"`, `[1,2]`, `{bad`} { // never a panic
		_ = Summary(Entry{Kind: "chat.out", Payload: json.RawMessage(p)})
	}
	long := `{"text":"` + strings.Repeat("é", 500) + "\\n" + `x"}`
	got := Summary(Entry{Kind: "chat.in", Payload: json.RawMessage(long)})
	if n := utf8.RuneCountInString(got); n != SummaryMax || strings.ContainsAny(got, "\n\r") {
		t.Errorf("long summary: %d runes (want %d), newline=%v", n, SummaryMax, strings.ContainsAny(got, "\n\r"))
	}
}
