package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
)

// The intents digest keeps what the owner wants the assistant to keep doing in
// front of the model on every turn, so that it never depends on the conversation
// surviving compaction. It shows the titles of ACTIVE goals, standing orders and
// watches, and only the NUMBER of suggested intents: the text of a suggestion was
// written by a model or by somebody who is not the owner, and none of it reaches a
// prompt through here.
//
// It exists only while proactive.intents is effectively on (instance.go attaches
// it then), so with the default config no prompt contains it, not even a header.

const (
	// intentDigestCap is the most bytes the section may take in a full (chat) turn.
	// It is measured in bytes: a character cap would let Thai text take three
	// times as much.
	intentDigestCap = 1200
	// intentDigestCompactCap is the cap in a light or proactive turn. Those turns
	// have a first-call budget (proactive_first_call_test.go) that the section is
	// carried twice in, so it is smaller.
	intentDigestCompactCap = 500
)

// intentsContributor renders the digest. It implements liveContributor: it is
// rebuilt on every prompt, so a new intent shows in the very next one, a lapsed
// intent is gone from it, and the cap follows the prompt mode. The store belongs
// to the shared database handle, so Close does not close it.
type intentsContributor struct {
	store *intent.Store
}

func (c *intentsContributor) Name() string { return "intents" }

// Section renders the full-prompt form.
func (c *intentsContributor) Section(ctx context.Context) (string, error) {
	return c.LiveSection(ctx, false)
}

func (c *intentsContributor) LiveSection(ctx context.Context, compact bool) (string, error) {
	active, err := c.store.ListIntents(ctx, intent.IntentFilter{
		Statuses: []intent.Status{intent.StatusActive},
		Types:    []intent.IntentType{intent.TypeGoal, intent.TypeStandingOrder, intent.TypeWatch},
		Limit:    intent.MaxOpenIntents,
	})
	if err != nil {
		return "", fmt.Errorf("list intents: %w", err)
	}
	proposed, err := c.store.CountIntents(ctx, intent.IntentFilter{Statuses: []intent.Status{intent.StatusProposed}})
	if err != nil {
		return "", fmt.Errorf("count suggested intents: %w", err)
	}
	limit := intentDigestCap
	if compact {
		limit = intentDigestCompactCap
	}
	return renderIntentDigest(active, proposed, limit), nil
}

func (c *intentsContributor) Close() error { return nil }

// digestRank orders the section: goals, then standing orders, then watches.
func digestRank(t intent.IntentType) int {
	switch t {
	case intent.TypeGoal:
		return 0
	case intent.TypeStandingOrder:
		return 1
	}
	return 2
}

func digestLabel(t intent.IntentType) string {
	return strings.ReplaceAll(string(t), "_", " ")
}

// renderIntentDigest renders the section in at most limit bytes: a header, one
// line per active intent (type and title, goals first), a line saying how many
// more there are when they do not all fit, and the number of suggestions. It
// returns "" when there is nothing to show. Titles are re-sanitised here
// whatever the store holds.
func renderIntentDigest(active []intent.Intent, proposed, limit int) string {
	if len(active) == 0 && proposed == 0 {
		return ""
	}
	// Stable by rank, then by the store's order (oldest first).
	ordered := make([]intent.Intent, 0, len(active))
	for rank := 0; rank <= 2; rank++ {
		for _, it := range active {
			if digestRank(it.Type) == rank {
				ordered = append(ordered, it)
			}
		}
	}

	const header = "Intents (active, set by the owner):\n"
	tail := ""
	if proposed > 0 {
		tail = fmt.Sprintf("Suggested intents, not active: %d (list_intents shows them)", proposed)
	}
	lines := make([]string, len(ordered))
	for i, it := range ordered {
		lines[i] = "- " + digestLabel(it.Type) + ": " + sanitizeInline(it.Title, intent.MaxTitleRunes)
	}

	// Everything fits?
	var b strings.Builder
	b.WriteString(header)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString(tail)
	if full := strings.TrimRight(b.String(), "\n"); len(full) <= limit {
		if len(ordered) == 0 {
			// Only suggestions: no header about active intents.
			return tail
		}
		return full
	}

	// Otherwise keep as many lines as fit, leaving room for the "more" line and
	// the tail.
	moreLine := func(n int) string { return fmt.Sprintf("- ... and %d more (list_intents shows them)", n) }
	reserve := len(moreLine(len(lines))) + 1 + len(tail) + 1
	used := len(header)
	kept := 0
	for _, l := range lines {
		if used+len(l)+1+reserve > limit {
			break
		}
		used += len(l) + 1
		kept++
	}
	var out strings.Builder
	if len(ordered) > 0 {
		out.WriteString(header)
		for _, l := range lines[:kept] {
			out.WriteString(l)
			out.WriteByte('\n')
		}
		if kept < len(lines) {
			out.WriteString(moreLine(len(lines) - kept))
			out.WriteByte('\n')
		}
	}
	out.WriteString(tail)
	return strings.TrimRight(out.String(), "\n")
}
