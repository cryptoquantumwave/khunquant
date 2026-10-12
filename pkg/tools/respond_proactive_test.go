package tools

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func proactiveCtx(kind proactive.TurnKind) (context.Context, *proactive.Collector) {
	c := proactive.NewCollector()
	return proactive.WithCollector(WithProactiveTurn(context.Background(), kind), c), c
}

func refs(rs ...ProactiveCandidateRef) []ProactiveCandidateRef { return rs }

func ref(id string, sev proactive.Severity, protective bool) ProactiveCandidateRef {
	return ProactiveCandidateRef{RenderedID: id, ID: id, Severity: sev, Protective: protective}
}

// The tool is never in the global registry, so the schema lint that walks the
// registry does not see it: check it here. Gemini rejects an untyped property.
func TestRespondProactive_SchemaFullyTyped(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	if bad := untypedSchemaPaths(tool.Parameters()); len(bad) != 0 {
		t.Fatalf("untyped schema nodes: %v", bad)
	}
	props := tool.Parameters()["properties"].(map[string]any)
	for _, k := range []string{"decision", "severity", "text", "question", "dedup_key", "candidate_ids"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema lacks %q", k)
		}
	}
	items := props["candidate_ids"].(map[string]any)["items"].(map[string]any)
	if items["type"] != "string" {
		t.Errorf("candidate_ids items = %v", items)
	}
	if tool.Name() != NameRespondProactive || ClassOf(tool.Name()) != ClassNotify {
		t.Errorf("name %q class %v", tool.Name(), ClassOf(tool.Name()))
	}
}

func TestRespondProactive_RefusesWithoutCollector(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	ctx := WithProactiveTurn(context.Background(), proactive.TurnProactive) // no collector
	res := tool.Execute(ctx, map[string]any{"decision": "silent"})
	if !res.IsError || !strings.Contains(res.ForLLM, "collects none") {
		t.Fatalf("got %+v", res)
	}
}

func TestRespondProactive_RefusesOutsideProactiveOrigin(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	for _, origin := range []string{OriginHeartbeat, OriginCron, OriginRemote, OriginInternal, ""} {
		c := proactive.NewCollector()
		ctx := proactive.WithCollector(context.Background(), c)
		ctx = WithToolOrigin(ctx, origin) // "" is a no-op: an untagged turn
		res := tool.Execute(ctx, map[string]any{"decision": "notify", "text": "hi"})
		if !res.IsError || !strings.Contains(res.ForLLM, "only available in a proactive turn") {
			t.Errorf("origin %q: got %+v", origin, res)
		}
		if n := len(c.Decisions()); n != 0 {
			t.Errorf("origin %q: recorded %d decisions", origin, n)
		}
	}
}

func TestRespondProactive_RefusesBadEnums(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	for name, args := range map[string]map[string]any{
		"decision unknown":    {"decision": "shout", "text": "x"},
		"decision wrong case": {"decision": "Notify", "text": "x"},
		"decision missing":    {"text": "x"},
		"decision not string": {"decision": 3},
		"severity unknown":    {"decision": "notify", "text": "x", "severity": "urgent"},
		"severity wrong case": {"decision": "notify", "text": "x", "severity": "Warning"},
		"text not string":     {"decision": "notify", "text": 4},
	} {
		ctx, c := proactiveCtx(proactive.TurnProactive)
		res := tool.Execute(ctx, args)
		if !res.IsError {
			t.Errorf("%s: accepted: %+v", name, res)
		}
		if len(c.Decisions()) != 0 {
			t.Errorf("%s: a refused call recorded a decision", name)
		}
	}
}

func TestRespondProactive_TextRules(t *testing.T) {
	tool := NewRespondProactiveTool(nil, true)
	for _, kind := range []string{"notify", "ask", "propose"} {
		ctx, _ := proactiveCtx(proactive.TurnProactive)
		if res := tool.Execute(ctx, map[string]any{"decision": kind, "text": "  \n "}); !res.IsError {
			t.Errorf("%s with blank text accepted", kind)
		}
	}
	// silent needs none.
	ctx, c := proactiveCtx(proactive.TurnProactive)
	if res := tool.Execute(ctx, map[string]any{"decision": "silent"}); res.IsError {
		t.Fatalf("silent refused: %+v", res)
	}
	if d := c.Decisions(); len(d) != 1 || d[0].Kind != proactive.DecideSilent || d[0].Severity != proactive.SeverityInfo {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestRespondProactive_TextTrimmedTruncatedOnRuneBoundary(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	ctx, c := proactiveCtx(proactive.TurnProactive)
	long := "  " + strings.Repeat("ก", 3000) + "  " // 3 bytes per rune
	res := tool.Execute(ctx, map[string]any{"decision": "notify", "text": long, "question": long})
	if res.IsError {
		t.Fatalf("refused: %+v", res)
	}
	d := c.Decisions()[0]
	for name, got := range map[string]string{"text": d.Text, "question": d.Question} {
		if n := len([]rune(got)); n != MaxProactiveTextRunes {
			t.Errorf("%s has %d runes, want %d", name, n, MaxProactiveTextRunes)
		}
		if strings.ContainsRune(got, '�') {
			t.Errorf("%s was cut inside a rune", name)
		}
	}

	ctx, c = proactiveCtx(proactive.TurnProactive)
	tool.Execute(ctx, map[string]any{"decision": "notify", "text": "  hello\x00 wor\u202eld\r\n  "})
	if got := c.Decisions()[0].Text; got != "hello world" {
		t.Errorf("text = %q, want control characters stripped and trimmed", got)
	}
}

func TestRespondProactive_SeverityClampTable(t *testing.T) {
	const (
		info, notice, warning, critical = proactive.SeverityInfo, proactive.SeverityNotice,
			proactive.SeverityWarning, proactive.SeverityCritical
	)
	candInfo := ref("a", info, false)
	candNotice := ref("b", notice, false)
	candWarn := ref("w", warning, false)
	candCritNP := ref("cn", critical, false) // critical but not protective
	candProt := ref("p", warning, true)      // protective
	candProtInfo := ref("pi", info, true)    // protective, but only info

	cases := []struct {
		name  string
		cands []ProactiveCandidateRef
		ids   []any
		ask   string
		want  proactive.Severity
	}{
		{"no candidates caps at warning", nil, nil, "critical", warning},
		{"no candidates, warning allowed", nil, nil, "warning", warning},
		{"info candidate allows notice", refs(candInfo), []any{"a"}, "warning", notice},
		{"info candidate, info stays", refs(candInfo), []any{"a"}, "info", info},
		{"notice candidate allows warning", refs(candNotice), []any{"b"}, "critical", warning},
		{"warning candidate, not protective, no critical", refs(candWarn), []any{"w"}, "critical", warning},
		{"critical non-protective candidate, no critical", refs(candCritNP), []any{"cn"}, "critical", warning},
		{"protective warning allows critical", refs(candProt), []any{"p"}, "critical", critical},
		{"protective info still capped by its severity", refs(candProtInfo), []any{"pi"}, "critical", notice},
		{"empty ids use all candidates", refs(candInfo, candNotice), nil, "critical", warning},
		{"cites only the low one", refs(candInfo, candWarn), []any{"a"}, "warning", notice},
		{"cites the high one", refs(candInfo, candWarn), []any{"w"}, "critical", warning},
		{"protective elsewhere does not help a non-protective citation", refs(candWarn, candProt), []any{"w"}, "critical", warning},
		{"all-unknown ids fall back to all candidates", refs(candProt), []any{"zzz"}, "critical", critical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewRespondProactiveTool(tc.cands, false)
			ctx, c := proactiveCtx(proactive.TurnProactive)
			args := map[string]any{"decision": "notify", "text": "x", "severity": tc.ask}
			if tc.ids != nil {
				args["candidate_ids"] = tc.ids
			}
			res := tool.Execute(ctx, args)
			if res.IsError {
				t.Fatalf("refused: %+v", res)
			}
			if got := c.Decisions()[0].Severity; got != tc.want {
				t.Errorf("severity = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRespondProactive_UnknownCandidateIDsDropped(t *testing.T) {
	tool := NewRespondProactiveTool(refs(
		ProactiveCandidateRef{RenderedID: "r1", ID: "orig-1", Severity: proactive.SeverityNotice},
		ref("b", proactive.SeverityNotice, false),
	), false)
	ctx, c := proactiveCtx(proactive.TurnProactive)
	res := tool.Execute(ctx, map[string]any{
		"decision": "notify", "text": "x",
		"candidate_ids": []any{"r1", "nope", "b", "r1", 7, "orig-1"},
	})
	if res.IsError || !strings.Contains(res.ForLLM, "not in this request") {
		t.Fatalf("result = %+v", res)
	}
	// Matched against what was rendered, reported by the candidate's own ID,
	// without duplicates; "orig-1" was never shown to the model.
	if got := c.Decisions()[0].CandidateIDs; !reflect.DeepEqual(got, []string{"orig-1", "b"}) {
		t.Errorf("CandidateIDs = %v", got)
	}
}

func TestRespondProactive_DedupKeyNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"SOL:Drop":                      "sol:drop",
		"  sol drop  ":                  "sol_drop",
		"btc/usdt?!#:tier-2_x":          "btcusdt:tier-2_x",
		"":                              "",
		"ÄÖ$$":                          "",
		strings.Repeat("a", 100):        strings.Repeat("a", 64),
		"ab" + strings.Repeat("-", 100): "ab" + strings.Repeat("-", 62),
	} {
		if got := normalizeDedupKey(in); got != want {
			t.Errorf("normalizeDedupKey(%q) = %q, want %q", in, got, want)
		}
	}
	tool := NewRespondProactiveTool(nil, false)
	ctx, c := proactiveCtx(proactive.TurnProactive)
	tool.Execute(ctx, map[string]any{"decision": "notify", "text": "x", "dedup_key": "SOL:Drop!"})
	if got := c.Decisions()[0].DedupKey; got != "sol:drop" {
		t.Errorf("DedupKey = %q", got)
	}
}

func TestRespondProactive_ProposeDowngradedUnlessApprovals(t *testing.T) {
	for _, approvals := range []bool{false, true} {
		tool := NewRespondProactiveTool(nil, approvals)
		ctx, c := proactiveCtx(proactive.TurnProactive)
		res := tool.Execute(ctx, map[string]any{"decision": "propose", "text": "close the hedge"})
		if res.IsError {
			t.Fatalf("approvals=%v refused: %+v", approvals, res)
		}
		want := proactive.DecideAsk
		if approvals {
			want = proactive.DecidePropose
		}
		if got := c.Decisions()[0].Kind; got != want {
			t.Errorf("approvals=%v: kind = %s, want %s", approvals, got, want)
		}
	}
}

func TestRespondProactive_DecisionCap(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	ctx, c := proactiveCtx(proactive.TurnProactive)
	var refused int
	for i := 0; i < proactive.MaxDecisionsPerTurn+4; i++ {
		if res := tool.Execute(ctx, map[string]any{"decision": "notify", "text": "t"}); res.IsError {
			refused++
			if !strings.Contains(res.ForLLM, "at most") {
				t.Errorf("refusal does not say why: %q", res.ForLLM)
			}
		}
	}
	if got := len(c.Decisions()); got != proactive.MaxDecisionsPerTurn || refused != 4 {
		t.Errorf("kept %d refused %d, want %d and 4", got, refused, proactive.MaxDecisionsPerTurn)
	}
}

func TestRespondProactive_ConcurrentCalls(t *testing.T) {
	tool := NewRespondProactiveTool(nil, false)
	ctx, c := proactiveCtx(proactive.TurnProactive)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tool.Execute(ctx, map[string]any{"decision": "notify", "text": "t"})
		}()
	}
	wg.Wait()
	if got := len(c.Decisions()); got != proactive.MaxDecisionsPerTurn {
		t.Errorf("kept %d", got)
	}
}
