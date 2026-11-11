package labjournal

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SummaryMax is the most runes Summary returns.
const SummaryMax = 160

// Summary is a one-line, human-readable account of e, at most SummaryMax runes, or ""
// for a kind it has no line for (a reader falls back to the subject). It reads the
// payload shapes the producers write; a field that is missing or of another type is
// left out, never an error. Text a person or a model wrote (a message, an answer) is
// included as data: callers render it as plain text only.
func Summary(e Entry) string {
	var doc any
	_ = json.Unmarshal(e.Payload, &doc)
	g := func(path string) string { // a scalar at a dotted path, "" when absent
		v, ok := Lookup(doc, path)
		if !ok || v == nil {
			return ""
		}
		switch t := v.(type) {
		case string:
			return t
		case float64:
			return fmt.Sprint(t)
		case bool:
			return fmt.Sprint(t)
		}
		return ""
	}
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, " ")
	}
	kv := func(k string, paths ...string) string { // k=<first non-empty path>
		for _, p := range paths {
			if v := g(p); v != "" {
				return k + "=" + v
			}
		}
		return ""
	}

	var s string
	switch k := e.Kind; {
	case k == "chat.in":
		s = join(g("as"), ":", g("text"), g("ignored"))
	case k == "chat.out":
		s = join(e.Subject, ":", g("text"), kv("severity", "severity"))
		if g("delivered") == "false" {
			s = join("[not delivered]", s, g("error"))
		}
	case k == "chat.edit":
		s = join(e.Subject, "edit:", g("text"))
	case k == "chat.press":
		s = join(e.Subject, "press", g("action"))
	case k == "chat.react":
		s = join(e.Subject, "react", g("emoji"))
	case k == "chat.state":
		s = join("chat", g("state"), kv("delay", "delay"), kv("error", "error"))
	case k == "llm.call":
		s = join(kv("turn", "turn"), kv("rule", "rule"), "->", g("text"), g("error"))
	case k == "llm.complete":
		s = join(g("caller"), kv("rule", "rule"), "->", g("text"), g("error"))
	case k == "llm.budget":
		s = join(g("event"), kv("spent_usd", "spent_usd"), kv("limit_usd", "limit_usd"))
	case k == "tool.call":
		s = join(g("name"), kv("origin", "origin"))
		if g("is_error") == "true" {
			s += " (error)"
		}
	case k == "workflow.step":
		s = join(e.Subject, g("event.payload.step"), g("event.payload.name"), g("event.payload.status"), g("event.payload.state"))
	case k == "proposal":
		s = join(e.Subject, kv("kind", "event.payload.kind"), g("event.kind"), g("event.payload.status"))
	case k == "policy.decision":
		s = join(e.Subject, g("event.payload.verdict"), g("event.payload.decision"), kv("tier", "event.payload.tier"))
	case strings.HasPrefix(k, "event."):
		s = join(strings.TrimPrefix(k, "event."), e.Subject, g("event.severity"))
	case k == "exchange.req":
		s = join(g("venue"), g("method"), g("path"), "->", g("status"), kv("stage", "stage"))
	case k == "exchange.order":
		s = join(g("venue"), g("event"), g("side"), g("amount"), g("symbol"), kv("avg", "avg"), g("status"))
	case k == "exchange.funding":
		s = join(g("venue"), g("symbol"), g("side"), kv("rate", "rate"), kv("payment", "payment"))
	case k == "fault.fired":
		s = join(kv("rule", "rule_id"), g("effect"), g("venue"), g("method"), g("path"), g("order_op"))
	case k == "web.req":
		s = join(g("method"), g("url"), "->", g("status"))
	case k == "crash":
		s = join(e.Subject, kv("point", "point"), kv("workflow", "workflow_id"), kv("step", "step"))
	case k == "settle":
		s = "settle ok=" + g("ok")
		s = join(s, kv("cut", "cut"), kv("wall_ms", "wall_ms"))
	case k == "clock.step":
		s = join("clock", g("at"), kv("timers_fired", "timers_fired"))
	case k == "config.reload":
		s = join("config reload ok="+g("ok"), g("error"))
	case k == "manage.call":
		s = join("manage", e.Subject)
	}
	s = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(strings.ReplaceAll(s, " :", ":"))
	if r := []rune(s); len(r) > SummaryMax {
		s = string(r[:SummaryMax-1]) + "…"
	}
	return s
}
