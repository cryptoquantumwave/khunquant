package sqlitextest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fixtureModule = "example.com/m"

func scanSource(t *testing.T, src string) []string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "x.go")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := findCrossPackageCallsInTx([]string{file}, fixtureModule, fixtureModule+"/pkg/mine")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

const header = `package mine

import (
	"context"
	"database/sql"
	"fmt"

	"example.com/m/pkg/other"
	al "example.com/m/pkg/aliased"
	"example.com/m/pkg/proactive"
	"example.com/m/pkg/logger"
	"example.com/m/pkg/mine/sub"
)

type Store struct {
	db    *sql.DB
	peer  *other.Store
	peers []al.Thing
	ok    int
}

var _ = fmt.Sprint
var _ = sub.X
`

func TestTxGuard_Detects(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string // substring expected in a violation
	}{
		"package func in a tx": {
			body: `func (s *Store) A(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return err }
	other.Notify("x")
	return tx.Commit()
}`, want: "other.Notify",
		},
		"Begin without ctx": {
			body: `func (s *Store) A() error {
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	_ = other.Do()
	return tx.Commit()
}`, want: "other.Do",
		},
		"aliased import": {
			body: `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	al.Go()
	return tx.Commit()
}`, want: "al.Go",
		},
		"inside a loop over rows": {
			body: `func (s *Store) A(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "select 1")
	if err != nil { return err }
	defer rows.Close()
	for rows.Next() {
		other.Handle()
	}
	return rows.Err()
}`, want: "other.Handle",
		},
		"method of a field with a foreign type": {
			body: `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	s.peer.Save()
	return tx.Commit()
}`, want: "s.peer.Save",
		},
		"method of a parameter with a foreign type": {
			body: `func (s *Store) A(ctx context.Context, p *other.Store) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	p.Save()
	return tx.Commit()
}`, want: "p.Save",
		},
		"method of a local var with a foreign type": {
			body: `func A(ctx context.Context, db *sql.DB) error {
	var p other.Store
	tx, _ := db.BeginTx(ctx, nil)
	p.Save()
	return tx.Commit()
}`, want: "p.Save",
		},
		"sub-package of the current package counts as another package": {
			body: `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	sub.X()
	return tx.Commit()
}`, want: "sub.X",
		},
		"inside a closure": {
			body: `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	f := func() { other.Do() }
	f()
	return tx.Commit()
}`, want: "other.Do",
		},
		"call in a deferred closure does not end the region": {
			body: `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	defer func() { _ = tx.Rollback() }()
	other.Do()
	return tx.Commit()
}`, want: "other.Do",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := scanSource(t, header+tc.body)
			if len(got) == 0 {
				t.Fatal("violation not detected")
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("violations %q do not mention %q", got, tc.want)
			}
		})
	}
}

func TestTxGuard_AllowsSafeCode(t *testing.T) {
	for name, body := range map[string]string{
		"call after commit": `func (s *Store) A(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return err }
	if _, err := tx.ExecContext(ctx, "x"); err != nil { _ = tx.Rollback(); return err }
	if err := tx.Commit(); err != nil { return err }
	other.Notify("after")
	s.peer.Save()
	return nil
}`,
		"call before begin": `func (s *Store) A(ctx context.Context) error {
	other.Prepare()
	tx, _ := s.db.BeginTx(ctx, nil)
	return tx.Commit()
}`,
		"call after rows are fully read": `func (s *Store) A(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "select 1")
	if err != nil { return err }
	defer rows.Close()
	for rows.Next() {
		var x int
		_ = rows.Scan(&x)
	}
	if err := rows.Err(); err != nil { return err }
	other.Handle()
	return nil
}`,
		"allowed vocabulary and logging packages": `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	id := proactive.NewID()
	logger.Info(id)
	return tx.Commit()
}`,
		"stdlib and same-package calls": `func (s *Store) A(ctx context.Context) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	_ = fmt.Sprintf("%d", 1)
	s.helper(tx)
	helper2()
	return tx.Commit()
}
func (s *Store) helper(tx *sql.Tx) {}
func helper2() {}`,
		"no transaction at all": `func (s *Store) A() { other.Do(); s.peer.Save() }`,
		"unexported selector on a foreign var": `func (s *Store) A(ctx context.Context, p *other.Store) error {
	tx, _ := s.db.BeginTx(ctx, nil)
	p.save()
	return tx.Commit()
}`,
		"non-sql Query with one result": `func (s *Store) A(u fakeURL) {
	q := u.Query()
	other.Do()
	_ = q
}
type fakeURL struct{}
func (fakeURL) Query() int { return 0 }`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := scanSource(t, header+body); len(got) != 0 {
				t.Errorf("false positive: %v", got)
			}
		})
	}
}

// Run the guard over this module's own sqlitex package now; later MRs add
// their package directories to a call like this one.
func TestNoNestedStoreCallsInTx(t *testing.T) {
	AssertNoCrossPackageCallsInTx(t, "pkg/sqlitex", "pkg/sqlitex/sqlitextest")
}

// recorder captures what the assertion reports instead of failing the test.
type recorder struct {
	testing.TB
	errs   []string
	fatals []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (r *recorder) Fatal(args ...any) {
	r.fatals = append(r.fatals, fmt.Sprint(args...))
	runtime.Goexit()
}

// run executes fn in its own goroutine so a Goexit from Fatal ends only that.
func (r *recorder) run(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
}

// The guard must be able to fail: point it at a deliberately bad package.
func TestAssertNoCrossPackageCallsInTx_FailsOnViolation(t *testing.T) {
	root, module, err := findModule()
	if err != nil {
		t.Fatal(err)
	}
	if module != "github.com/cryptoquantumwave/khunquant" {
		t.Fatalf("module = %q", module)
	}

	// The fixture lives under testdata, which the go tool and the linter ignore.
	r := &recorder{TB: t}
	r.run(func() { AssertNoCrossPackageCallsInTx(r, "pkg/sqlitex/sqlitextest/testdata/txviolation") })
	if len(r.errs) != 1 || !strings.Contains(r.errs[0], "sqlitex.Open") {
		t.Errorf("errors = %v, fatals = %v; want one error naming sqlitex.Open", r.errs, r.fatals)
	}

	// Misuse is loud, never a silent pass.
	for name, dirs := range map[string][]string{
		"no directories":     nil,
		"missing directory":  {"pkg/does_not_exist"},
		"directory w/o code": {"pkg/sqlitex/sqlitextest/testdata"},
		"outside the module": {filepath.Dir(root)},
	} {
		r := &recorder{TB: t}
		r.run(func() { AssertNoCrossPackageCallsInTx(r, dirs...) })
		if len(r.fatals) == 0 && len(r.errs) == 0 {
			t.Errorf("%s: no failure reported", name)
		}
	}
}
