package sqlitex_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func applied(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT component || ':' || version FROM schema_migrations ORDER BY component, version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestMigrate_Idempotent(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	ms := []sqlitex.Migration{
		{Component: "intent", Version: 1, SQL: `CREATE TABLE intent(id INTEGER PRIMARY KEY); INSERT INTO intent VALUES (1);`},
		{Component: "intent", Version: 2, SQL: `ALTER TABLE intent ADD COLUMN note TEXT`},
	}
	for i := 0; i < 3; i++ {
		if err := sqlitex.Migrate(ctx, db, ms...); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got, want := applied(t, db), []string{"intent:1", "intent:2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("applied = %v, want %v", got, want)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM intent`).Scan(&n); err != nil || n != 1 {
		t.Errorf("migration body ran %d times (err %v), want once", n, err)
	}
	var at int64
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations LIMIT 1`).Scan(&at); err != nil || at < 1_600_000_000_000 {
		t.Errorf("applied_at = %d (err %v), want unix milliseconds", at, err)
	}

	// Calling later with a longer list applies only the new step.
	ms = append(ms, sqlitex.Migration{Component: "intent", Version: 3, SQL: `CREATE INDEX intent_note ON intent(note)`})
	if err := sqlitex.Migrate(ctx, db, ms...); err != nil {
		t.Fatal(err)
	}
	if got := applied(t, db); len(got) != 3 {
		t.Errorf("applied = %v, want 3 rows", got)
	}
	// No migrations at all is fine and still ensures the bookkeeping table.
	if err := sqlitex.Migrate(ctx, sqlitextest.Open(t)); err != nil {
		t.Errorf("empty Migrate: %v", err)
	}
}

func TestMigrate_OrderAndTransactional(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE log(seq INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT)`); err != nil {
		t.Fatal(err)
	}
	step := func(label string) string {
		return `CREATE TABLE t_` + label + `(x); INSERT INTO log(label) VALUES ('` + label + `');`
	}

	// Supplied out of order; applied by (Component, Version).
	err := sqlitex.Migrate(ctx, db,
		sqlitex.Migration{Component: "b", Version: 2, SQL: step("b2")},
		sqlitex.Migration{Component: "a", Version: 2, SQL: step("a2")},
		sqlitex.Migration{Component: "b", Version: 1, SQL: step("b1")},
		sqlitex.Migration{Component: "a", Version: 1, SQL: step("a1")},
	)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT label FROM log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		order = append(order, l)
	}
	_ = rows.Close()
	if want := []string{"a1", "a2", "b1", "b2"}; !reflect.DeepEqual(order, want) {
		t.Errorf("apply order = %v, want %v", order, want)
	}

	// A failing step: its earlier statements roll back, nothing is recorded, and
	// later steps never run. Earlier steps stay applied.
	err = sqlitex.Migrate(ctx, db,
		sqlitex.Migration{Component: "c", Version: 1, SQL: step("c1")},
		sqlitex.Migration{Component: "c", Version: 2, SQL: `CREATE TABLE t_c2(x); INSERT INTO log(label) VALUES ('c2'); INSERT INTO no_such_table VALUES (1);`},
		sqlitex.Migration{Component: "c", Version: 3, SQL: step("c3")},
	)
	if err == nil || !strings.Contains(err.Error(), "c v2") {
		t.Fatalf("expected a failure naming c v2, got %v", err)
	}
	if !tableExists(t, db, "t_c1") {
		t.Error("c1 should have stayed applied")
	}
	if tableExists(t, db, "t_c2") {
		t.Error("the failed step's CREATE TABLE was not rolled back")
	}
	if tableExists(t, db, "t_c3") {
		t.Error("a step after the failure ran")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM log WHERE label IN ('c2','c3')`).Scan(&n); err != nil || n != 0 {
		t.Errorf("failed step left %d log rows (err %v)", n, err)
	}
	if got, want := applied(t, db), []string{"a:1", "a:2", "b:1", "b:2", "c:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded = %v, want %v", got, want)
	}

	// The connection must have been released cleanly: further work succeeds, and
	// once the step is fixed the run resumes where it stopped.
	err = sqlitex.Migrate(ctx, db,
		sqlitex.Migration{Component: "c", Version: 1, SQL: step("c1")},
		sqlitex.Migration{Component: "c", Version: 2, SQL: step("c2")},
		sqlitex.Migration{Component: "c", Version: 3, SQL: step("c3")},
	)
	if err != nil {
		t.Fatalf("resume after fix: %v", err)
	}
	if got := applied(t, db); len(got) != 7 {
		t.Errorf("recorded = %v, want 7 rows", got)
	}
}

func TestMigrate_CancelledContext(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sqlitex.Migrate(ctx, db, sqlitex.Migration{Component: "x", Version: 1, SQL: "CREATE TABLE x(a)"}); err == nil {
		t.Error("a cancelled context must fail the migration")
	}
	if tableExists(t, db, "x") {
		t.Error("cancelled migration left its table behind")
	}
	// The pool is still healthy.
	if err := sqlitex.Migrate(context.Background(), db, sqlitex.Migration{Component: "x", Version: 1, SQL: "CREATE TABLE x(a)"}); err != nil {
		t.Errorf("migrate after cancel: %v", err)
	}
}

func TestMigrate_RejectsGapsAndDuplicates(t *testing.T) {
	ok := func(c string, v int) sqlitex.Migration {
		return sqlitex.Migration{Component: c, Version: v, SQL: "CREATE TABLE " + c + "_" + string(rune('a'+v)) + "(x)"}
	}
	for name, ms := range map[string][]sqlitex.Migration{
		"starts at 2":    {ok("a", 2)},
		"starts at 0":    {ok("a", 0)},
		"negative":       {ok("a", -1)},
		"gap":            {ok("a", 1), ok("a", 3)},
		"duplicate":      {ok("a", 1), ok("a", 1)},
		"dup after gap":  {ok("a", 1), ok("a", 2), ok("a", 2)},
		"second comp":    {ok("a", 1), ok("b", 2)},
		"empty comp":     {{Component: "", Version: 1, SQL: "SELECT 1"}},
		"blank comp":     {{Component: "  ", Version: 1, SQL: "SELECT 1"}},
		"empty sql":      {{Component: "a", Version: 1, SQL: "  \n"}},
		"later gap safe": {ok("a", 1), ok("b", 1), ok("b", 3)},
	} {
		t.Run(name, func(t *testing.T) {
			db := sqlitextest.Open(t)
			if err := sqlitex.Migrate(context.Background(), db, ms...); err == nil {
				t.Fatal("expected a validation error")
			}
			// Validation happens before anything is touched, even for valid prefixes.
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err != nil || n != 0 {
				t.Errorf("schema has %d objects after a rejected call (err %v)", n, err)
			}
		})
	}
}

func TestMigrate_NewerSchemaRefused(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	v := func(c string, n int) sqlitex.Migration {
		return sqlitex.Migration{Component: c, Version: n, SQL: "CREATE TABLE " + c + string(rune('0'+n)) + "(x)"}
	}
	// A "newer binary" knows three steps of attention and two of intent.
	if err := sqlitex.Migrate(ctx, db, v("attention", 1), v("attention", 2), v("attention", 3), v("intent", 1), v("intent", 2)); err != nil {
		t.Fatal(err)
	}

	// An older binary knows only up to attention v2: refuse.
	err := sqlitex.Migrate(ctx, db, v("attention", 1), v("attention", 2), v("intent", 1), v("intent", 2))
	if err == nil || !strings.Contains(err.Error(), "attention") || !strings.Contains(err.Error(), "v3") {
		t.Fatalf("expected a refusal naming attention v3, got %v", err)
	}
	before := applied(t, db)

	// Same binary, a component with nothing recorded, and components the caller
	// does not mention are all fine.
	if err := sqlitex.Migrate(ctx, db, v("attention", 1), v("attention", 2), v("attention", 3), v("feedback", 1)); err != nil {
		t.Fatalf("matching versions plus a new component: %v", err)
	}
	if err := sqlitex.Migrate(ctx, db, v("feedback", 1)); err != nil {
		t.Fatalf("a caller migrating only its own component: %v", err)
	}
	if got := applied(t, db); len(got) != len(before)+1 {
		t.Errorf("applied = %v (before %v)", got, before)
	}

	// A refusal must not apply anything, including other components' new steps.
	err = sqlitex.Migrate(ctx, db, v("attention", 1), v("attention", 2), v("zzz", 1))
	if err == nil {
		t.Fatal("expected refusal")
	}
	if tableExists(t, db, "zzz1") {
		t.Error("a refused call still applied another component's migration")
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Many goroutines on one handle, and two handles on one file (standing in for
// two processes), all call Migrate at once: every step runs exactly once.
func TestMigrate_ConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	open := func() *sql.DB {
		db, err := sqlitex.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	dbs := []*sql.DB{open(), open()}

	var ms []sqlitex.Migration
	for _, c := range []string{"attention", "intent", "journal"} {
		ms = append(ms,
			sqlitex.Migration{Component: c, Version: 1, SQL: `CREATE TABLE ` + c + `_runs(n); INSERT INTO ` + c + `_runs(n) VALUES (1);`},
			sqlitex.Migration{Component: c, Version: 2, SQL: `INSERT INTO ` + c + `_runs(n) VALUES (2); ALTER TABLE ` + c + `_runs ADD COLUMN extra TEXT;`},
			sqlitex.Migration{Component: c, Version: 3, SQL: `INSERT INTO ` + c + `_runs(n) VALUES (3)`},
		)
	}

	const callers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- sqlitex.Migrate(context.Background(), dbs[i%len(dbs)], ms...)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}

	for _, c := range []string{"attention", "intent", "journal"} {
		if n := countRows(t, dbs[0], c+"_runs"); n != 3 {
			t.Errorf("%s migrations produced %d rows, want 3 (each step exactly once)", c, n)
		}
	}
	if n := countRows(t, dbs[1], "schema_migrations"); n != 9 {
		t.Errorf("schema_migrations has %d rows, want 9", n)
	}
}
