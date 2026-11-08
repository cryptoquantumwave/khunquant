package agent

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
)

// TestBuildIdentityHeadersAreMasked pins the canonical model request to the real prompts:
// both identity headers the agent prints (getIdentity and proactiveIdentity) carry
// config.FormatVersion(), and sim's canonicaliser must mask exactly that, so a cassette
// recorded by a plain `go build` replays under a `make build` binary. Other text with the
// plain build's "dev" in it is left alone.
func TestBuildIdentityHeadersAreMasked(t *testing.T) {
	oldV, oldC := config.Version, config.GitCommit
	t.Cleanup(func() { config.Version, config.GitCommit = oldV, oldC })

	cb := NewContextBuilder(t.TempDir())
	keyOf := func(version, commit string) string {
		config.Version, config.GitCommit = version, commit
		msgs := []protocoltypes.Message{
			{Role: "system", Content: cb.getIdentity()},
			{Role: "system", Content: proactiveIdentity()},
			{Role: "user", Content: "check my device, /dev/i2c, and ask the developer"},
		}
		got := sim.Canonicalize("m", msgs, nil, nil)
		for i := range 2 {
			if !strings.Contains(got.Messages[i].Content, "(<VERSION>)") {
				t.Errorf("version %q: prompt %d not masked: %.80q", version, i, got.Messages[i].Content)
			}
			if v := config.FormatVersion(); v != "" && strings.Contains(got.Messages[i].Content, "("+v+")") {
				t.Errorf("version %q: prompt %d still prints its version", version, i)
			}
		}
		if !strings.Contains(got.Messages[2].Content, "device, /dev/i2c, and ask the developer") {
			t.Errorf("version %q: user text was touched: %q", version, got.Messages[2].Content)
		}
		return sim.Key(got)
	}
	if a, b := keyOf("dev", ""), keyOf("v0.7.1-beta.2-273-g2d0868b5-dirty", "2d0868b5"); a != b {
		t.Errorf("a plain build and a make build must share a key: %s %s", a, b)
	}
}
