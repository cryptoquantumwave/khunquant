package sim

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

const devWords = "check my device, /dev/i2c, and ask the developer"

// TestCanonicalizeMasksBuildIdentity: the system prompt prints the build (config.FormatVersion)
// in its header, which `make build` stamps with the version and commit. Two builds must share
// a key, and only that printed header is masked: a plain build's version is "dev", which must
// not be taken out of other text. (The real prompts are checked in pkg/agent against these
// same formats: TestBuildIdentityHeadersAreMasked.)
func TestCanonicalizeMasksBuildIdentity(t *testing.T) {
	oldV, oldC := config.Version, config.GitCommit
	t.Cleanup(func() { config.Version, config.GitCommit = oldV, oldC })

	request := func() []protocoltypes.Message {
		return []protocoltypes.Message{
			{Role: "system", Content: "# khunquant 🦞 (" + config.FormatVersion() + ")\n\nYou are khunquant.\n\n# khunquant (" + config.FormatVersion() + ")\nproactive"},
			{Role: "user", Content: devWords},
		}
	}
	key := func() string { return Key(Canonicalize("m", request(), nil, nil)) }

	config.Version, config.GitCommit = "dev", ""
	plain := key()
	config.Version, config.GitCommit = "v0.9.1-12-gabc1234-dirty", "abc1234"
	stamped := key()
	config.Version, config.GitCommit = "v1.0.0", "fffffff"
	other := key()
	if plain != stamped || stamped != other {
		t.Errorf("builds with different stamps must share a key: %s %s %s", plain, stamped, other)
	}

	for _, v := range []string{"dev", "v0.9.1"} {
		config.Version, config.GitCommit = v, ""
		got := Canonicalize("m", request(), nil, nil)
		if sys := got.Messages[0].Content; !strings.Contains(sys, "# khunquant 🦞 (<VERSION>)") || !strings.Contains(sys, "# khunquant (<VERSION>)") {
			t.Errorf("version %q: headers not masked: %q", v, sys)
		}
		if got.Messages[1].Content != devWords {
			t.Errorf("version %q: other text was touched: %q", v, got.Messages[1].Content)
		}
	}

	// Different content still differs.
	if keyOf(sysUser("# khunquant ("+config.FormatVersion()+") one", "hi")) == keyOf(sysUser("# khunquant ("+config.FormatVersion()+") two", "hi")) {
		t.Error("masking the version must not hide other content")
	}
}
