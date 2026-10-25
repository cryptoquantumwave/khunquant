package control

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
)

// The reply to Save and Discard names the suggestion by code and category, and says what
// happened to memory; it is words by code, in a conversation a model reads.
func TestDecidedText_Memory(t *testing.T) {
	p := approval.Proposal{Kind: approval.KindMemory, Code: "AB3K", Summary: "Memory suggestion AB3K (fact)", Status: approval.StatusExecuted}

	if got := decidedText(p, true); !strings.Contains(got, "Saved Memory suggestion AB3K (fact) to memory") {
		t.Errorf("saved: %q", got)
	}
	p.Status = approval.StatusExecuting
	if got := decidedText(p, true); !strings.Contains(got, "Saving it to memory now") {
		t.Errorf("saving: %q", got)
	}
	p.Status = approval.StatusRejected
	if got := decidedText(p, false); !strings.Contains(got, "Discarded") || !strings.Contains(got, "Nothing was saved to memory") ||
		strings.Contains(strings.ToLower(got), "order") {
		t.Errorf("discarded: %q", got)
	}
}
