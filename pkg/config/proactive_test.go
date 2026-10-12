package config

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

func validOwner() OwnerConfig {
	return OwnerConfig{Channel: "telegram", ChatID: "123456789", SenderID: "telegram:123456789"}
}

// liveProactive asks for every delivering mode, with a valid owner.
func liveProactive() ProactiveConfig {
	c := defaultProactiveConfig()
	c.Enabled = true
	c.Owner = validOwner()
	c.Heartbeat.Mode = HeartbeatModeSweep
	c.Attention.Mode = ProactiveModeLive
	c.Triggers.Mode = ProactiveModeLive
	c.Approvals.Enabled = true
	c.Mandates.Enabled = true
	return c
}

func TestProactiveDefaults(t *testing.T) {
	want := ProactiveConfig{
		Heartbeat: ProactiveHeartbeatConfig{Mode: "legacy", Model: "primary"},
		Attention: AttentionConfig{
			Mode: "shadow", DailyBudget: 3, DedupWindowHours: 24, GroupWindowSeconds: 60, Digest: true,
		},
		Perception: PerceptionConfig{MarketPollSeconds: 60, ReadsPerMinuteLimit: 30, SnapshotMinutes: 15},
		Triggers:   ModeConfig{Mode: "off"},
		Approvals:  ApprovalsConfig{ExpiryMinutes: 10},
		Cognition:  CognitionConfig{MaxConcurrentLLM: 1, WakesPerHour: 6, TokensPerDay: 200000},
		Policy:     ModeConfig{Mode: "off"},
		Reflection: ReflectionConfig{AtLocal: "21:00"},
		Learning:   LearningConfig{HoldoutRate: 0.05},
	}
	got := DefaultConfig().Proactive
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", got, want)
	}
	if err := ValidateProactive(got); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if e := got.Effective(); len(e.Downgrades) != 0 || e.Enabled || e.OwnerConfigured {
		t.Fatalf("a clean config must not downgrade or enable anything: %+v", e)
	}
}

func TestProactive_LegacyConfigFileGetsDefaults(t *testing.T) {
	want := DefaultConfig().Proactive

	for name, body := range map[string]string{
		"no proactive key": `{"gateway": {"host": "127.0.0.1", "port": 18790}}`,
		"empty section":    `{"proactive": {}}`,
	} {
		cfg, err := LoadConfig(writeRawConfig(t, body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(cfg.Proactive, want) {
			t.Errorf("%s: got %+v, want defaults", name, cfg.Proactive)
		}
	}

	// A partial section overrides only what it names.
	path := writeRawConfig(t, `{"proactive": {"enabled": true, "attention": {"daily_budget": 7},
		"owner": {"channel": "telegram"}}}`)
	for name, load := range map[string]func(string) (*Config, error){
		"LoadConfig": LoadConfig, "LoadConfigSkipSecurity": LoadConfigSkipSecurity,
	} {
		cfg, err := load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p := cfg.Proactive
		if !p.Enabled || p.Attention.DailyBudget != 7 || p.Owner.Channel != "telegram" {
			t.Errorf("%s: partial section not applied: %+v", name, p)
		}
		if p.Attention.Mode != "shadow" || p.Attention.DedupWindowHours != 24 || !p.Attention.Digest ||
			p.Heartbeat.Mode != "legacy" || p.Cognition.TokensPerDay != 200000 {
			t.Errorf("%s: fields absent from the file lost their defaults: %+v", name, p)
		}
	}
}

func TestValidateProactive_GroupChatRejected(t *testing.T) {
	for _, chat := range []string{"-1001234567890", "-5", "abc", "123/4", "0"} {
		c := defaultProactiveConfig()
		c.Owner = validOwner()
		c.Owner.ChatID = chat
		err := ValidateProactive(c)
		if err == nil {
			t.Errorf("chat id %q accepted", chat)
			continue
		}
		if chat == "-1001234567890" && !strings.Contains(err.Error(), "direct chat") {
			t.Errorf("group rejection should say the chat must be direct, got %v", err)
		}
		if chat != "0" && strings.Contains(err.Error(), chat) {
			t.Errorf("error echoes the chat id: %v", err)
		}
		if _, ok := c.OwnerRoute(); ok {
			t.Errorf("chat id %q produced an owner route", chat)
		}
	}
}

func TestValidateProactive_NonCanonicalSenderRejected(t *testing.T) {
	for _, sender := range []string{"123456789", "@alice", "telegram:", ":123", "telegram:alice", "telegram:-5"} {
		c := defaultProactiveConfig()
		c.Owner = validOwner()
		c.Owner.SenderID = sender
		if err := ValidateProactive(c); err == nil {
			t.Errorf("sender %q accepted", sender)
		}
		if _, ok := c.OwnerRoute(); ok {
			t.Errorf("sender %q produced an owner route", sender)
		}
	}
}

func TestValidateProactive_PlatformMismatch(t *testing.T) {
	c := defaultProactiveConfig()
	c.Owner = OwnerConfig{Channel: "discord", ChatID: "555", SenderID: "telegram:123"}
	err := ValidateProactive(c)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("platform mismatch: err = %v", err)
	}
	// Same platform on a non-telegram channel is fine, and an empty owner is valid.
	c.Owner = OwnerConfig{Channel: "discord", ChatID: "555", SenderID: "discord:777"}
	if err := ValidateProactive(c); err != nil {
		t.Fatalf("valid discord owner rejected: %v", err)
	}
	c.Owner = OwnerConfig{}
	if err := ValidateProactive(c); err != nil {
		t.Fatalf("empty owner must be valid: %v", err)
	}
}

func TestValidateProactive_UnknownModes(t *testing.T) {
	cases := map[string]func(*ProactiveConfig){
		"heartbeat.mode":      func(c *ProactiveConfig) { c.Heartbeat.Mode = "live" },
		"heartbeat.model":     func(c *ProactiveConfig) { c.Heartbeat.Model = "fast" },
		"attention.mode":      func(c *ProactiveConfig) { c.Attention.Mode = "sweep" },
		"triggers.mode":       func(c *ProactiveConfig) { c.Triggers.Mode = "loud" },
		"policy.mode":         func(c *ProactiveConfig) { c.Policy.Mode = "governed" },
		"daily_budget":        func(c *ProactiveConfig) { c.Attention.DailyBudget = -1 },
		"dedup_window_hours":  func(c *ProactiveConfig) { c.Attention.DedupWindowHours = -1 },
		"market_poll_seconds": func(c *ProactiveConfig) { c.Perception.MarketPollSeconds = -1 },
		"expiry_minutes":      func(c *ProactiveConfig) { c.Approvals.ExpiryMinutes = -1 },
		"tokens_per_day":      func(c *ProactiveConfig) { c.Cognition.TokensPerDay = -1 },
		"holdout_rate high":   func(c *ProactiveConfig) { c.Learning.HoldoutRate = 1.5 },
		"holdout_rate low":    func(c *ProactiveConfig) { c.Learning.HoldoutRate = -0.1 },
		"at_local":            func(c *ProactiveConfig) { c.Reflection.AtLocal = "9pm" },
		"at_local range":      func(c *ProactiveConfig) { c.Reflection.AtLocal = "25:00" },
	}
	for name, mutate := range cases {
		c := defaultProactiveConfig()
		mutate(&c)
		if err := ValidateProactive(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Every problem is reported, not just the first.
	c := defaultProactiveConfig()
	c.Heartbeat.Mode = "x"
	c.Attention.Mode = "y"
	c.Learning.HoldoutRate = 2
	err := ValidateProactive(c)
	if err == nil || !strings.Contains(err.Error(), "heartbeat.mode") ||
		!strings.Contains(err.Error(), "attention.mode") || !strings.Contains(err.Error(), "holdout_rate") {
		t.Fatalf("want all three problems in the joined error, got %v", err)
	}

	// Boundary values are fine.
	c = defaultProactiveConfig()
	c.Learning.HoldoutRate = 1
	c.Reflection.AtLocal = "00:00"
	c.Attention.DailyBudget = 0
	if err := ValidateProactive(c); err != nil {
		t.Fatalf("boundary values rejected: %v", err)
	}
}

// captureProactiveLogs swaps the log sinks and clears the once-per-process guard.
func captureProactiveLogs(t *testing.T) (errs, warns *[]string) {
	t.Helper()
	var mu sync.Mutex
	errs, warns = new([]string), new([]string)
	origE, origW := proactiveErrorLog, proactiveWarnLog
	proactiveErrorLog = func(m string, _ map[string]any) { mu.Lock(); *errs = append(*errs, m); mu.Unlock() }
	proactiveWarnLog = func(m string, _ map[string]any) { mu.Lock(); *warns = append(*warns, m); mu.Unlock() }
	reset := func() {
		loggedProactive.Range(func(k, _ any) bool { loggedProactive.Delete(k); return true })
	}
	reset()
	t.Cleanup(func() { proactiveErrorLog, proactiveWarnLog = origE, origW; reset() })
	return errs, warns
}

func TestLoadConfig_InvalidProactiveDoesNotFail(t *testing.T) {
	errs, warns := captureProactiveLogs(t)
	const secretChat, secretSender = "-1009988776655", "telegram:424242424"
	path := writeRawConfig(t, `{"proactive": {"enabled": true,
		"owner": {"channel": "telegram", "chat_id": "`+secretChat+`", "sender_id": "`+secretSender+`"},
		"attention": {"mode": "live"}, "heartbeat": {"mode": "bogus"}}}`)

	for name, load := range map[string]func(string) (*Config, error){
		"LoadConfig": LoadConfig, "LoadConfigSkipSecurity": LoadConfigSkipSecurity,
	} {
		cfg, err := load(path)
		if err != nil {
			t.Fatalf("%s must not fail on an invalid proactive section: %v", name, err)
		}
		e := cfg.Proactive.Effective()
		if e.OwnerConfigured {
			t.Errorf("%s: an invalid section must count as no owner", name)
		}
		if e.AttentionMode != "shadow" || e.HeartbeatMode != "legacy" {
			t.Errorf("%s: delivering modes not downgraded: %+v", name, e)
		}
	}

	if len(*errs) != 1 {
		t.Fatalf("want exactly one ERROR across both loads, got %d: %v", len(*errs), *errs)
	}
	for _, line := range append(append([]string{}, *errs...), *warns...) {
		if strings.Contains(line, secretChat) || strings.Contains(line, "424242424") {
			t.Errorf("log line leaks an owner id: %q", line)
		}
	}
	if len(*warns) == 0 {
		t.Error("expected downgrade warnings")
	}
}

func TestOwnerRoute_PeerIDFromSender(t *testing.T) {
	c := defaultProactiveConfig()
	c.Owner = OwnerConfig{Channel: " Telegram ", ChatID: " 123456789 ", SenderID: "Telegram:123456789"}
	r, ok := c.OwnerRoute()
	if !ok {
		t.Fatal("valid owner gave no route")
	}
	if r.Channel != "telegram" || r.ChatID != "123456789" || r.SenderID != "telegram:123456789" || r.PeerID != "123456789" {
		t.Fatalf("route = %+v", r)
	}

	for name, o := range map[string]OwnerConfig{
		"empty":       {},
		"no channel":  {ChatID: "1", SenderID: "telegram:1"},
		"no chat":     {Channel: "telegram", SenderID: "telegram:1"},
		"no sender":   {Channel: "telegram", ChatID: "1"},
		"group":       {Channel: "telegram", ChatID: "-1", SenderID: "telegram:1"},
		"mismatching": {Channel: "discord", ChatID: "1", SenderID: "telegram:1"},
	} {
		c.Owner = o
		if _, ok := c.OwnerRoute(); ok {
			t.Errorf("%s: produced a route", name)
		}
	}
}

func TestEffective_DowngradesWithoutOwner(t *testing.T) {
	states := map[string]func(*ProactiveConfig){
		"disabled":      func(c *ProactiveConfig) { c.Enabled = false },
		"no owner":      func(c *ProactiveConfig) { c.Owner = OwnerConfig{} },
		"invalid owner": func(c *ProactiveConfig) { c.Owner.ChatID = "-100123" },
		"valid owner":   func(*ProactiveConfig) {},
	}
	for _, hb := range []string{HeartbeatModeSweep, HeartbeatModeGoverned} {
		for name, mutate := range states {
			c := liveProactive()
			c.Heartbeat.Mode = hb
			mutate(&c)
			e := c.Effective()
			delivers := name == "valid owner"

			wantAttention, wantTriggers, wantHeartbeat := "shadow", "shadow", "legacy"
			if delivers {
				wantAttention, wantTriggers, wantHeartbeat = "live", "live", hb
			}
			if e.AttentionMode != wantAttention || e.TriggersMode != wantTriggers || e.HeartbeatMode != wantHeartbeat {
				t.Errorf("%s: modes = %s/%s/%s, want %s/%s/%s", name,
					e.AttentionMode, e.TriggersMode, e.HeartbeatMode, wantAttention, wantTriggers, wantHeartbeat)
			}
			if e.Approvals != delivers || e.Mandates != delivers {
				t.Errorf("%s: approvals=%v mandates=%v, want both %v", name, e.Approvals, e.Mandates, delivers)
			}
			wantOwner := name == "valid owner" || name == "disabled"
			if e.OwnerConfigured != wantOwner || e.CanDeliver() != delivers {
				t.Errorf("%s: OwnerConfigured = %v, CanDeliver = %v", name, e.OwnerConfigured, e.CanDeliver())
			}
			wantDown := 0
			if !delivers {
				wantDown = 5 // attention, triggers, heartbeat, approvals, mandates
			}
			if len(e.Downgrades) != wantDown {
				t.Errorf("%s: %d downgrades %q, want %d", name, len(e.Downgrades), e.Downgrades, wantDown)
			}
			if !delivers && e.Owner.ChatID != "" {
				t.Errorf("%s: route exposed without a valid owner: %+v", name, e.Owner)
			}
			for _, d := range e.Downgrades {
				if strings.Contains(d, "123456789") {
					t.Errorf("%s: downgrade leaks owner id: %q", name, d)
				}
			}
		}
	}

	// Heartbeat governed downgrades too, and off/shadow stay as configured.
	c := liveProactive()
	c.Owner = OwnerConfig{}
	c.Heartbeat.Mode = HeartbeatModeGoverned
	c.Attention.Mode = ProactiveModeOff
	c.Triggers.Mode = ProactiveModeShadow
	c.Policy.Mode = ProactiveModeLive
	e := c.Effective()
	if e.HeartbeatMode != "legacy" || e.AttentionMode != "off" || e.TriggersMode != "shadow" || e.PolicyMode != "live" {
		t.Errorf("off/shadow must stay as configured, policy is not a delivery mode: %+v", e)
	}
	if want := "proactive: owner not configured; heartbeat.mode governed → legacy"; !contains(e.Downgrades, want) {
		t.Errorf("downgrades %q lack %q", e.Downgrades, want)
	}

	// Other switches are the configured value AND-ed with Enabled.
	c = defaultProactiveConfig()
	c.Intents.Enabled, c.News.Enabled, c.Perception.Enabled = true, true, true
	c.Cognition.Enabled, c.Reflection.Enabled, c.Learning.Enabled = true, true, true
	if e := c.Effective(); e.Intents || e.News || e.Perception || e.Cognition || e.Reflection || e.Learning {
		t.Errorf("a disabled section must run none of them: %+v", e)
	}
	c.Enabled = true
	if e := c.Effective(); !e.Intents || !e.News || !e.Perception || !e.Cognition || !e.Reflection || !e.Learning {
		t.Errorf("enabled section must keep them: %+v", e)
	}

	// Unknown mode strings fall back to the field default.
	c = defaultProactiveConfig()
	c.Attention.Mode, c.Triggers.Mode, c.Policy.Mode, c.Heartbeat.Mode = "z", "z", "z", "z"
	if e := c.Effective(); e.AttentionMode != "shadow" || e.TriggersMode != "off" ||
		e.PolicyMode != "off" || e.HeartbeatMode != "legacy" {
		t.Errorf("unknown modes did not fall back to defaults: %+v", e)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestEffective_PureFunctionOfConfig(t *testing.T) {
	captureProactiveLogs(t)
	c := liveProactive()
	c.Owner = OwnerConfig{}
	first := c.Effective()
	LogProactiveDowngrades(c) // logging must not change later results
	if second := c.Effective(); !reflect.DeepEqual(first, second) {
		t.Fatalf("Effective changed between calls:\n%+v\n%+v", first, second)
	}

	// A "reload" that fixes the owner takes effect at once, no stale state.
	c.Owner = validOwner()
	if e := c.Effective(); e.AttentionMode != "live" || len(e.Downgrades) != 0 {
		t.Fatalf("fixed owner not reflected: %+v", e)
	}
	// And Effective did not mutate its receiver.
	if c.Attention.Mode != "live" {
		t.Fatal("Effective mutated the config")
	}
	c.Owner = OwnerConfig{}
	if e := c.Effective(); e.AttentionMode != "shadow" {
		t.Fatalf("removed owner not reflected: %+v", e)
	}
}

func TestLogProactiveDowngrades_OncePerDistinctDowngrade(t *testing.T) {
	_, warns := captureProactiveLogs(t)
	c := liveProactive()
	c.Owner = OwnerConfig{}

	LogProactiveDowngrades(c)
	n := len(*warns)
	if n != 5 {
		t.Fatalf("want 5 warnings, got %d: %q", n, *warns)
	}
	LogProactiveDowngrades(c)
	if len(*warns) != n {
		t.Fatalf("a reload with the same downgrades repeated them: %q", *warns)
	}
	c.Triggers.Mode = ProactiveModeShadow // downgrade set shrinks: nothing new
	LogProactiveDowngrades(c)
	if len(*warns) != n {
		t.Fatalf("a shrunk set logged again: %q", *warns)
	}
	c.Enabled = false // same changes, different reason: new messages
	LogProactiveDowngrades(c)
	if len(*warns) == n {
		t.Fatal("a changed downgrade set logged nothing")
	}
	if !strings.HasPrefix((*warns)[0], "proactive: owner not configured; ") {
		t.Errorf("unexpected message %q", (*warns)[0])
	}
}

// Enabling proactive with no usable owner changes no mode when the modes are at
// their defaults, so it is the one case Downgrades cannot announce.
func TestReportProactive_EnabledWithoutOwnerWarnsOnce(t *testing.T) {
	_, warns := captureProactiveLogs(t)
	c := defaultProactiveConfig()
	c.Enabled = true
	c.Owner.Channel = "telegram" // incomplete

	if got := c.Effective().Downgrades; len(got) != 0 {
		t.Fatalf("precondition: default modes must not downgrade, got %q", got)
	}
	reportProactive(c)
	reportProactive(c)
	want := "proactive: owner not configured; proactive delivery disabled"
	if len(*warns) != 1 || (*warns)[0] != want {
		t.Fatalf("warns = %q, want exactly [%q]", *warns, want)
	}

	// A complete owner, or proactive disabled, stays silent.
	_, quiet := captureProactiveLogs(t)
	c.Owner = validOwner()
	reportProactive(c)
	c.Owner = OwnerConfig{}
	c.Enabled = false
	reportProactive(c)
	if len(*quiet) != 0 {
		t.Fatalf("expected silence, got %q", *quiet)
	}
}

func TestProactive_EnvOverride(t *testing.T) {
	t.Setenv("KHUNQUANT_PROACTIVE_ENABLED", "true")
	t.Setenv("KHUNQUANT_PROACTIVE_ATTENTION_DAILY_BUDGET", "9")
	t.Setenv("KHUNQUANT_PROACTIVE_TRIGGERS_MODE", "shadow")
	t.Setenv("KHUNQUANT_PROACTIVE_NEWS_ENABLED", "true")
	t.Setenv("KHUNQUANT_PROACTIVE_LEARNING_HOLDOUT_RATE", "0.2")
	t.Setenv("KHUNQUANT_PROACTIVE_OWNER_CHANNEL", "telegram")
	t.Setenv("KHUNQUANT_PROACTIVE_HEARTBEAT_MODEL", "light")

	cfg, err := LoadConfig(writeRawConfig(t, `{"proactive": {"attention": {"daily_budget": 1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Proactive
	if !p.Enabled || p.Attention.DailyBudget != 9 || p.Triggers.Mode != "shadow" ||
		!p.News.Enabled || p.Learning.HoldoutRate != 0.2 || p.Owner.Channel != "telegram" ||
		p.Heartbeat.Model != "light" {
		t.Fatalf("env overrides not applied: %+v", p)
	}
	// Untouched fields keep their defaults.
	if p.Attention.Mode != "shadow" || p.Heartbeat.Mode != "legacy" || p.Policy.Mode != "off" || p.Mandates.Enabled {
		t.Fatalf("env override disturbed other fields: %+v", p)
	}
}

func TestValidateOwnerInput(t *testing.T) {
	got, err := ValidateOwnerInput(" Telegram ", " 123456789 ", " telegram:123456789 ")
	if err != nil {
		t.Fatalf("valid owner refused: %v", err)
	}
	if got != validOwner() {
		t.Errorf("normalised owner = %+v, want %+v", got, validOwner())
	}

	for name, tc := range map[string]struct{ channel, chat, sender, want string }{
		"group chat":        {"telegram", "-100123", "telegram:123456789", "direct chat"},
		"username sender":   {"telegram", "123456789", "telegram:alice", "numeric user id"},
		"not canonical":     {"telegram", "123456789", "123456789", "canonical"},
		"platform mismatch": {"telegram", "123456789", "discord:123456789", "does not match"},
		"missing channel":   {"", "123456789", "telegram:123456789", "missing: channel"},
		"missing both":      {"telegram", "", " ", "missing: chat id, sender id"},
	} {
		if _, err := ValidateOwnerInput(tc.channel, tc.chat, tc.sender); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}
