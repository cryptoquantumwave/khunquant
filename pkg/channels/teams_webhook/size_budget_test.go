package teamswebhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// buildLargeMarkdownTable returns a well-formed markdown table of at least
// minBytes, used to reproduce T4-3: a large table inflates roughly 10x when
// rendered as native Adaptive Card Table elements, which can push a message
// past Teams' payload limit.
func buildLargeMarkdownTable(minBytes int) string {
	var b strings.Builder
	b.WriteString("| Column A | Column B | Column C | Column D |\n")
	b.WriteString("|----------|----------|----------|----------|\n")
	row := "| aaaaaaaaaa | bbbbbbbbbb | cccccccccc | dddddddddd |\n"
	for b.Len() < minBytes {
		b.WriteString(row)
	}
	return b.String()
}

func testChannel(t *testing.T) *TeamsWebhookChannel {
	t.Helper()
	msgBus := bus.NewMessageBus()
	ch, err := NewTeamsWebhookChannel(config.TeamsWebhookConfig{
		Enabled: true,
		Webhooks: map[string]config.TeamsWebhookTarget{
			"default": {WebhookURL: *config.NewSecureString("https://example.invalid/hook")},
		},
	}, msgBus)
	if err != nil {
		t.Fatalf("NewTeamsWebhookChannel() error = %v", err)
	}
	return ch
}

// TestBuildAdaptiveCard_NativeTableRenderingInflatesPastBudget documents the
// defect: a ~19 KB markdown table, rendered as native Table elements, blows
// past the 25 KB budget on its own.
func TestBuildAdaptiveCard_NativeTableRenderingInflatesPastBudget(t *testing.T) {
	ch := testChannel(t)
	table := buildLargeMarkdownTable(19 * 1024)
	if len(table) < 19*1024 {
		t.Fatalf("test fixture too small: %d bytes", len(table))
	}
	target := ch.config.Webhooks["default"]

	card, err := ch.buildAdaptiveCard(bus.OutboundMessage{Content: table}, target, false)
	if err != nil {
		t.Fatalf("buildAdaptiveCard() error = %v", err)
	}
	size, err := cardMessageSize(card)
	if err != nil {
		t.Fatalf("cardMessageSize() error = %v", err)
	}
	if size <= teamsCardSizeBudget {
		t.Fatalf(
			"native table rendering of a %d-byte table produced only %d bytes; "+
				"this test no longer reproduces the inflation the tablesAsCode fallback exists for",
			len(table), size,
		)
	}
}

// TestBuildTeamsMessages_OversizedTableStaysUnderBudget is the acceptance
// test for T4-3: a 19 KB table must produce payload(s) each <= 25 KB.
func TestBuildTeamsMessages_OversizedTableStaysUnderBudget(t *testing.T) {
	ch := testChannel(t)
	table := buildLargeMarkdownTable(19 * 1024)
	content := "Report for today:\n\n" + table + "\nEnd of report."
	target := ch.config.Webhooks["default"]

	messages, err := ch.buildTeamsMessages(bus.OutboundMessage{Content: content}, target)
	if err != nil {
		t.Fatalf("buildTeamsMessages() error = %v", err)
	}
	if len(messages) == 0 {
		t.Fatal("buildTeamsMessages() returned no messages")
	}
	for i, m := range messages {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		if len(b) > teamsCardSizeBudget {
			t.Errorf("message %d/%d size = %d bytes, want <= %d", i+1, len(messages), len(b), teamsCardSizeBudget)
		}
	}
}

// TestBuildTeamsMessages_EvenLargerContentSplitsIntoMultipleCards forces the
// third degrade step (paragraph-boundary splitting): content large enough
// that even tables-as-code doesn't fit in one card.
func TestBuildTeamsMessages_EvenLargerContentSplitsIntoMultipleCards(t *testing.T) {
	ch := testChannel(t)
	target := ch.config.Webhooks["default"]

	var content strings.Builder
	for i := 0; i < 6; i++ {
		content.WriteString("Section heading text describing part ")
		content.WriteString(strings.Repeat("x", 40))
		content.WriteString("\n\n")
		content.WriteString(buildLargeMarkdownTable(15 * 1024))
		content.WriteString("\n\n")
	}

	messages, err := ch.buildTeamsMessages(bus.OutboundMessage{Content: content.String()}, target)
	if err != nil {
		t.Fatalf("buildTeamsMessages() error = %v", err)
	}
	if len(messages) < 2 {
		t.Fatalf("expected content this large to split into multiple cards, got %d", len(messages))
	}
	for i, m := range messages {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		if len(b) > teamsCardSizeBudget {
			t.Errorf("message %d/%d size = %d bytes, want <= %d", i+1, len(messages), len(b), teamsCardSizeBudget)
		}
	}
}

// TestSplitTextToFit_TerminatesOnSingleLine guards the recursion base case:
// a single line that still doesn't fit is returned as-is rather than looping
// forever trying to split it further.
func TestSplitTextToFit_TerminatesOnSingleLine(t *testing.T) {
	neverFits := func(string) (bool, error) { return false, nil }
	got, err := splitTextToFit("one line with no newline", neverFits)
	if err != nil {
		t.Fatalf("splitTextToFit() error = %v", err)
	}
	if len(got) != 1 || got[0] != "one line with no newline" {
		t.Errorf("splitTextToFit() = %v, want the single line unchanged", got)
	}
}

// TestSplitTextToFit_SplitsUntilFits asserts pieces get smaller until the
// fits predicate is satisfied.
func TestSplitTextToFit_SplitsUntilFits(t *testing.T) {
	text := "line1\nline2\nline3\nline4"
	fitsUnderTwoLines := func(s string) (bool, error) {
		return strings.Count(s, "\n") < 1, nil
	}
	got, err := splitTextToFit(text, fitsUnderTwoLines)
	if err != nil {
		t.Fatalf("splitTextToFit() error = %v", err)
	}
	for _, piece := range got {
		if strings.Count(piece, "\n") >= 1 {
			t.Errorf("piece %q still has 2+ lines", piece)
		}
	}
	// Re-joining preserves every original line.
	joined := strings.Join(got, "\n")
	if joined != text {
		t.Errorf("splitTextToFit() lost content: got %q, want %q", joined, text)
	}
}
