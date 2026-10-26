package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestProfile_ValuesAndChannelsOff(t *testing.T) {
	home := t.TempDir()
	cfg, err := Profile(ProfileOptions{Home: home, Port: 18791})
	if err != nil {
		t.Fatal(err)
	}

	ws := filepath.Join(home, "workspace")
	if cfg.Agents.Defaults.Workspace != ws || !cfg.Agents.Defaults.RestrictToWorkspace {
		t.Errorf("workspace = %q restrict=%v", cfg.Agents.Defaults.Workspace, cfg.Agents.Defaults.RestrictToWorkspace)
	}
	if cfg.Debug.Sandbox.FixturesDir != filepath.Join(ws, "sandbox") || !cfg.Debug.Sandbox.Enabled || cfg.Debug.Sandbox.Token != "" {
		t.Errorf("sandbox config = %+v", cfg.Debug.Sandbox)
	}
	if !cfg.Debug.DevMCP.Enabled || cfg.Debug.DevMCP.Token != "" {
		t.Errorf("dev_mcp = %+v", cfg.Debug.DevMCP)
	}
	if cfg.Gateway.Host != "127.0.0.1" || cfg.Gateway.Port != 18791 {
		t.Errorf("gateway = %s:%d", cfg.Gateway.Host, cfg.Gateway.Port)
	}
	if len(cfg.ModelList) != 1 || cfg.ModelList[0].Model != "sim/scripted" ||
		cfg.ModelList[0].ModelName != "sim" || cfg.Agents.Defaults.ModelName != cfg.ModelList[0].ModelName {
		t.Errorf("model = %+v default=%q", cfg.ModelList, cfg.Agents.Defaults.ModelName)
	}
	if cfg.Agents.Defaults.FollowUpNudge {
		t.Error("follow_up_nudge is on: one scripted text answer would be two model calls")
	}
	if cfg.Tools.Exec.Enabled || cfg.Tools.MCP.Enabled {
		t.Error("exec or MCP enabled in the profile")
	}
	if cfg.Proactive.Owner.WebIsOwnerEnabled() {
		t.Error("web_is_owner enabled in the profile")
	}

	// simchat is the one chat channel enabled (for everyone), pico included in the rest.
	chans := reflect.ValueOf(cfg.Channels)
	for i := 0; i < chans.NumField(); i++ {
		name := chans.Type().Field(i).Name
		if en := chans.Field(i).FieldByName("Enabled"); en.IsValid() && en.Bool() && name != "SimChat" {
			t.Errorf("channel %s is enabled in the profile", name)
		}
	}
	if sc := cfg.Channels.SimChat; !sc.Enabled || len(sc.AllowFrom) != 1 || sc.AllowFrom[0] != "*" {
		t.Errorf("simchat = %+v, want enabled with allow_from [*]", sc)
	}
	if cfg.Channels.Pico.Enabled {
		t.Error("pico is enabled in the profile")
	}
}

// config.DefaultConfig reads KHUNQUANT_HOME from the building process. The profile
// must not carry any trace of it: everything path-like comes from ProfileOptions.Home.
func TestProfile_DoesNotLeakBuilderHome(t *testing.T) {
	decoy := filepath.Join(t.TempDir(), "decoy-khunquant-home")
	t.Setenv("KHUNQUANT_HOME", decoy)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(decoy, "config.json"))

	cfg, err := Profile(ProfileOptions{Home: filepath.Join(t.TempDir(), "instance"), Port: 18794})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), decoy) {
		t.Errorf("the profile contains the builder's KHUNQUANT_HOME %q", decoy)
	}
	if !strings.Contains(string(raw), "instance") {
		t.Error("the marshalled profile does not even contain the instance home")
	}
}

func TestProfile_RejectsBadOptions(t *testing.T) {
	for name, o := range map[string]ProfileOptions{
		"relative home": {Home: "rel/home", Port: 8000},
		"empty home":    {Home: "", Port: 8000},
		"port zero":     {Home: t.TempDir(), Port: 0},
		"port too big":  {Home: t.TempDir(), Port: 70000},
	} {
		if _, err := Profile(o); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := WriteProfile(o); err == nil {
			t.Errorf("%s: WriteProfile want an error", name)
		}
	}
}

// WriteProfile writes the config and its .security.yml only inside the home, and the
// result loads back unchanged.
func TestWriteProfile_StaysInsideHome(t *testing.T) {
	// A separate real home that must stay untouched even if KHUNQUANT_HOME points at it.
	other := t.TempDir()
	t.Setenv("KHUNQUANT_HOME", other)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(other, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", "")

	home := filepath.Join(t.TempDir(), "instance")
	path, err := WriteProfile(ProfileOptions{Home: home, Port: 18792})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "config.json") {
		t.Fatalf("path = %q", path)
	}

	var got []string
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	if strings.Join(got, ",") != ".security.yml,config.json" {
		t.Errorf("files in the instance home = %v, want only config.json and .security.yml", got)
	}
	if left, _ := os.ReadDir(other); len(left) != 0 {
		t.Errorf("WriteProfile wrote outside the instance home: %v", left)
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("profile does not load: %v", err)
	}
	if loaded.Gateway.Port != 18792 || loaded.ModelList[0].Model != "sim/scripted" {
		t.Errorf("round trip: port=%d model=%q", loaded.Gateway.Port, loaded.ModelList[0].Model)
	}
}
