package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runner copies a repository recording into the instance home and points the op at
// the copy, as it does a cassette: the op reads only inside the home.
func TestRunner_CopiesARecordingIntoTheHome(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("testdata/replay/x", 0o755); err != nil {
		t.Fatal(err)
	}
	const data = `{"clock":"2026-03-10T05:00:00Z","event":{"kind":"perception.market.tick","payload":{"provider":"okx","symbol":"BTC/USDT","last":100}}}` + "\n"
	if err := os.WriteFile("testdata/replay/x/events.jsonl", []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := newFakeSup(t)
	rep := runOne(t, sup, scenarioOf(t, `
market: {BTC/USDT: {venue: okx, model: recording, path: testdata/replay/x/events.jsonl, align: start}}
timeline:
  - market.recording: {symbol: ETH/USDT, path: testdata/replay/x/events.jsonl}
`))
	if !rep.Runs[0].Pass {
		t.Fatalf("result = %+v", rep.Runs[0])
	}
	home := sup.order[0].home
	got, err := os.ReadFile(filepath.Join(home, "lab", "recordings", "rec1.jsonl"))
	if err != nil || string(got) != data {
		t.Fatalf("the recording was not copied into the home: %q, %v", got, err)
	}
	calls := journalPayloads(t, sup.order[0], "fake.market.*")
	if len(calls) != 2 {
		t.Fatalf("market calls = %v", calls)
	}
	for _, c := range calls {
		if !strings.Contains(c, `"path":"lab/recordings/rec1.jsonl"`) {
			t.Errorf("call %s must carry the home-relative copy (one copy per run)", c)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "lab", "recordings", "rec2.jsonl")); err == nil {
		t.Error("the same file was copied twice")
	}
}

func TestRunner_RefusesARecordingOutsideTheWorkingDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "e.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	for _, path := range []string{outside, "../e.jsonl", "missing.jsonl"} {
		sup := newFakeSup(t)
		res := runOne(t, sup, scenarioOf(t, "market: {BTC/USDT: {model: recording, path: \""+path+"\"}}\n")).Runs[0]
		if res.Pass || !strings.Contains(res.Failure, "recording") {
			t.Errorf("%s: %+v", path, res)
		}
		if len(journalPayloads(t, sup.order[0], "fake.market.*")) != 0 {
			t.Errorf("%s: the op was called", path)
		}
	}
}

// A file that is not a valid recording is refused before anything is copied: a repository
// file such as a git-ignored .env must never land in the instance home.
func TestRunner_RefusesANonRecordingBeforeCopying(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("API_KEY=not-a-real-secret\nOTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, step := range map[string]string{
		"market block": "market: {BTC/USDT: {venue: okx, model: recording, path: .env}}\n",
		"step":         "timeline:\n  - market.recording: {symbol: BTC/USDT, path: .env}\n",
	} {
		sup := newFakeSup(t)
		res := runOne(t, sup, scenarioOf(t, step)).Runs[0]
		if res.Pass || !strings.Contains(res.Failure, "not a valid recording") {
			t.Errorf("%s: %+v", name, res)
		}
		if strings.Contains(res.Failure, "not-a-real-secret") {
			t.Errorf("%s: the failure echoes the file: %q", name, res.Failure)
		}
		if len(journalPayloads(t, sup.order[0], "fake.market.*")) != 0 {
			t.Errorf("%s: the op was called", name)
		}
		entries, err := os.ReadDir(filepath.Join(sup.order[0].home, "lab", "recordings"))
		if err == nil && len(entries) != 0 {
			t.Errorf("%s: %d file(s) copied into the home", name, len(entries))
		}
	}
}
