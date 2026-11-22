package control_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
)

// /stop reports which proposals it cancelled, by kind, and which exits it kept, by
// code; a kept exit does not count as cancelled.
func TestStop_ReportsCancelledByKindAndKeptExits(t *testing.T) {
	r := newKillRig(t)
	r.proposals.pending = []approval.Cancelled{
		{Code: "AAAA", Kind: approval.KindOrder}, {Code: "BBBB", Kind: approval.KindMemory}, {Code: "CCCC", Kind: approval.KindMandate},
	}
	r.proposals.kept = []approval.Cancelled{{Code: "EXT1", Kind: approval.KindOrder}, {Code: "PRO2", Kind: approval.KindOrder}}

	sum := r.stop()
	if sum.ProposalsCancelled != 3 || len(sum.ProposalsKept) != 2 {
		t.Fatalf("cancelled %d, kept %v", sum.ProposalsCancelled, sum.ProposalsKept)
	}
	text := sum.Text()
	for _, want := range []string{
		"3 proposals (mandate, memory, order)",
		"Kept 2 exit proposals (EXT1, PRO2) so you can still close positions",
		"/resume allows new trades and automatic actions again",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "allow approvals again") {
		t.Errorf("the misleading sentence is back:\n%s", text)
	}
}

// The Thai reply names the same cancelled kinds, the kept codes and what /resume does.
func TestStop_ReplyInThai(t *testing.T) {
	r := newKillRig(t)
	r.proposals.pending = []approval.Cancelled{{Code: "AAAA", Kind: approval.KindOrder}}
	r.proposals.kept = []approval.Cancelled{{Code: "EXT1", Kind: approval.KindOrder}}
	r.k.Lang = func(context.Context) string { return "th" }

	sum := r.stop()
	text := sum.Text()
	for _, want := range []string{"หยุดแล้ว", "1 ข้อเสนอ (order)", "เก็บข้อเสนอปิดสถานะไว้ 1 รายการ (EXT1)", "/resume จะอนุญาตให้เทรดใหม่"} {
		if !strings.Contains(text, want) {
			t.Errorf("Thai text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Kept ") || strings.Contains(text, "Stopped.") {
		t.Errorf("English leaked into the Thai reply:\n%s", text)
	}
}

// A second /stop with nothing new to cancel still names the exits that are kept, and
// cancels nothing more.
func TestStop_SecondStopChangesNothing(t *testing.T) {
	r := newKillRig(t)
	r.proposals.pending = []approval.Cancelled{{Code: "AAAA", Kind: approval.KindOrder}}
	r.proposals.kept = []approval.Cancelled{{Code: "EXT1", Kind: approval.KindOrder}}
	first := r.stop()
	second := r.stop()
	if first.ProposalsCancelled != 1 || second.ProposalsCancelled != 0 || len(second.ProposalsKept) != 1 || !second.AlreadyEngaged {
		t.Errorf("first %+v, second %+v", first, second)
	}
}

// The language lookup may read the database, so it runs after the kill flag is set.
func TestStop_LanguageLookupComesAfterTheFlag(t *testing.T) {
	r := newKillRig(t)
	r.k.Lang = func(context.Context) string { r.log.add("lang"); return "en" }
	r.stop()
	f, l := r.log.index("flag.engage"), r.log.index("lang")
	if f < 0 || l < 0 || f > l {
		t.Errorf("flag.engage at %d, lang at %d; calls = %v", f, l, r.log.all())
	}
}
