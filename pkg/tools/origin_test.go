package tools

import (
	"context"
	"testing"
)

func TestToolOrigin_UntaggedIsEmpty(t *testing.T) {
	// An untagged context must report "", not a default that a policy could
	// mistake for permission. Callers decide what to do with "unknown"; this
	// helper never invents an answer.
	if got := ToolOrigin(context.Background()); got != "" {
		t.Fatalf("ToolOrigin(background) = %q, want \"\"", got)
	}
}

func TestWithToolOrigin_TagsAndReadsBack(t *testing.T) {
	for _, origin := range []string{OriginInternal, OriginRemote, OriginCron} {
		ctx := WithToolOrigin(context.Background(), origin)
		if got := ToolOrigin(ctx); got != origin {
			t.Errorf("ToolOrigin() = %q, want %q", got, origin)
		}
	}
}

// TestWithToolOrigin_SetOnceKeepsCron is the assertion the whole cron gate rests
// on. Cron tags at its call site, outside the agent loop; the loop then tags
// every root turn by channel. If the second tag won, a scheduled job would be
// relabelled "internal" and the cron gate would never fire.
func TestWithToolOrigin_SetOnceKeepsCron(t *testing.T) {
	ctx := WithToolOrigin(context.Background(), OriginCron)
	ctx = WithToolOrigin(ctx, OriginInternal)
	ctx = WithToolOrigin(ctx, OriginRemote)

	if got := ToolOrigin(ctx); got != OriginCron {
		t.Fatalf("ToolOrigin() = %q, want %q (the first tag must win)", got, OriginCron)
	}
}

// An empty origin must not occupy the slot, or a caller passing "" would lock
// out the real tag that follows.
func TestWithToolOrigin_EmptyDoesNotClaimTheSlot(t *testing.T) {
	ctx := WithToolOrigin(context.Background(), "")
	if got := ToolOrigin(ctx); got != "" {
		t.Fatalf("ToolOrigin() = %q, want \"\" after tagging with an empty origin", got)
	}

	ctx = WithToolOrigin(ctx, OriginRemote)
	if got := ToolOrigin(ctx); got != OriginRemote {
		t.Fatalf("ToolOrigin() = %q, want %q", got, OriginRemote)
	}
}

// TestCLITurnIsNotTaggedCron pins the trap the original plan would have walked
// into. ProcessDirectWithChannel hardcodes SenderID "cron" for every caller, and
// the CLI path routes through that same function - so tagging keyed off the
// sender would gate an interactive CLI turn as an unattended scheduled job.
// Origin is derived from the channel instead. If someone later "simplifies"
// tagging back to the sender field, this is the test that fails.
func TestCLITurnIsNotTaggedCron(t *testing.T) {
	ctx := WithToolOrigin(context.Background(), OriginForChannel("cli"))

	got := ToolOrigin(ctx)
	if got == OriginCron {
		t.Fatalf("a CLI turn was tagged %q; it must not be gated as a scheduled job", got)
	}
	if got != OriginInternal {
		t.Fatalf("ToolOrigin() = %q, want %q", got, OriginInternal)
	}
}

func TestOriginForChannelMapping(t *testing.T) {
	for _, tc := range []struct {
		channel string
		want    string
	}{
		// constants.IsInternalChannel covers exactly these three.
		{"cli", OriginInternal},
		{"system", OriginInternal},
		{"subagent", OriginInternal},
		{"telegram", OriginRemote},
		{"discord", OriginRemote},
		{"", OriginInternal},
	} {
		if got := OriginForChannel(tc.channel); got != tc.want {
			t.Errorf("OriginForChannel(%q) = %q, want %q", tc.channel, got, tc.want)
		}
	}
}
