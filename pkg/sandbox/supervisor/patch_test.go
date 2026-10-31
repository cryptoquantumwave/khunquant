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
