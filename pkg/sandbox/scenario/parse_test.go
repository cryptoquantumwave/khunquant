package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseString(t *testing.T, src string) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func parseFixture(t *testing.T, name string) *File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil || len(data) == 0 {
		t.Fatalf("fixture %s is missing or empty: %v", name, err)
	}
	return parseString(t, string(data))
}

const base = `version: 1
scenario: t
clock: {start: "2026-10-20T09:00:00+07:00"}
`

func TestParse_DesignExamples(t *testing.T) {
	h8 := parseFixture(t, "design_8_1.yaml")
	if h8.Scenario != "alert-survives-channel-outage" || len(h8.Timeline) != 4 || len(h8.Expect) != 4 || len(h8.Setup) != 1 {
		t.Fatalf("8.1 = %q, %d timeline, %d expect, %d setup", h8.Scenario, len(h8.Timeline), len(h8.Expect), len(h8.Setup))
	}
	if s := h8.Timeline[0]; s.At != "+5m" || s.Kind != "chat.channel" {
		t.Errorf("first step = %+v, want at +5m chat.channel", s)
	}
	if s := h8.Timeline[3]; s.At != "" || s.Kind != "run_until" {
		t.Errorf("last step = %+v, want run_until", s)
	}
	if v, ok := h8.Timeline[3].Scalar(); !ok || v != "+2h" {
		t.Errorf("run_until arg = %q", v)
	}
	m := h8.Expect[1]
	if m.ID != "delivered-after-recovery" || m.Kind != "message" || m.Count != "1" || m.Within == nil || m.Within.From != "+30m" {
		t.Errorf("expect[1] = %+v", m)
	}
	if to, _ := h8.Expect[2].Fields["to"].([]any); len(to) != 2 {
		t.Errorf("to = %v, want a list of two", h8.Expect[2].Fields["to"])
	}
	if h8.Instance.Config["proactive"] == nil || h8.Profile["active_hours"] == nil || h8.Account["binance/main"].Balances["BTC"] != 0.1 {
		t.Error("instance.config, profile and account must be kept")
	}
	if mi := h8.Market["BTC/USDT"]; mi.Venue != "binance" || mi.Model != "set" || mi.Args["price"] != 98000 {
		t.Errorf("market = %+v", mi)
	}

	mute := parseFixture(t, "design_8_2.yaml")
	if len(mute.Timeline) != 6 || len(mute.LLMScript) != 1 || mute.Expect[0].Count != "1" || mute.Expect[1].Count != ">= 1" {
		t.Fatalf("8.2 = %d timeline, %d llm_script", len(mute.Timeline), len(mute.LLMScript))
	}
	if mute.Expect[2].Kind != "decision" || mute.Expect[2].Fields["reason"] != "!muted" {
		t.Errorf("decision = %+v", mute.Expect[2])
	}

	hb := parseFixture(t, "design_8_3.yaml")
	if len(hb.Matrix["template"]) != 2 || hb.Runs != 5 || hb.PassRate != 0.8 || hb.LLM != "live" {
		t.Errorf("8.3 = matrix %v, runs %d, pass_rate %v", hb.Matrix, hb.Runs, hb.PassRate)
	}
	if hb.Expect[0].During != "22:00-07:00" || hb.Expect[0].Count != "0" {
		t.Errorf("during/count are lifted out of the kind mapping: %+v", hb.Expect[0])
	}
	if _, ok := hb.Expect[0].Fields["during"]; ok {
		t.Error("during must not stay in Fields")
	}
}

func TestParseTarget(t *testing.T) {
	for _, bad := range []string{"", "tomorrow", "next 25:00", "next 8"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) must fail", bad)
		}
	}
	tg, err := ParseTarget("next 08:00")
	if err != nil || tg.Kind != "next" {
		t.Fatalf("next 08:00 = %+v, %v", tg, err)
	}
	f := parseString(t, base)
	loc := f.StartTime().Location()
	got := tg.Resolve(f.StartTime(), f.StartTime(), loc)
	if want := f.StartTime().AddDate(0, 0, 1).Add(-time.Hour); !got.Equal(want) {
		t.Errorf("next 08:00 from 09:00 = %v, want %v", got, want)
	}
}

func TestDefaultDir(t *testing.T) {
	if DefaultDir != "testdata/scenarios" {
		t.Errorf("DefaultDir = %q", DefaultDir)
	}
}
