package attention

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func detailItem(title, detail, prose string) Item {
	return Item{
		Candidate: proactive.Candidate{DedupKey: "k:" + title, Title: title, Detail: detail, Severity: proactive.SeverityNotice},
		Prose:     prose,
	}
}

func TestRenderGroup_SingleDetailBetweenTitleAndProse(t *testing.T) {
	m := renderGroup([]Item{detailItem("BTC/USDT fell below 60000", "Price 59900\nThreshold 60000", "  model words  ")})
	if m.Title != "BTC/USDT fell below 60000" {
		t.Fatalf("title = %q", m.Title)
	}
	want := "Price 59900\nThreshold 60000\n\nmodel words"
	if m.Body != want {
		t.Fatalf("body = %q, want %q", m.Body, want)
	}
}

func TestRenderGroup_SingleWithoutDetailIsUnchanged(t *testing.T) {
	if m := renderGroup([]Item{detailItem("t", "", "  prose ")}); m.Body != "prose" {
		t.Fatalf("body = %q, want the trimmed prose and nothing else", m.Body)
	}
	if m := renderGroup([]Item{detailItem("t", "", "")}); m.Body != "" {
		t.Fatalf("body = %q, want empty", m.Body)
	}
}

func TestRenderGroup_SingleDetailOnly(t *testing.T) {
	if m := renderGroup([]Item{detailItem("t", "line one", "")}); m.Body != "line one" {
		t.Fatalf("body = %q", m.Body)
	}
}

func TestRenderGroup_GroupedKeepsTitleAndFirstDetailLineOnly(t *testing.T) {
	m := renderGroup([]Item{
		detailItem("first", "first detail\nsecond detail line", "prose a"),
		detailItem("second", "", ""),
		detailItem("third", "third detail", ""),
	})
	if m.Title != "3 updates" {
		t.Fatalf("title = %q", m.Title)
	}
	want := "• first\n  first detail\n  prose a\n• second\n• third\n  third detail"
	if m.Body != want {
		t.Fatalf("body = %q, want %q", m.Body, want)
	}
	if strings.Contains(m.Body, "second detail line") {
		t.Fatal("a grouped message must carry only the first line of each detail")
	}
}

func TestRenderGroup_DetailIsCleanedAndCapped(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line")
	}
	detail := strings.Join(lines, "\n") + "\n\x00bad\u202econtrol\ttab"
	m := renderGroup([]Item{detailItem("t", detail, "")})
	if got := strings.Count(m.Body, "\n") + 1; got != proactive.MaxDetailLines {
		t.Fatalf("body has %d lines, want %d: %q", got, proactive.MaxDetailLines, m.Body)
	}

	long := strings.Repeat("x", 5000)
	m = renderGroup([]Item{detailItem("t", "a\r\n\n   \n"+long, "")})
	parts := strings.Split(m.Body, "\n")
	if len(parts) != 2 || parts[0] != "a" {
		t.Fatalf("blank lines must be dropped: %q", parts)
	}
	if n := len([]rune(parts[1])); n > maxDetailLineRunes {
		t.Fatalf("a detail line has %d runes, the cap is %d", n, maxDetailLineRunes)
	}
	// A 300-character quoted message plus its label fits in one line.
	m = renderGroup([]Item{detailItem("t", "Note: \""+strings.Repeat("y", 300)+"\"", "")})
	if strings.Contains(m.Body, "…") {
		t.Fatalf("a 300-character message must not be cut: %q", m.Body)
	}

	m = renderGroup([]Item{detailItem("t", "x\u202ey\x07z", "")})
	if strings.ContainsAny(m.Body, "\u202e\x07") {
		t.Fatalf("control and format characters must not survive: %q", m.Body)
	}
}
