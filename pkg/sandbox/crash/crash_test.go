package crash

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

const testToken = "t-0123456789abcdef0123456789abcdef"

func lockForTest(t *testing.T) {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: testToken}); err != nil {
		t.Fatal(err)
	}
}

// fakeJournal records what the arms write and read.
type fakeJournal struct {
	mu      sync.Mutex
	entries []labjournal.Entry
	flushes int
}

func (f *fakeJournal) Append(e labjournal.Entry) {
	f.mu.Lock()
	f.entries = append(f.entries, e)
	f.mu.Unlock()
}
func (f *fakeJournal) Flush(context.Context) error {
	f.mu.Lock()
	f.flushes++
	f.mu.Unlock()
	return nil
}
func (f *fakeJournal) Query(context.Context, labjournal.Query) ([]labjournal.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]labjournal.Entry(nil), f.entries...), nil
}

// stubKill replaces selfKill with a recorder for the test.
func stubKill(t *testing.T) *int {
	t.Helper()
	n := 0
	old := selfKill
	selfKill = func() { n++ }
	t.Cleanup(func() { selfKill = old })
	return &n
}

func TestArms_RefuseUnlocked_OneShot(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	a := New(Options{Home: t.TempDir()})
	if id, err := a.Arm(Arm{Workflow: "dn.open", Step: "place_second_leg", Point: journal.CrashAfterFn}); err == nil || id != "" {
		t.Fatalf("an unlocked process armed a crash: %q, %v", id, err)
	}
	lockForTest(t)
	kills := stubKill(t)
	for _, bad := range []Arm{
		{Step: "s", Point: journal.CrashAfterFn},
		{Workflow: "w", Point: journal.CrashAfterFn},
		{Workflow: "w", Step: "s", Point: "during"},
		{Workflow: "w", Step: "s", Point: journal.CrashAfterFn, Attempt: -1},
	} {
		if _, err := a.Arm(bad); err == nil {
			t.Errorf("Arm(%+v) was accepted", bad)
		}
	}
	id, err := a.Arm(Arm{Workflow: "dn.open", Step: "place_second_leg", Point: journal.CrashAfterFn})
	if err != nil {
		t.Fatal(err)
	}
	hook := a.Hook()
	for _, miss := range []journal.CrashPoint{
		{Point: journal.CrashAfterStarted, Kind: "dn.open", Step: "place_second_leg", Attempt: 1},
		{Point: journal.CrashAfterFn, Kind: "dn.open", Step: "place_first_leg", Attempt: 1},
		{Point: journal.CrashAfterFn, Kind: "dn.unwind", Step: "place_second_leg", Attempt: 1},
	} {
		hook(miss)
	}
	if *kills != 0 {
		t.Fatalf("a point that does not match killed the process %d times", *kills)
	}
	hook(journal.CrashPoint{Point: journal.CrashAfterFn, WorkflowID: "wf_1", Kind: "dn.open", Step: "place_second_leg", Attempt: 1})
	hook(journal.CrashPoint{Point: journal.CrashAfterFn, WorkflowID: "wf_1", Kind: "dn.open", Step: "place_second_leg", Attempt: 2})
	if *kills != 1 {
		t.Fatalf("an armed point killed %d times, want exactly once (one-shot)", *kills)
	}
	if armed, _, _ := a.List(context.Background()); len(armed) != 0 {
		t.Errorf("the arm %s is still armed after it fired: %+v", id, armed)
	}
	if a.Disarm(id) {
		t.Error("a fired arm could still be disarmed")
	}
	// An attempt-specific arm waits for its attempt, and a disarm removes it.
	id2, _ := a.Arm(Arm{Workflow: "w", Step: "s", Point: journal.CrashAfterOutput, Attempt: 2})
	hook(journal.CrashPoint{Point: journal.CrashAfterOutput, Kind: "w", Step: "s", Attempt: 1})
	if *kills != 1 {
		t.Fatal("attempt 1 matched an arm for attempt 2")
	}
	if !a.Disarm(id2) {
		t.Fatal("Disarm did not find the arm")
	}
	hook(journal.CrashPoint{Point: journal.CrashAfterOutput, Kind: "w", Step: "s", Attempt: 2})
	if *kills != 1 {
		t.Fatal("a disarmed arm fired")
	}
}

// The fire path flushes the journal, leaves the marker (atomically, so a file is never
// half written) and then kills; the next process reports the marker once as a crash entry.
func TestFire_MarkerThenKill_ReportedOnce(t *testing.T) {
	lockForTest(t)
	home := t.TempDir()
	j := &fakeJournal{}
	at := time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC)
	a := New(Options{Home: home, Journal: j, Now: func() time.Time { return at }})
	var markerAtKill []byte
	old := selfKill
	selfKill = func() { markerAtKill, _ = os.ReadFile(filepath.Join(home, "lab", MarkerName)) }
	t.Cleanup(func() { selfKill = old })

	if _, err := a.Arm(Arm{Workflow: "dn.open", Step: "place_second_leg", Point: journal.CrashAfterFn}); err != nil {
		t.Fatal(err)
	}
	a.Hook()(journal.CrashPoint{Point: journal.CrashAfterFn, WorkflowID: "wf_9", Kind: "dn.open", Step: "place_second_leg", Attempt: 1})
	if j.flushes != 1 {
		t.Errorf("the journal was flushed %d times before the kill, want 1", j.flushes)
	}
	var m Marker
	if err := json.Unmarshal(markerAtKill, &m); err != nil {
		t.Fatalf("the marker was not on disk when the process was killed: %q (%v)", markerAtKill, err)
	}
	if m.Kind != "dn.open" || m.Step != "place_second_leg" || m.Point != journal.CrashAfterFn || m.WorkflowID != "wf_9" ||
		m.Attempt != 1 || m.ArmID != "c1" || !m.SimTime.Equal(at) || m.WallTime.IsZero() {
		t.Errorf("marker = %+v", m)
	}

	next := New(Options{Home: home, Journal: j})
	if err := next.ReportPrevious(); err != nil {
		t.Fatal(err)
	}
	if len(j.entries) != 1 || j.entries[0].Kind != KindCrash || j.entries[0].Subject != "dn.open" {
		t.Fatalf("entries = %+v", j.entries)
	}
	var p Marker
	if err := json.Unmarshal(j.entries[0].Payload, &p); err != nil || p != m {
		t.Errorf("crash entry payload %s, want %+v", j.entries[0].Payload, m)
	}
	if _, err := os.Stat(filepath.Join(home, "lab", MarkerName)); !os.IsNotExist(err) {
		t.Errorf("the marker was not moved away: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "lab", "crash-1.json")); err != nil {
		t.Errorf("crash-1.json: %v", err)
	}
	if err := next.ReportPrevious(); err != nil || len(j.entries) != 1 {
		t.Errorf("a second report found something: %v, %d entries", err, len(j.entries))
	}
	_, fired, err := next.List(context.Background())
	if err != nil || len(fired) != 1 || fired[0].Kind != "dn.open" {
		t.Errorf("List fired = %+v, %v", fired, err)
	}

	// A marker that is not valid stops the lab instead of being skipped.
	if err := os.WriteFile(filepath.Join(home, "lab", MarkerName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := next.ReportPrevious(); err == nil {
		t.Error("an invalid marker was accepted")
	}
}

// selfKill is reachable only from fire: nothing else in the package calls it, so the only
// way to the kill is a match on an armed point in a locked process.
func TestSelfKillOnlyFromFire(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources: %v", err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "selfKill" && fn.Name.Name != "fire" {
						t.Errorf("%s: %s calls selfKill; only fire may", name, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
}

// Nothing in production installs the crash hook except the lab wiring: SetCrashHook is
// called from no file outside the package that defines it and the gateway's lab wiring.
func TestSetCrashHookOnlyInLabWiring(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	allowed := map[string]bool{
		filepath.Join(root, "pkg", "journal", "crashhook.go"):                                   true,
		filepath.Join(root, "cmd", "khunquant", "internal", "gateway", "sandbox_lab_wiring.go"): true,
	}
	scanned := 0
	for _, sub := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if strings.Contains(string(b), ".SetCrashHook(") && !allowed[path] {
				t.Errorf("%s installs a crash hook outside the lab wiring", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files: the walk is not looking at the repository", scanned)
	}
}
