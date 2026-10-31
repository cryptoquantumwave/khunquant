package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

// repoRootDir is the repository, found by walking up to go.mod.
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// journalKindLiterals scans the non-test sources that write to the lab journal for the
// kinds they name: a producer written through a helper (simchat's w.log, the sim
// engine's Journal.Record) is found as well as a direct Entry{Kind: ...}.
func journalKindLiterals(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRootDir(t)
	res := []*regexp.Regexp{
		regexp.MustCompile(`\bKind:\s*"([a-z][a-z_.]*)"`),
		regexp.MustCompile(`\.log\("([a-z][a-z_.]*)"`),
		regexp.MustCompile(`\bKind\w*\s*=\s*"([a-z][a-z_.]*)"`),
		regexp.MustCompile(`\bkind(?:, subject)?\s*:?=\s*"([a-z][a-z_.]*)"`),
		regexp.MustCompile(`\.Kind, e\.Subject = "([a-z][a-z_.]*)"`),
	}
	found := map[string][]string{}
	scanned := 0
	for _, sub := range []string{"pkg", "cmd/khunquant"} {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(data)
			if !strings.Contains(src, "labjournal") && !strings.Contains(src, "Journal.Record(") && !strings.Contains(path, "channels/simchat") {
				return nil
			}
			scanned++
			for _, re := range res {
				for _, m := range re.FindAllStringSubmatch(src, -1) {
					rel, _ := filepath.Rel(root, path)
					found[m[1]] = append(found[m[1]], rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 5 || len(found) < 15 {
		t.Fatalf("the source scan is blind: %d files, %d kinds (a moved package or a changed pattern?)", scanned, len(found))
	}
	t.Logf("source scan: %d files, kinds %v", scanned, found)
	return found
}

// A locked gateway declares every kind the components of this binary write, so a
// scenario that reads one is not falsely told its PR is missing.
func TestDeclaredKinds_CoverMergedProducers(t *testing.T) {
	f := newLockedFixture(t)
	kinds := f.lab.Kinds()
	for _, want := range []string{
		"chat.in", "chat.out", "chat.edit", "chat.press", "chat.react", "chat.state", "exchange.req", "llm.call", "llm.complete",
		"event.*", "tool.call", "workflow.step", "proposal", "policy.decision", "manage.call", "clock.step", "settle", "config.reload",
	} {
		if !scenario.KindDeclared(kinds, want) {
			t.Errorf("kind %q is not declared; declared: %v", want, kinds)
		}
	}
	if !scenario.KindDeclared(kinds, "event.trigger.candidate") {
		t.Error("event.* must cover event.trigger.candidate")
	}
	// Whatever the source writes must be declared, whoever wrote it.
	for kind, files := range journalKindLiterals(t) {
		if !scenario.KindDeclared(kinds, kind) {
			t.Errorf("%s writes journal kind %q, which labJournalKinds does not declare", strings.Join(files, ", "), kind)
		}
	}
}

func reloadEntries(t *testing.T, f *lockedFixture) []struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
} {
	t.Helper()
	es, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{"config.reload"}})
	if err != nil {
		t.Fatal(err)
	}
	var out []struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	for _, e := range es {
		var p struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("config.reload payload %s: %v", e.Payload, err)
		}
		out = append(out, p)
	}
	return out
}

// A reload is journaled when it has finished, and a refused one says why: the runner
// waits for this entry after config.patch.
func TestConfigReload_JournalsOutcome(t *testing.T) {
	f := newLockedFixture(t)
	if got := reloadEntries(t, f); len(got) != 0 {
		t.Fatalf("a fresh instance has no config.reload entry, got %v", got)
	}
	if err := f.reload(func(c *config.Config) { c.Heartbeat.Interval = 45 }); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloadEntries(t, f); len(got) != 1 || !got[0].OK {
		t.Fatalf("after a good reload: %+v, want one ok entry", got)
	}

	// A model the lock does not allow is refused before anything stops; the gateway
	// loop then exits with code 3 (TestLockedGateway_ReloadFailureExit3), and the entry says why.
	err := f.reload(func(c *config.Config) {
		c.ModelList = append(c.ModelList, config.ModelConfig{ModelName: "real", Model: "openai/gpt-5"})
	})
	if err == nil || !strings.Contains(err.Error(), "sandbox lock") {
		t.Fatalf("a real model must be refused under the lock, got %v", err)
	}
	got := reloadEntries(t, f)
	if len(got) != 2 || got[1].OK || !strings.Contains(got[1].Error, "openai") {
		t.Fatalf("after a refused reload: %+v, want a second entry with ok:false naming the protocol", got)
	}
}

// The same refusal in a real locked process: exit 3, and the journal left in its home
// says why, so the runner can report a refused config.patch by name.
func TestLockedGateway_RefusedReloadIsJournaled(t *testing.T) {
	p := startLockedProcess(t)
	p.waitReady()
	writeSandboxConfig(t, p.home, p.port, func(c *config.Config) {
		c.ModelList = []config.ModelConfig{{ModelName: "sandbox", Model: "claude-cli/x"}}
		c.Agents.Defaults.ModelName = "sandbox"
	})
	err := p.wait(45 * time.Second)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("exit = %v, want exit status 3\n%s", err, p.out.String())
	}
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(p.home, "lab", "journal.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	es, err := j.Query(context.Background(), labjournal.Query{Kinds: []string{"config.reload"}})
	if err != nil || len(es) != 1 || !strings.Contains(string(es[0].Payload), `"ok":false`) || !strings.Contains(string(es[0].Payload), "claude-cli") {
		t.Fatalf("config.reload entries = %v (%v), want one ok:false naming the protocol", es, err)
	}
}
