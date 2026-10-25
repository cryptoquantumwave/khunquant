package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func webOwnerCfg() *Config {
	c := DefaultConfig()
	c.Proactive.Owner = OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	return c
}

func armGate(t *testing.T, ok bool, missing string) {
	t.Helper()
	SetWebLauncherGate(func(string) (bool, string) { return ok, missing })
	t.Cleanup(func() { SetWebLauncherGate(nil) })
}

func TestWebOwnerStatus(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name        string
		cfg         func() *Config
		gate        func(*testing.T)
		wantActive  bool
		wantMissing string
	}{
		{"all hold", webOwnerCfg, func(t *testing.T) { armGate(t, true, "") }, true, ""},
		{"explicitly on", func() *Config { c := webOwnerCfg(); c.Proactive.Owner.WebIsOwner = &on; return c }, func(t *testing.T) { armGate(t, true, "") }, true, ""},
		{"(a) no owner", DefaultConfig, func(t *testing.T) { armGate(t, true, "") }, false, "no valid proactive.owner"},
		{"(a) a partial owner", func() *Config { c := webOwnerCfg(); c.Proactive.Owner.ChatID = ""; return c }, func(t *testing.T) { armGate(t, true, "") }, false, "no valid proactive.owner"},
		{"(c) gate says no", webOwnerCfg, func(t *testing.T) { armGate(t, false, "why") }, false, "why"},
		{"(c) no gate armed", webOwnerCfg, func(t *testing.T) { SetWebLauncherGate(nil) }, false, "could not be established"},
		{"switch off", func() *Config { c := webOwnerCfg(); c.Proactive.Owner.WebIsOwner = &off; return c }, func(t *testing.T) { armGate(t, true, "") }, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.gate(t)
			active, missing := tc.cfg().WebOwnerStatus()
			if active != tc.wantActive || !strings.Contains(missing, tc.wantMissing) || (tc.wantMissing == "" && missing != "") {
				t.Fatalf("(%v, %q), want (%v, %q)", active, missing, tc.wantActive, tc.wantMissing)
			}
		})
	}
	var nilCfg *Config
	if nilCfg.WebOwnerActive() {
		t.Error("a nil config has an active web owner")
	}
}

func TestWebOwnerGateGetsTheGatewayHost(t *testing.T) {
	var got string
	SetWebLauncherGate(func(h string) (bool, string) { got = h; return true, "" })
	t.Cleanup(func() { SetWebLauncherGate(nil) })
	c := webOwnerCfg()
	c.Gateway.Host = " 0.0.0.0 "
	c.WebOwnerActive()
	if got != "0.0.0.0" {
		t.Fatalf("gate saw host %q", got)
	}
}

func TestIsWebOwnerTurn(t *testing.T) {
	armGate(t, true, "")
	c := webOwnerCfg()
	if !c.IsWebOwnerTurn(WebChannel, true) {
		t.Error("authenticated pico turn is not the owner")
	}
	if c.IsWebOwnerTurn(WebChannel, false) || c.IsWebOwnerTurn("telegram", true) || c.IsWebOwnerTurn("", true) {
		t.Error("an unauthenticated connection or another channel counts as the web owner")
	}
}

func TestOwnerDecider(t *testing.T) {
	c := webOwnerCfg()
	armGate(t, true, "")
	if id, ok := c.OwnerDecider("web"); !ok || id != "web" {
		t.Errorf("web decision with the route active: (%q, %v)", id, ok)
	}
	if id, ok := c.OwnerDecider("telegram:1001"); !ok || id != "telegram:1001" {
		t.Errorf("configured decision: (%q, %v)", id, ok)
	}
	armGate(t, false, "x") // the posture changed after the approval
	if id, _ := c.OwnerDecider("web"); id == "web" {
		t.Error("a web decision still counts after the web route went inactive")
	}
	if _, ok := DefaultConfig().OwnerDecider("web"); ok {
		t.Error("a web decision counts with no owner")
	}
}

func TestWebIsOwnerJSONDefaultsOnAndSurvivesRoundTrip(t *testing.T) {
	var o OwnerConfig
	if err := json.Unmarshal([]byte(`{"channel":"telegram","chat_id":"1","sender_id":"telegram:1"}`), &o); err != nil {
		t.Fatal(err)
	}
	if !o.WebIsOwnerEnabled() {
		t.Error("web_is_owner must default to true")
	}
	if b, _ := json.Marshal(o); strings.Contains(string(b), "web_is_owner") {
		t.Errorf("an unset switch is written out: %s", b)
	}
	if err := json.Unmarshal([]byte(`{"web_is_owner":false}`), &o); err != nil {
		t.Fatal(err)
	}
	if o.WebIsOwnerEnabled() {
		t.Error("web_is_owner:false was not read")
	}
	if b, _ := json.Marshal(o); !strings.Contains(string(b), `"web_is_owner":false`) {
		t.Errorf("an explicit false is not written out: %s", b)
	}
}

func TestOwnerWriteKeepsTheWebSwitch(t *testing.T) {
	off := false
	prev := OwnerConfig{Channel: "telegram", ChatID: "1", SenderID: "telegram:1", WebIsOwner: &off}
	fresh, err := ValidateOwnerInput("telegram", "2", "telegram:2")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.WithWebFrom(prev).WebIsOwnerEnabled() {
		t.Error("changing the owner turned an explicit web_is_owner:false back on")
	}
	if (OwnerConfig{}).WithWebFrom(prev).WebIsOwnerEnabled() {
		t.Error("clearing the owner turned an explicit web_is_owner:false back on")
	}
}
