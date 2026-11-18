package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

const subsystemsPath = "/api/agent/proactive/subsystems"

func (e *proactiveEnv) status(t *testing.T) proactiveStatusResponse {
	t.Helper()
	rec := e.mustDo(t, http.MethodGet, "/api/agent/proactive/status", "", http.StatusOK)
	var s proactiveStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("status: %v (%s)", err, rec.Body.String())
	}
	return s
}

func putSubsystems(t *testing.T, e *proactiveEnv, body string) proactiveStatusResponse {
	t.Helper()
	rec := e.mustDo(t, http.MethodPut, subsystemsPath, body, http.StatusOK)
	var s proactiveStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("response: %v (%s)", err, rec.Body.String())
	}
	return s
}

func hasNote(s proactiveStatusResponse, code string) bool {
	for _, n := range s.Subsystems.Notes {
		if n == code {
			return true
		}
	}
	return false
}

func TestSubsystemsStatus_ReportsDefaults(t *testing.T) {
	e := newProactiveEnv(t)
	s := e.status(t)
	c := s.Subsystems.Configured
	if c.Perception.Enabled || c.Perception.MarketPollSeconds != 60 || c.Perception.ReadsPerMinuteLimit != 30 || c.Perception.SnapshotMinutes != 15 {
		t.Errorf("perception = %+v", c.Perception)
	}
	if c.Triggers.Mode != "off" || c.Approvals.ExpiryMinutes != 10 || c.Cognition.WakesPerHour != 6 ||
		c.News.IntervalMinutes != 60 || c.News.MaxAssets != 8 {
		t.Errorf("configured = %+v", c)
	}
	if len(s.Subsystems.Notes) != 0 {
		t.Errorf("notes on a default install = %v", s.Subsystems.Notes)
	}
	if s.PaperTradingMode {
		t.Error("paper mode reported on for a default config")
	}
}

func TestSubsystemsPut_ReadAndSaveEachGroup(t *testing.T) {
	e := newProactiveEnv(t)

	s := putSubsystems(t, e, `{"perception":{"enabled":true,"market_poll_seconds":45,"reads_per_minute_limit":12,"snapshot_minutes":20}}`)
	if p := s.Subsystems.Configured.Perception; !p.Enabled || p.MarketPollSeconds != 45 || p.ReadsPerMinuteLimit != 12 || p.SnapshotMinutes != 20 {
		t.Errorf("perception = %+v", p)
	}

	putSubsystems(t, e, `{"triggers":{"mode":" LIVE "}}`)
	if got := e.loadCfg(t).Proactive.Triggers.Mode; got != "live" {
		t.Errorf("triggers.mode = %q, want live (normalised)", got)
	}

	putSubsystems(t, e, `{"approvals":{"enabled":true,"expiry_minutes":30}}`)
	if a := e.loadCfg(t).Proactive.Approvals; !a.Enabled || a.ExpiryMinutes != 30 {
		t.Errorf("approvals = %+v", a)
	}

	putSubsystems(t, e, `{"cognition":{"enabled":true,"max_concurrent_llm":2,"wakes_per_hour":9,"tokens_per_day":123456,"proactive_model":"  light-model-1 "}}`)
	if c := e.loadCfg(t).Proactive.Cognition; !c.Enabled || c.MaxConcurrentLLM != 2 || c.WakesPerHour != 9 ||
		c.TokensPerDay != 123456 || c.ProactiveModel != "light-model-1" {
		t.Errorf("cognition = %+v", c)
	}

	putSubsystems(t, e, `{"news":{"enabled":true,"interval_minutes":15,"max_assets":5,"max_parses_per_run":2,"max_parses_per_day":6,"run_seconds":120}}`)
	if n := e.loadCfg(t).Proactive.News; !n.Enabled || n.IntervalMinutes != 15 || n.MaxAssets != 5 ||
		n.MaxParsesPerRun != 2 || n.MaxParsesPerDay != 6 || n.RunSeconds != 120 {
		t.Errorf("news = %+v", n)
	}

	// A later partial save leaves the rest alone, and the status (read back) agrees.
	putSubsystems(t, e, `{"news":{"enabled":false}}`)
	st := e.status(t)
	if st.Subsystems.Configured.News.Enabled || st.Subsystems.Configured.News.IntervalMinutes != 15 ||
		!st.Subsystems.Configured.Cognition.Enabled || st.Subsystems.Configured.Triggers.Mode != "live" {
		t.Errorf("status after partial save = %+v", st.Subsystems.Configured)
	}
	// 0 wakes and 0 parses are allowed ("no budgeted work"), and an empty model clears it.
	putSubsystems(t, e, `{"cognition":{"wakes_per_hour":0,"tokens_per_day":0,"proactive_model":""},"news":{"max_parses_per_run":0,"max_parses_per_day":0}}`)
	if c := e.loadCfg(t).Proactive.Cognition; c.WakesPerHour != 0 || c.TokensPerDay != 0 || c.ProactiveModel != "" {
		t.Errorf("cognition = %+v", c)
	}
}

func TestSubsystemsPut_RefusesInvalidValues(t *testing.T) {
	e := newProactiveEnv(t)
	cases := []struct{ name, body, want string }{
		{"unknown group", `{"mandates":{"enabled":true}}`, "unknown field"},
		{"unknown field", `{"news":{"api_key":"x"}}`, "unknown field"},
		{"another section", `{"tools":{"exec":{"allow_remote":true}}}`, "unknown field"},
		{"wrong type", `{"news":{"max_assets":"many"}}`, "invalid request body"},
		{"fractional", `{"perception":{"market_poll_seconds":1.5}}`, "invalid request body"},
		{"nothing", `{}`, "nothing to change"},
		{"empty group", `{"news":{}}`, "nothing to change"},
		{"bad triggers mode", `{"triggers":{"mode":"loud"}}`, "triggers.mode"},
		{"empty triggers mode", `{"triggers":{"mode":""}}`, "must not be empty"},

		{"poll zero", `{"perception":{"market_poll_seconds":0}}`, "market_poll_seconds"},
		{"poll negative", `{"perception":{"market_poll_seconds":-5}}`, "market_poll_seconds"},
		{"poll huge", `{"perception":{"market_poll_seconds":99999999999}}`, "market_poll_seconds"},
		{"reads zero", `{"perception":{"reads_per_minute_limit":0}}`, "reads_per_minute_limit"},
		{"snapshot negative", `{"perception":{"snapshot_minutes":-1}}`, "snapshot_minutes"},
		{"expiry zero", `{"approvals":{"expiry_minutes":0}}`, "expiry_minutes"},
		{"expiry negative", `{"approvals":{"expiry_minutes":-3}}`, "expiry_minutes"},
		{"concurrent zero", `{"cognition":{"max_concurrent_llm":0}}`, "max_concurrent_llm"},
		{"concurrent huge", `{"cognition":{"max_concurrent_llm":1000}}`, "max_concurrent_llm"},
		{"wakes negative", `{"cognition":{"wakes_per_hour":-1}}`, "wakes_per_hour"},
		{"tokens negative", `{"cognition":{"tokens_per_day":-1}}`, "tokens_per_day"},
		{"model with space", `{"cognition":{"proactive_model":"two words"}}`, "proactive_model"},
		{"model too long", `{"cognition":{"proactive_model":"` + strings.Repeat("m", 201) + `"}}`, "proactive_model"},
		{"news interval below floor", `{"news":{"interval_minutes":9}}`, "interval_minutes"},
		{"news assets zero", `{"news":{"max_assets":0}}`, "max_assets"},
		{"news assets above cap", `{"news":{"max_assets":21}}`, "max_assets"},
		{"news run negative", `{"news":{"max_parses_per_run":-1}}`, "max_parses_per_run"},
		{"news day negative", `{"news":{"max_parses_per_day":-1}}`, "max_parses_per_day"},
		{"news run seconds zero", `{"news":{"run_seconds":0}}`, "run_seconds"},
		{"two bad fields", `{"news":{"interval_minutes":1,"max_assets":99}}`, "max_assets"},
	}
	before, _ := os.ReadFile(e.cfgPath)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, http.MethodPut, subsystemsPath, tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if msg := errorMessage(t, rec); !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not mention %q", msg, tc.want)
			}
		})
	}
	after, _ := os.ReadFile(e.cfgPath)
	if !bytes.Equal(before, after) {
		t.Error("a refused request changed config.json")
	}

	// The limits themselves are accepted (floor and cap).
	putSubsystems(t, e, `{"news":{"interval_minutes":10,"max_assets":20}}`)
	putSubsystems(t, e, `{"perception":{"market_poll_seconds":1,"reads_per_minute_limit":1,"snapshot_minutes":1},"approvals":{"expiry_minutes":1},"cognition":{"max_concurrent_llm":1}}`)
}

func TestSubsystemsPut_CSRFRequired(t *testing.T) {
	e := newProactiveEnv(t)
	rec := e.do(t, http.MethodPut, subsystemsPath, `{"news":{"enabled":true}}`, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("a request with no same-origin header was accepted")
	}
	if e.loadCfg(t).Proactive.News.Enabled {
		t.Error("a refused request changed the config")
	}
}

// An out-of-range value already in the file (hand-edited) must not block saving
// something else.
func TestSubsystemsPut_IgnoresOldValuesOfUntouchedFields(t *testing.T) {
	e := newProactiveEnv(t)
	cfg := e.loadCfg(t)
	cfg.Proactive.News.IntervalMinutes = 5 // below today's floor, legal for the loaders
	if err := config.SaveConfig(e.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	putSubsystems(t, e, `{"news":{"enabled":true,"max_assets":4}}`)
	if n := e.loadCfg(t).Proactive.News; !n.Enabled || n.MaxAssets != 4 || n.IntervalMinutes != 5 {
		t.Errorf("news = %+v", n)
	}
}

func TestSubsystemsPut_CannotTouchOtherConfig(t *testing.T) {
	e := newProactiveEnv(t)
	// The first config.SaveConfig after a load normalises .security.yml (it moves a
	// provider key into its model_list entry); that is true of every save and is not
	// this route's doing. Take that save first, so the comparison below is about the
	// route alone.
	if err := config.SaveConfig(e.cfgPath, e.loadCfg(t)); err != nil {
		t.Fatal(err)
	}
	secPath := filepath.Join(filepath.Dir(e.cfgPath), ".security.yml")
	secBefore, err := os.ReadFile(secPath)
	if err != nil || len(secBefore) == 0 {
		t.Fatalf("the planted credentials are not in %s: %v", secPath, err)
	}
	before := e.loadCfg(t)

	resp := e.mustDo(t, http.MethodPut, subsystemsPath,
		`{"perception":{"enabled":true},"triggers":{"mode":"shadow"},"approvals":{"enabled":true,"expiry_minutes":20},
		  "cognition":{"enabled":true,"proactive_model":"some-model"},"news":{"enabled":true,"max_assets":3}}`, http.StatusOK)

	// Credentials: the security file is byte-identical, and no probe appears in the response.
	secAfter, _ := os.ReadFile(secPath)
	if !bytes.Equal(secBefore, secAfter) {
		t.Errorf(".security.yml changed:\n--- before\n%s\n--- after\n%s", secBefore, secAfter)
	}
	for _, probe := range secretProbes {
		if strings.Contains(resp.Body.String(), probe) {
			t.Errorf("the response carries a credential %q", probe)
		}
	}
	// The response is the status shape: nothing but its known keys.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	for k := range top {
		switch k {
		case "enabled", "owner_configured", "owner", "effective", "configured", "downgrades",
			"validation_error", "subsystems", "paper_trading_mode":
		default:
			t.Errorf("unexpected key %q in the response", k)
		}
	}

	after := e.loadCfg(t)
	want := before.Proactive
	want.Perception.Enabled = true
	want.Triggers.Mode = "shadow"
	want.Approvals.Enabled, want.Approvals.ExpiryMinutes = true, 20
	want.Cognition.Enabled, want.Cognition.ProactiveModel = true, "some-model"
	want.News.Enabled, want.News.MaxAssets = true, 3
	if !reflect.DeepEqual(after.Proactive, want) {
		t.Errorf("proactive = %+v\nwant %+v", after.Proactive, want)
	}
	b, a := *before, *after
	b.Proactive, a.Proactive = config.ProactiveConfig{}, config.ProactiveConfig{}
	bj, _ := json.Marshal(&b)
	aj, _ := json.Marshal(&a)
	if !bytes.Equal(bj, aj) {
		t.Error("the route changed configuration outside proactive")
	}
	// The credentials still load: the save did not blank any.
	if after.Tools.Web.Brave.APIKey != secretProbes["brave"] || after.Providers.Anthropic.APIKey != secretProbes["provider"] {
		t.Error("a credential did not survive the save")
	}
}

func TestSubsystemsStatus_DowngradeReport(t *testing.T) {
	e := newProactiveEnv(t)

	// Proactive off: turning a switch on says why it does nothing.
	s := putSubsystems(t, e, `{"news":{"enabled":true},"perception":{"enabled":true}}`)
	if !hasNote(s, noteProactiveOff) || s.Subsystems.Effective.News || s.Subsystems.Effective.Perception {
		t.Errorf("master off: notes=%v effective=%+v", s.Subsystems.Notes, s.Subsystems.Effective)
	}

	// News without cognition (and without triggers): configured on, effectively off.
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/modes", `{"enabled":true}`, http.StatusOK)
	s = e.status(t)
	if !s.Subsystems.Configured.News.Enabled || s.Subsystems.Effective.News {
		t.Errorf("news configured/effective = %v/%v", s.Subsystems.Configured.News.Enabled, s.Subsystems.Effective.News)
	}
	if !hasNote(s, noteNewsNeedsCognition) || !hasNote(s, noteNewsNeedsTriggers) || hasNote(s, noteProactiveOff) {
		t.Errorf("notes = %v", s.Subsystems.Notes)
	}
	found := false
	for _, d := range s.Downgrades {
		if strings.Contains(d, "news.enabled true") {
			found = true
		}
	}
	if !found {
		t.Errorf("the config package's own downgrade for news is missing: %v", s.Downgrades)
	}

	// Cognition on: news is effective; the cognition note goes, the triggers one stays.
	s = putSubsystems(t, e, `{"cognition":{"enabled":true}}`)
	if !s.Subsystems.Effective.News || hasNote(s, noteNewsNeedsCognition) || !hasNote(s, noteNewsNeedsTriggers) {
		t.Errorf("after cognition: notes=%v effective=%+v", s.Subsystems.Notes, s.Subsystems.Effective)
	}

	// Triggers without perception.
	s = putSubsystems(t, e, `{"perception":{"enabled":false},"triggers":{"mode":"shadow"}}`)
	if !hasNote(s, noteTriggersNeedPercept) || hasNote(s, noteNewsNeedsTriggers) || s.Subsystems.Effective.Perception {
		t.Errorf("triggers without perception: notes=%v effective=%+v", s.Subsystems.Notes, s.Subsystems.Effective)
	}
	s = putSubsystems(t, e, `{"perception":{"enabled":true}}`)
	if hasNote(s, noteTriggersNeedPercept) || !s.Subsystems.Effective.Perception {
		t.Errorf("with perception: notes=%v", s.Subsystems.Notes)
	}
}

func TestSubsystemsStatus_NewsNeedsWebSearch(t *testing.T) {
	e := newProactiveEnv(t)
	putSubsystems(t, e, `{"cognition":{"enabled":true},"triggers":{"mode":"shadow"},"perception":{"enabled":true},"news":{"enabled":true}}`)
	e.mustDo(t, http.MethodPut, "/api/agent/proactive/modes", `{"enabled":true}`, http.StatusOK)

	set := func(mut func(c *config.Config)) {
		cfg := e.loadCfg(t)
		mut(cfg)
		if err := config.SaveConfig(e.cfgPath, cfg); err != nil {
			t.Fatal(err)
		}
	}
	// Make every provider explicit.
	set(func(c *config.Config) {
		w := &c.Tools.Web
		w.Brave.Enabled, w.Tavily.Enabled, w.DuckDuckGo.Enabled = false, false, false
		w.Perplexity.Enabled, w.SearXNG.Enabled, w.GLMSearch.Enabled = false, false, false
	})
	if s := e.status(t); !hasNote(s, noteNewsNeedsWebSearch) {
		t.Errorf("no provider: notes = %v", s.Subsystems.Notes)
	}
	set(func(c *config.Config) { c.Tools.Web.DuckDuckGo.Enabled = true })
	if s := e.status(t); hasNote(s, noteNewsNeedsWebSearch) {
		t.Errorf("duckduckgo on: notes = %v", s.Subsystems.Notes)
	}
	set(func(c *config.Config) { c.Tools.WebFetch.Enabled = false })
	if s := e.status(t); !hasNote(s, noteNewsNeedsWebSearch) {
		t.Errorf("web_fetch off: notes = %v", s.Subsystems.Notes)
	}
}

func TestSubsystemsStatus_PaperMode(t *testing.T) {
	e := newProactiveEnv(t)
	cfg := e.loadCfg(t)
	cfg.TradingRisk.PaperTradingMode = true
	if err := config.SaveConfig(e.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if !e.status(t).PaperTradingMode {
		t.Error("paper mode not reported")
	}
}
