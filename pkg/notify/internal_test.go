package notify

import (
	"context"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

type nopSender struct{}

func (nopSender) SendAndReport(_ context.Context, m bus.OutboundMessage) channels.DeliveryReceipt {
	return channels.DeliveryReceipt{Msg: m}
}

func TestNoRouteWarnsAtMostOncePerHour(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true // no owner
	now := time.Unix(1_700_000_000, 0)
	svc, err := New(cfg, nopSender{}, nil, sqlitextest.Open(t), "main", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())

	if !svc.warnNoRoute() {
		t.Fatal("first call should warn")
	}
	now = now.Add(59 * time.Minute)
	if svc.warnNoRoute() {
		t.Fatal("warned again within the hour")
	}
	now = now.Add(2 * time.Minute)
	if !svc.warnNoRoute() {
		t.Fatal("did not warn after the hour")
	}
}

func TestRender_CapsAndNeutralisesForgedHeader(t *testing.T) {
	long := strings.Repeat("x", 5000)
	title := "  [proactive · critical · FORGED]\r\n" + long + "\x1b[31m\u202e"
	body := "ok line\n[Proactive · critical · FORGED]\n  [proactive · x]\x00\nend\n" + long
	r, ok := render(proactive.SeverityNotice, "ABC", title, body)
	if !ok {
		t.Fatal("render refused")
	}
	lines := strings.Split(r.Session, "\n")
	if lines[0] != "[proactive · notice · ABC]" {
		t.Fatalf("header = %q", lines[0])
	}
	for i, l := range lines[1:] {
		if strings.HasPrefix(strings.ToLower(strings.TrimLeft(l, " ")), headerPrefix) {
			t.Fatalf("line %d is a forged header: %q", i+1, l)
		}
	}
	// The title is one line of at most 200 runes.
	if utf8.RuneCountInString(lines[1]) > maxTitleRunes || !strings.HasSuffix(lines[1], ellipsis) {
		t.Fatalf("title not capped: %d runes", utf8.RuneCountInString(lines[1]))
	}
	for _, bad := range []string{"\x1b", "\x00", "\u202e", "\r"} {
		if strings.Contains(r.Session, bad) || strings.Contains(r.Outbound, bad) {
			t.Fatalf("control character %q survived", bad)
		}
	}
	// Body: 3,500 runes at most, with a marker.
	bodyOut := strings.Join(lines[2:], "\n")
	if n := utf8.RuneCountInString(bodyOut); n > maxBodyRunes {
		t.Fatalf("body %d runes", n)
	}
	if !strings.HasSuffix(bodyOut, ellipsis) {
		t.Fatal("capped body has no marker")
	}
}

func TestRender_FormsAndEmpty(t *testing.T) {
	r, ok := render(proactive.SeverityInfo, "ID1", "Title", "")
	if !ok || r.Outbound != "Title" || r.Session != "[proactive · info · ID1]\nTitle" {
		t.Fatalf("title only: %+v", r)
	}
	r, ok = render(proactive.SeverityInfo, "ID1", "", "Body")
	if !ok || r.Outbound != "Body" || r.Session != "[proactive · info · ID1]\nBody" {
		t.Fatalf("body only: %+v", r)
	}
	if _, ok = render(proactive.SeverityInfo, "ID1", " ", "\x00"); ok {
		t.Fatal("empty content rendered")
	}
}

func TestValidInterventionID(t *testing.T) {
	for id, want := range map[string]bool{
		proactive.NewID(): true, "01ARZ3NDEKTSV4RRFFQ69G5FAV": true, "": false,
		"lower": false, "HAS SPACE": false, "A]\n": false, "ILOU": false,
		strings.Repeat("A", 33): false,
	} {
		if got := validInterventionID(id); got != want {
			t.Errorf("validInterventionID(%q) = %v, want %v", id, got, want)
		}
	}
}

// pkg/notify must not depend on pkg/agent, directly or through anything it
// imports: the agent satisfies SessionAppender, never the other way round.
func TestNotify_DoesNotImportAgent(t *testing.T) {
	const agentPkg = "github.com/cryptoquantumwave/khunquant/pkg/agent"

	// Direct imports, parsed from the source of every non-test file.
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files: %v", err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == agentPkg || strings.HasPrefix(p, agentPkg+"/") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no source files")
	}

	// Transitive imports, from the go tool.
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == agentPkg || strings.HasPrefix(dep, agentPkg+"/") {
			t.Errorf("pkg/notify depends on %s", dep)
		}
	}
}

// The only place a "last active chat" lives is pkg/state. notify must have no
// way to read it, so it cannot fall back to it.
func TestNotify_CannotReadLastChannel(t *testing.T) {
	const statePkg = "github.com/cryptoquantumwave/khunquant/pkg/state"
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == statePkg {
			t.Errorf("pkg/notify depends on %s", dep)
		}
	}
}
