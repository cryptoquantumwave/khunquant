package supervisor

import (
	"strings"
	"testing"
)

func TestCheckConfigPatch(t *testing.T) {
	cases := []struct {
		name  string
		patch map[string]any
		want  string // "" = accepted; else the dotted path of the one issue
	}{
		{"plain", map[string]any{"proactive": map[string]any{"enabled": true}}, ""},
		{"loopback url", map[string]any{"x": map[string]any{"base_url": "http://127.0.0.1:9/v1"}}, ""},
		{"lock key", map[string]any{"debug": map[string]any{"sandbox": map[string]any{"enabled": false}}}, "debug.sandbox"},
		{"gateway", map[string]any{"gateway": map[string]any{"port": 1}}, "gateway"},
		{"workspace", map[string]any{"agents": map[string]any{"defaults": map[string]any{"workspace": "/tmp/x"}}}, "agents.defaults.workspace"},
		{"credential", map[string]any{"exchanges": map[string]any{"okx": map[string]any{"api_key": "k"}}}, "exchanges.okx.api_key"},
		{"real url", map[string]any{"a": []any{map[string]any{"b": "https://api.binance.com/x"}}}, "a.0.b"},
		{"real channel", map[string]any{"channels": map[string]any{"telegram": map[string]any{"enabled": true}}}, "channels.telegram"},
		{"channel off", map[string]any{"channels": map[string]any{"telegram": map[string]any{"enabled": false}}}, ""},
		{"simchat", map[string]any{"channels": map[string]any{"simchat": map[string]any{"enabled": true}}}, ""},
		// encoding/json decodes keys case-insensitively, so the case of a key protects nothing.
		{"gateway upper", map[string]any{"GATEWAY": map[string]any{"port": 7}}, "GATEWAY"},
		{"gateway title", map[string]any{"Gateway": map[string]any{"port": 1}}, "Gateway"},
		{"debug sandbox title", map[string]any{"Debug": map[string]any{"Sandbox": map[string]any{"enabled": false}}}, "Debug.Sandbox"},
		{"workspace title", map[string]any{"Agents": map[string]any{"Defaults": map[string]any{"Workspace": "/tmp/x"}}}, "Agents.Defaults.Workspace"},
		{"credential upper", map[string]any{"tools": map[string]any{"API_KEY": "real"}}, "tools.API_KEY"},
		{"credential mixed", map[string]any{"x": map[string]any{"Bot_Token": "t"}}, "x.Bot_Token"},
		{"channel title", map[string]any{"Channels": map[string]any{"telegram": map[string]any{"enabled": true}}}, "Channels.telegram"},
		{"channel name upper", map[string]any{"channels": map[string]any{"TELEGRAM": map[string]any{"enabled": true}}}, "channels.TELEGRAM"},
		{"channel off spelled", map[string]any{"channels": map[string]any{"telegram": map[string]any{"Enabled": false}}}, ""},
		{"simchat upper", map[string]any{"Channels": map[string]any{"SimChat": map[string]any{"enabled": true}}}, ""},
		{"kelvin sign folds to k", map[string]any{"tools": map[string]any{"api_\u212aey": "real"}}, "tools.api_\u212aey"},
		// An ancestor of a protected path may not be nulled or replaced either.
		{"debug null", map[string]any{"debug": nil}, "debug"},
		{"agents null", map[string]any{"agents": nil}, "agents"},
		{"defaults null", map[string]any{"agents": map[string]any{"defaults": nil}}, "agents.defaults"},
		{"defaults scalar upper", map[string]any{"AGENTS": map[string]any{"Defaults": "x"}}, "AGENTS.Defaults"},
		{"sandbox null", map[string]any{"debug": map[string]any{"sandbox": nil}}, "debug.sandbox"},
		{"channels null", map[string]any{"channels": nil}, "channels"},
		{"channels scalar", map[string]any{"channels": "off"}, "channels"},
		{"simchat null", map[string]any{"channels": map[string]any{"simchat": nil}}, "channels.simchat"},
		{"simchat scalar", map[string]any{"channels": map[string]any{"simchat": false}}, "channels.simchat"},
		// What stays allowed.
		{"web search", map[string]any{"tools": map[string]any{"web_search": map[string]any{"enabled": true}}}, ""},
		{"agents defaults other key", map[string]any{"agents": map[string]any{"defaults": map[string]any{"model_name": "sim"}}}, ""},
		{"debug other key", map[string]any{"debug": map[string]any{"dev_mcp": map[string]any{"enabled": true}}}, ""},
		{"loopback upper url", map[string]any{"X": map[string]any{"base_url": "http://LOCALHOST:9/v1"}}, ""},
		{"url in list", map[string]any{"urls": []any{"https://example.com"}}, "urls.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := CheckConfigPatch(c.patch)
			if c.want == "" {
				if len(issues) != 0 {
					t.Fatalf("want accepted, got %v", issues)
				}
				return
			}
			if len(issues) != 1 || strings.Join(issues[0].Path, ".") != c.want {
				t.Fatalf("issues = %v, want one at %s", issues, c.want)
			}
		})
	}
}

func TestCheckWorkspacePath(t *testing.T) {
	for _, ok := range []string{"HEARTBEAT.md", "memory/MEMORY.md", "a/b/c.txt"} {
		if err := CheckWorkspacePath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/..", "a\\b", ".", "./", "C:/x", "a\x00b"} {
		if err := CheckWorkspacePath(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
