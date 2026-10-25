package intent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func TestStore_EmptyReturnsDefaults(t *testing.T) {
	needZone(t, DefaultTimezone)
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Timezone != DefaultTimezone || p.NotificationBudgetPerDay != 3 || p.DigestTime != "08:00" {
		t.Fatalf("no row must read as the defaults: %+v", p)
	}
	if has, err := s.HasProfile(ctx); err != nil || has {
		t.Fatalf("HasProfile on an empty store = %v, %v", has, err)
	}
	if _, ok, err := s.Latest(ctx); err != nil || ok {
		t.Fatalf("Latest on an empty store = ok %v, err %v", ok, err)
	}
	if h, err := s.ProfileHistory(ctx, 10); err != nil || len(h) != 0 {
		t.Fatalf("history on an empty store = %v, %v", h, err)
	}
}

func TestPutProfile_Versioned(t *testing.T) {
	needZone(t, DefaultTimezone)
	s := newTestStore(t)
	ctx := context.Background()

	p := DefaultProfile()
	var versions []int64
	for i := 1; i <= 3; i++ {
		p.NotificationBudgetPerDay = i
		v, err := s.PutProfile(ctx, p, fmt.Sprintf("telegram:%d", i), proactive.TrustUserAuthored)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if !(versions[0] < versions[1] && versions[1] < versions[2]) {
		t.Fatalf("versions must increase: %v", versions)
	}

	cur, err := s.Profile(ctx)
	if err != nil || cur.NotificationBudgetPerDay != 3 {
		t.Fatalf("current profile = %+v, %v; want the newest", cur, err)
	}

	hist, err := s.ProfileHistory(ctx, 10)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history = %d rows, %v", len(hist), err)
	}
	for i, want := range []int{3, 2, 1} { // newest first
		if hist[i].Profile.NotificationBudgetPerDay != want || hist[i].Version != versions[len(versions)-1-i] {
			t.Errorf("history[%d] = v%d budget %d, want budget %d", i, hist[i].Version, hist[i].Profile.NotificationBudgetPerDay, want)
		}
		if hist[i].UpdatedBy != fmt.Sprintf("telegram:%d", want) || hist[i].Provenance != proactive.TrustUserAuthored {
			t.Errorf("history[%d] author/trust = %q/%v", i, hist[i].UpdatedBy, hist[i].Provenance)
		}
		if hist[i].UpdatedAt.IsZero() {
			t.Errorf("history[%d] has no timestamp", i)
		}
	}
	if two, _ := s.ProfileHistory(ctx, 2); len(two) != 2 || two[0].Version != versions[2] {
		t.Errorf("limit 2 returned %d rows", len(two))
	}
	latest, ok, err := s.Latest(ctx)
	if err != nil || !ok || latest.Version != versions[2] {
		t.Errorf("Latest = %+v, %v, %v", latest, ok, err)
	}

	// Bad input stores nothing and burns no version.
	bad := p
	bad.NotificationBudgetPerDay = 99
	if _, err := s.PutProfile(ctx, bad, "x", proactive.TrustUserAuthored); err == nil {
		t.Fatal("an invalid profile must be refused")
	}
	if _, err := s.PutProfile(ctx, p, "  \n", proactive.TrustUserAuthored); err == nil {
		t.Fatal("an empty author must be refused")
	}
	if _, err := s.PutProfile(ctx, p, "x", proactive.Trust(42)); err == nil {
		t.Fatal("an unknown trust level must be refused")
	}
	if h, _ := s.ProfileHistory(ctx, 10); len(h) != 3 {
		t.Fatalf("refused writes left %d versions, want 3", len(h))
	}
}

func TestPutProfile_HistoryCap(t *testing.T) {
	needZone(t, DefaultTimezone)
	s := newTestStore(t)
	ctx := context.Background()

	p := DefaultProfile()
	total := MaxProfileVersions + 5
	var last int64
	for i := 0; i < total; i++ {
		p.NotificationBudgetPerDay = i % (MaxBudgetPerDay + 1)
		p.Horizon = fmt.Sprintf("v%d", i)
		v, err := s.PutProfile(ctx, p, "tester", proactive.TrustUserAuthored)
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		last = v
	}
	hist, err := s.ProfileHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != MaxProfileVersions {
		t.Fatalf("kept %d versions, want exactly %d", len(hist), MaxProfileVersions)
	}
	if hist[0].Version != last || hist[0].Profile.Horizon != fmt.Sprintf("v%d", total-1) {
		t.Errorf("newest version missing: %+v", hist[0])
	}
	oldest := hist[len(hist)-1]
	if oldest.Profile.Horizon != fmt.Sprintf("v%d", total-MaxProfileVersions) {
		t.Errorf("oldest kept = %q, want the 5 oldest pruned", oldest.Profile.Horizon)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM profile_versions`).Scan(&n); err != nil || n != MaxProfileVersions {
		t.Errorf("table holds %d rows (%v), want %d", n, err, MaxProfileVersions)
	}
}

func TestProfile_ZeroBudgetIsStorable(t *testing.T) {
	needZone(t, DefaultTimezone)
	s := newTestStore(t)
	ctx := context.Background()

	p := DefaultProfile()
	p.NotificationBudgetPerDay = 0
	if _, err := s.PutProfile(ctx, p, "tester", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	got, err := s.Profile(ctx)
	if err != nil || got.NotificationBudgetPerDay != 0 {
		t.Fatalf("a stored 0 must read back as 0, got %+v, %v", got, err)
	}

	// A row that never said (older or hand-written JSON) reads as the default.
	if _, err := s.db.Exec(
		`INSERT INTO profile_versions (profile_json, updated_at_ms, updated_by, trust) VALUES (?, 1, 'legacy', 3)`,
		`{"timezone":"UTC"}`); err != nil {
		t.Fatal(err)
	}
	got, err = s.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.NotificationBudgetPerDay != DefaultBudgetPerDay || got.Timezone != "UTC" || got.DigestTime != "08:00" ||
		got.ActiveHours != (HoursRange{"07:00", "22:00"}) {
		t.Fatalf("unset fields must read as defaults: %+v", got)
	}

	// An explicit 0 in a hand-written row is still 0.
	if _, err := s.db.Exec(
		`INSERT INTO profile_versions (profile_json, updated_at_ms, updated_by, trust) VALUES (?, 2, 'legacy', 3)`,
		`{"notification_budget_per_day":0}`); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Profile(ctx); got.NotificationBudgetPerDay != 0 {
		t.Fatalf("explicit 0 in stored JSON must stay 0: %+v", got)
	}
}

func TestStore_ConcurrentPutAndRead(t *testing.T) {
	needZone(t, DefaultTimezone)
	s := newTestStore(t)
	ctx := context.Background()

	const writers = 50
	var wg sync.WaitGroup
	got := make([]int64, writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := DefaultProfile()
			p.Horizon = fmt.Sprintf("writer-%d", i)
			p.NotificationBudgetPerDay = i % (MaxBudgetPerDay + 1)
			got[i], errs[i] = s.PutProfile(ctx, p, fmt.Sprintf("w%d", i), proactive.TrustUserAuthored)
		}(i)
		wg.Add(1)
		go func() { // readers run alongside the writers
			defer wg.Done()
			if _, err := s.Profile(ctx); err != nil {
				t.Errorf("read during writes: %v", err)
			}
		}()
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i := 0; i < writers; i++ {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if seen[got[i]] {
			t.Fatalf("version %d handed out twice", got[i])
		}
		seen[got[i]] = true
	}

	hist, err := s.ProfileHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != writers {
		t.Fatalf("history holds %d versions, want %d (a lost update)", len(hist), writers)
	}
	horizons := map[string]bool{}
	for i, v := range hist {
		if i > 0 && v.Version >= hist[i-1].Version {
			t.Fatalf("versions not strictly increasing: %d then %d", hist[i-1].Version, v.Version)
		}
		horizons[v.Profile.Horizon] = true
		if !seen[v.Version] {
			t.Errorf("history has version %d that no writer received", v.Version)
		}
	}
	if len(horizons) != writers {
		t.Fatalf("only %d distinct profiles survived, want %d", len(horizons), writers)
	}
	cur, _ := s.Profile(ctx)
	if cur.Horizon != hist[0].Profile.Horizon {
		t.Errorf("current profile is not the newest version")
	}
}

func TestIntent_NoCrossPackageCallsInTx(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/intent")
}

func TestNewStore_Idempotent(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ { // a reload runs the migration again
		if _, err := NewStore(ctx, db); err != nil {
			t.Fatalf("NewStore #%d: %v", i+1, err)
		}
	}
	if _, err := NewStore(ctx, nil); err == nil {
		t.Fatal("a nil database must be refused")
	}
}

func TestImportIfEmpty_OnlyOnce(t *testing.T) {
	needZone(t, "Asia/Bangkok")
	s := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Missing file and unfilled template: nothing imported, no rows.
	if ok, err := s.ImportIfEmpty(ctx, filepath.Join(dir, "absent.md")); err != nil || ok {
		t.Fatalf("missing file: imported=%v err=%v", ok, err)
	}
	template := filepath.Join(repoRoot(t), "workspace", "USER.md")
	if ok, err := s.ImportIfEmpty(ctx, template); err != nil || ok {
		t.Fatalf("unfilled template: imported=%v err=%v", ok, err)
	}
	if has, _ := s.HasProfile(ctx); has {
		t.Fatal("nothing usable was found, so no row may be created")
	}

	// A filled file imports once, as the owner's own words.
	path := writeFile(t, dir, "USER.md", "- Timezone: Asia/Bangkok\n- Risk tolerance: aggressive\n")
	ok, err := s.ImportIfEmpty(ctx, path)
	if err != nil || !ok {
		t.Fatalf("filled file: imported=%v err=%v", ok, err)
	}
	v, _, _ := s.Latest(ctx)
	if v.UpdatedBy != "import:USER.md" || v.Provenance != proactive.TrustUserAuthored || v.Profile.RiskTolerance != "aggressive" {
		t.Fatalf("imported version = %+v", v)
	}

	// Never again, even from a different file, and not over a later edit.
	other := writeFile(t, dir, "OTHER.md", "- Risk tolerance: conservative\n")
	if ok, err := s.ImportIfEmpty(ctx, other); err != nil || ok {
		t.Fatalf("second import: imported=%v err=%v", ok, err)
	}
	if h, _ := s.ProfileHistory(ctx, 10); len(h) != 1 {
		t.Fatalf("history has %d versions after a second import, want 1", len(h))
	}
}

func TestImportIfEmpty_Concurrent(t *testing.T) {
	needZone(t, "Asia/Bangkok")
	s := newTestStore(t)
	path := writeFile(t, t.TempDir(), "USER.md", "- Risk tolerance: moderate\n")
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.ImportIfEmpty(context.Background(), path)
			if err != nil {
				t.Errorf("import: %v", err)
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()
	n := 0
	for _, ok := range results {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d concurrent imports reported success, want exactly 1", n)
	}
	if h, _ := s.ProfileHistory(context.Background(), 10); len(h) != 1 {
		t.Fatalf("%d versions after concurrent import, want 1", len(h))
	}
}
