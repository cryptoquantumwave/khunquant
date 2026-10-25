package intent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t testing.TB, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const filledUserMD = `# User

## Preferences

- Communication style: casual
- Timezone: Asia/Bangkok
- Language: Thai

## Personal Information

- Name: Somchai
- Thai Tax ID: (optional)

## Investor Profile

- Risk tolerance: Aggressive
- Investment horizon: long-term 5yr+
- Primary investment goal: wealth accumulation

## Portfolio Allocation Targets

Describe your target allocation, e.g.:
- Crypto: 40%
- Thai equities: 35%
- US equities: 10.5%
- Cash / money market: 10%
- Gold: 4%

## Strategy Notes

- Crypto: 99%
`

func TestImportFromUserMD_Template(t *testing.T) {
	needZone(t, "Asia/Bangkok")

	// The real template, as shipped: nothing has been answered yet.
	tmplPath := filepath.Join(repoRoot(t), "workspace", "USER.md")
	data, err := os.ReadFile(tmplPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("the workspace USER.md template did not load: %v (%d bytes)", err, len(data))
	}
	p, found, err := ImportFromUserMD(tmplPath)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("the unfilled template must not count as an answer: %+v", p)
	}

	// A filled sample: every recognised key lands.
	path := writeFile(t, t.TempDir(), "USER.md", filledUserMD)
	got, found, err := ImportFromUserMD(path)
	if err != nil || !found {
		t.Fatalf("filled sample: found=%v err=%v", found, err)
	}
	want := DefaultProfile()
	want.Timezone = "Asia/Bangkok"
	want.Language = "th"
	want.RiskTolerance = "aggressive"
	want.Horizon = "long-term 5yr+"
	want.PrimaryGoal = "wealth accumulation"
	want.AllocationTargets = map[string]float64{
		"crypto": 40, "th_equity": 35, "us_equity": 10.5, "cash": 10, "gold": 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed profile\n got %+v\nwant %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("an imported profile must be storable: %v", err)
	}
}

func TestImportFromUserMD_Partial(t *testing.T) {
	needZone(t, "Asia/Bangkok")
	dir := t.TempDir()

	// The shipped example allocation, edited by one number, is an answer.
	edited := writeFile(t, dir, "a.md", "## Portfolio Allocation Targets\n- Crypto: 31%\n- Thai equities: 50%\n- Cash / money market: 19%\n")
	p, found, err := ImportFromUserMD(edited)
	if err != nil || !found || p.AllocationTargets["crypto"] != 31 {
		t.Fatalf("edited example: found=%v err=%v %+v", found, err, p.AllocationTargets)
	}

	// Percent lines outside the allocation section are not allocation.
	other := writeFile(t, dir, "b.md", "## Strategy Notes\n- Crypto: 30%\n")
	if _, found, _ := ImportFromUserMD(other); found {
		t.Fatal("a percentage under another heading is not an allocation target")
	}

	// Language "both", empty and placeholder values say nothing.
	empty := writeFile(t, dir, "c.md", "- Language: both\n- Timezone:\n- Risk tolerance: (conservative / moderate / aggressive)\n")
	if _, found, _ := ImportFromUserMD(empty); found {
		t.Fatal("placeholders and empty values must be ignored")
	}

	// Invalid values are dropped one by one; the good ones survive.
	mixed := writeFile(t, dir, "d.md",
		"- Timezone: Mars/Olympus\n- Risk tolerance: reckless\n- Language: English\n"+
			"## Portfolio Allocation Targets\n- Crypto: 70%\n- Cash: 50%\n- Gold: 20%\n")
	p, found, err = ImportFromUserMD(mixed)
	if err != nil || !found {
		t.Fatalf("mixed: found=%v err=%v", found, err)
	}
	if p.Timezone != DefaultTimezone || p.RiskTolerance != "" || p.Language != "en" {
		t.Fatalf("bad values must fall back to defaults and good ones stay: %+v", p)
	}
	if !reflect.DeepEqual(p.AllocationTargets, map[string]float64{"crypto": 70, "gold": 20}) {
		t.Fatalf("a line that overshoots 100%% must be dropped alone: %v", p.AllocationTargets)
	}

	// Unreadable file: an error that wraps os.ErrNotExist.
	if _, _, err := ImportFromUserMD(filepath.Join(dir, "missing.md")); !os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("missing file error = %v", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}
