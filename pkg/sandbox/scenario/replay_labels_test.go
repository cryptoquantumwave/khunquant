package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const labelsScenario = `
expect:
  - id: crit
    message: {to: owner, severity: critical}
    within: {from: "+12m", to: "+16m"}
  - id: warn
    message: {to: owner, contains: [BTC]}
    within: {from: "+20m", to: "+30m"}
  - id: reply
    message: {to: owner, contains: [hello]}
    within: {from: "+1m", to: "+2m"}
  - id: quiet
    message: {to: owner, count: 0}
    within: {from: "+40m", to: "+50m"}
  - id: quiet-labelled
    message: {to: owner, count: 0, replay_match: "price_cross"}
    within: {from: "+40m", to: "+50m"}
  - id: open-ended
    message: {to: owner}
  - id: not-a-message
    decision: {trigger: x}
    within: {from: "+1m", to: "+2m"}
`

func labelsResult(f *File) *Result {
	sent := func(key, sev string) SentMsg { return SentMsg{Seq: 1, DedupKey: key, Severity: sev} }
	res := &Result{Expect: []ExpectResult{{ID: "settled", OK: true}}}
	for i := range f.Expect {
		e := ExpectResult{ID: expectID(i, &f.Expect[i]), OK: true}
		switch e.ID {
		case "crit":
			e.Sent = []SentMsg{sent("liq:okx/main:crit", "critical"), sent("liq:okx/main:crit", "critical")}
		case "warn":
			e.Sent = []SentMsg{sent("liq:okx/main:warn", "warning"), sent("price_cross:abc", "warning")}
		case "reply":
			e.Sent = []SentMsg{sent("", "info")}
		}
		res.Expect = append(res.Expect, e)
	}
	return res
}

func TestReplayLabelsFromDedupKeys(t *testing.T) {
	f := scenarioOf(t, strings.TrimPrefix(labelsScenario, "\n"))
	labels, skipped := ReplayLabels(f, labelsResult(f))
	var got []string
	for _, l := range labels {
		got = append(got, l.Match+"|"+l.From.Format("15:04")+"-"+l.To.Format("15:04")+"|"+map[bool]string{true: "fire", false: "silent"}[l.ShouldFire]+"|"+map[bool]string{true: "crit", false: "-"}[l.Critical])
	}
	// clock.start is 09:00+07:00 = 02:00Z; windows are offsets from it, in UTC.
	want := []string{
		"liq:okx/main:crit|02:12-02:16|fire|crit",
		"liq:okx/main:warn|02:20-02:30|fire|-",
		"price_cross:abc|02:20-02:30|fire|-",
		"price_cross|02:40-02:50|silent|-",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v\nwant     %v", got, want)
	}
	if labels[0].Note != "t/crit" {
		t.Errorf("note = %q", labels[0].Note)
	}
	for _, id := range []string{"reply", "quiet:", "open-ended"} {
		if !strings.Contains(strings.Join(skipped, "\n"), id) {
			t.Errorf("skipped %v does not name %s", skipped, id)
		}
	}
}

// A count-0 expectation without replay_match is named in skipped, never dropped silently.
func TestReplayLabelsNegativeNeedsReplayMatch(t *testing.T) {
	f := scenarioOf(t, "expect:\n  - id: quiet\n    message: {to: owner, count: 0}\n    within: {from: \"+1m\", to: \"+9m\"}\n")
	labels, skipped := ReplayLabels(f, labelsResult(f))
	if len(labels) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "quiet") || !strings.Contains(skipped[0], "replay_match") {
		t.Errorf("labels %v, skipped %v; want one skip naming quiet and replay_match", labels, skipped)
	}
}

// replay_match is validated like every other expectation field.
func TestValidate_ReplayMatch(t *testing.T) {
	for in, want := range map[string]string{
		"message: {to: owner, count: 0, replay_match: x}\n    within: {from: +1m, to: +9m}": "",
		"message: {to: owner, count: 1, replay_match: x}\n    within: {from: +1m, to: +9m}": "count: 0",
		"message: {to: owner, replay_match: x}\n    within: {from: +1m, to: +9m}":          "count: 0",
		"message: {to: owner, count: 0, replay_match: x}":                                   "within",
		"message: {to: owner, count: 0, replay_match: x}\n    within: {from: +1m}":          "within",
		"message: {to: owner, count: 0, replay_match: \"\"}\n    within: {from: +1m, to: +9m}": "must not be empty",
		"message: {to: owner, count: 0, replay_match: [a]}\n    within: {from: +1m, to: +9m}": "one value",
		"decision: {trigger: x, replay_match: y}":                                          "message",
	} {
		issues := Validate(parseString(t, base+"expect:\n  - "+in+"\n"), nil)
		got := issueAt(issues, "/expect/0/replay_match")
		switch {
		case want == "" && got != "":
			t.Errorf("%q: %s", in, got)
		case want != "" && !strings.Contains(got, want):
			t.Errorf("%q: got %q, want it to say %q (%v)", in, got, want, issues)
		}
	}
	// A misspelt modifier is not a modifier: the expectation then has two kind keys.
	issues := Validate(parseString(t, base+"expect:\n  - message: {to: owner, count: 0}\n    replay_mach: x\n"), nil)
	if issueAt(issues, "/expect/0") == "" {
		t.Errorf("a misspelt replay_match must fail: %v", issues)
	}
}

func TestCopyReplayCase(t *testing.T) {
	home, out := t.TempDir(), t.TempDir()
	src := filepath.Join(home, "lab", "export", "c1")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range replayCaseFiles {
		if err := os.WriteFile(filepath.Join(src, f), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(out, "replay")
	dest, err := CopyReplayCase(src, home, dir, "c1", false)
	if err != nil || dest != filepath.Join(dir, "c1") {
		t.Fatalf("copy: %q, %v", dest, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "labels.json")); string(b) != "labels.json" {
		t.Errorf("labels.json = %q", b)
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("%v: want the case only, no staging directory", es)
	}
	// An existing case is never overwritten unless asked.
	if err := os.WriteFile(filepath.Join(src, "golden.json"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyReplayCase(src, home, dir, "c1", false); err == nil {
		t.Error("an existing case was overwritten without force")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "golden.json")); string(b) != "golden.json" {
		t.Errorf("a refused copy changed the case: %q", b)
	}
	if _, err := CopyReplayCase(src, home, dir, "c1", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "golden.json")); string(b) != "new" {
		t.Errorf("force did not replace the case: %q", b)
	}
	// The destination stays below dir; the source stays inside the home; a symlink is no file.
	for _, name := range []string{"../c1", "a/b", ""} {
		if _, err := CopyReplayCase(src, home, dir, name, true); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	if _, err := CopyReplayCase(src, t.TempDir(), dir, "c2", false); err == nil {
		t.Error("a source outside the home was accepted")
	}
	secret := filepath.Join(out, "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "policy.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "policy.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyReplayCase(src, home, dir, "c3", false); err == nil {
		t.Error("a symlink was copied")
	}
	if _, err := os.Stat(filepath.Join(dir, "c3")); err == nil {
		t.Error("a refused copy left a case")
	}
}
