package triggers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// logUpdates records every UPDATE of the triggers table, so that a test can count
// the writes the registry makes.
func logUpdates(e *env) {
	e.t.Helper()
	for _, q := range []string{
		`CREATE TABLE upd_log (id TEXT, state TEXT)`,
		`CREATE TRIGGER t_upd AFTER UPDATE ON triggers BEGIN INSERT INTO upd_log VALUES (new.id, new.state_json); END`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) updates() int { return e.count(`SELECT COUNT(*) FROM upd_log`) }

func TestRegistry_StatePersistedOnlyWhenDirty(t *testing.T) {
	// call 1: reads, does not touch its state;  call 2: changes it and fires;
	// call 3: marks it dirty with the same bytes;  call 4: changes it, fires nothing.
	var seen []string
	e := newEnv(t, tickFactory(func(call int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		seen = append(seen, string(st.Raw))
		switch call {
		case 2:
			st.Raw, st.Dirty = json.RawMessage(`{"side":"below"}`), true
			return []proactive.Candidate{cand(testKind, "below")}, nil
		case 3:
			st.Raw, st.Dirty = json.RawMessage(`{"side":"below"}`), true
		case 4:
			st.Raw, st.Dirty = json.RawMessage(`{"side":"above"}`), true
		}
		return nil, nil
	}))
	e.add("s1", testKind, ModeLive, `{}`)
	logUpdates(e)

	e.eval(kindTick) // 1
	if n := e.updates(); n != 0 {
		t.Fatalf("a clean evaluation wrote %d times", n)
	}

	cs := e.evalUncommitted(kindTick) // 2: decided later
	if _, st, _ := e.row("s1"); st != "" || e.updates() != 0 {
		t.Fatalf("the state was written before attention decided: %q (%d writes)", st, e.updates())
	}
	e.r.Committed(context.Background(), idsOf(cs))
	if _, st, _ := e.row("s1"); st != `{"side":"below"}` || e.updates() != 1 {
		t.Fatalf("after the commit: state %q, %d writes", st, e.updates())
	}

	e.eval(kindTick) // 3: dirty, but nothing changed
	if n := e.updates(); n != 1 {
		t.Fatalf("an unchanged state was written again (%d writes)", n)
	}

	e.eval(kindTick) // 4: changed, no candidate: nothing to wait for
	if _, st, _ := e.row("s1"); st != `{"side":"above"}` || e.updates() != 2 {
		t.Fatalf("a state change with no candidate: state %q, %d writes", st, e.updates())
	}
	if want := []string{"", "", `{"side":"below"}`, `{"side":"below"}`}; strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("the trigger saw states %q, want %q", seen, want)
	}
}

// The in-memory state moves on only after the commit too: a decision attention failed
// to record must not leave the trigger believing it has already fired.
func TestRegistry_StateIsNotAdoptedUntilTheDecisionIsRecorded(t *testing.T) {
	e := newEnv(t, tickFactory(edge(testKind)))
	e.add("s1", testKind, ModeLive, `{}`)
	ctx := context.Background()

	first := e.evalUncommitted(kindTick)
	if len(first) != 1 {
		t.Fatalf("first = %+v", first)
	}
	e.r.Committed(ctx, nil) // attention returned an error for it
	second := e.evalUncommitted(kindTick)
	if len(second) != 1 {
		t.Fatalf("the alert was lost: the trigger did not fire again (%+v)", second)
	}
	if first[0].ID == second[0].ID {
		t.Fatalf("two candidates share the ID %q", first[0].ID)
	}
	e.r.Committed(ctx, []string{first[0].ID}) // a stale ID names nothing of this event
	if _, st, _ := e.row("s1"); st != "" {
		t.Fatalf("a stale commit saved the state: %q", st)
	}
	third := e.evalUncommitted(kindTick)
	if len(third) != 1 {
		t.Fatalf("third = %+v", third)
	}
	e.r.Committed(ctx, idsOf(third))
	if got := e.eval(kindTick); len(got) != 0 {
		t.Fatalf("it fired again after a recorded decision: %+v", got)
	}
	if st := e.r.Stats(); st.Discarded != 2 || st.StateSaved != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// A trigger with two candidates keeps its old state unless both were decided.
func TestRegistry_AllCandidatesOfAnEvaluationMustBeDecided(t *testing.T) {
	e := newEnv(t, tickFactory(func(_ int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		if len(st.Raw) > 0 {
			return nil, nil
		}
		st.Raw, st.Dirty = json.RawMessage(`{"fired":2}`), true
		return []proactive.Candidate{cand(testKind, "one"), cand(testKind, "two")}, nil
	}))
	e.add("s1", testKind, ModeLive, `{}`)
	cs := e.evalUncommitted(kindTick)
	e.r.Committed(context.Background(), idsOf(cs[:1]))
	if _, st, _ := e.row("s1"); st != "" {
		t.Fatalf("saved with one of two decided: %q", st)
	}
	cs = e.evalUncommitted(kindTick)
	e.r.Committed(context.Background(), idsOf(cs))
	if _, st, _ := e.row("s1"); st != `{"fired":2}` {
		t.Fatalf("state = %q", st)
	}
}

func TestRegistry_StateIsLoadedLazilyAndSurvivesARestart(t *testing.T) {
	save := func(call int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		st.Raw, st.Dirty = json.RawMessage(fmt.Sprintf(`{"n":%d}`, call)), true
		return nil, nil
	}
	e := newEnv(t, tickFactory(save))
	e.add("s1", testKind, ModeLive, `{}`)
	e.eval(kindTick)
	e.eval(kindTick)

	var seen string
	r2 := e.registry(tickFactory(func(_ int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		seen = string(st.Raw)
		return nil, nil
	}))
	if r2.snap.Load().byID["s1"].stateLoaded {
		t.Fatal("the state was read when the registry was built, not when it was first needed")
	}
	// An event the trigger is not routed to does not load it either.
	if _, err := r2.Evaluate(context.Background(), fakeView{now: noon}, diffOf(kindPort)); err != nil {
		t.Fatal(err)
	}
	if r2.snap.Load().byID["s1"].stateLoaded {
		t.Fatal("an unrouted event loaded the state")
	}
	e.r = r2
	e.eval(kindTick)
	if seen != `{"n":2}` {
		t.Fatalf("the trigger restarted with state %q, want {\"n\":2}", seen)
	}
}

func TestRegistry_OversizedOrInvalidStateIsAnError(t *testing.T) {
	for name, raw := range map[string]string{
		"too big":     `"` + strings.Repeat("x", MaxStateBytes) + `"`,
		"not json":    `{broken`,
		"exactly max": `"` + strings.Repeat("x", MaxStateBytes-2) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tickFactory(func(_ int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
				st.Raw, st.Dirty = json.RawMessage(raw), true
				return nil, nil
			}))
			e.add("s1", testKind, ModeLive, `{}`)
			e.eval(kindTick)
			_, st, _ := e.row("s1")
			if name == "exactly max" {
				if st != raw || e.r.Stats().Errors != 0 {
					t.Fatalf("a state of exactly the limit was refused: %+v", e.r.Stats())
				}
				return
			}
			if st != "" || e.r.Stats().InvalidState != 1 {
				t.Fatalf("a bad state got through: %d bytes saved, stats %+v", len(st), e.r.Stats())
			}
		})
	}
}

// An evaluation that was running when its spec was updated must not overwrite the
// new spec's state, and one for a removed spec must not bring its row back.
func TestRegistry_LateCommitCannotTouchAChangedOrRemovedSpec(t *testing.T) {
	e := newEnv(t, tickFactory(edge(testKind)))
	e.add("s1", testKind, ModeLive, `{}`)
	ctx := context.Background()

	cs := e.evalUncommitted(kindTick)
	if err := e.r.Update(ctx, Spec{ID: "s1", Kind: testKind, Mode: ModeShadow, Origin: "t", Params: json.RawMessage(`{"tag":"v2"}`)}); err != nil {
		t.Fatal(err)
	}
	e.r.Committed(ctx, idsOf(cs))
	if mode, st, _ := e.row("s1"); mode != ModeShadow || st != "" {
		t.Fatalf("a late commit wrote into the updated spec: mode %s state %q", mode, st)
	}

	cs = e.evalUncommitted(kindTick)
	if err := e.r.Remove(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	e.r.Committed(ctx, idsOf(cs))
	if n := e.count(`SELECT COUNT(*) FROM triggers`); n != 0 {
		t.Fatalf("a late commit brought a removed row back (%d rows)", n)
	}
}

func TestRegistry_UpdateKeepsOrClearsState(t *testing.T) {
	e := newEnv(t, tickFactory(edge(testKind)))
	e.add("s1", testKind, ModeLive, `{"tag":"a"}`)
	ctx := context.Background()
	e.eval(kindTick)
	if _, st, _ := e.row("s1"); st == "" {
		t.Fatal("no state saved")
	}
	// Only the mode changes: the state stays, and the trigger stays quiet.
	if err := e.r.Update(ctx, Spec{ID: "s1", Kind: testKind, Mode: ModeShadow, Origin: "t", Params: json.RawMessage(`{"tag":"a"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, st, _ := e.row("s1"); st == "" {
		t.Fatal("a mode change cleared the state")
	}
	if got := e.eval(kindTick); len(got) != 0 {
		t.Fatalf("fired again after a mode change: %+v", got)
	}
	// The parameters change: the state belongs to the old ones and is cleared.
	if err := e.r.Update(ctx, Spec{ID: "s1", Kind: testKind, Mode: ModeLive, Origin: "t", Params: json.RawMessage(`{"tag":"b"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, st, _ := e.row("s1"); st != "" {
		t.Fatalf("state survived a change of parameters: %q", st)
	}
	if got := e.eval(kindTick); len(got) != 1 {
		t.Fatalf("a new condition did not start from nothing: %+v", got)
	}
}

// A failed save leaves the state adopted in memory (no repeat from this process) and
// is retried on the next change.
func TestRegistry_FailedSaveIsRetried(t *testing.T) {
	e := newEnv(t, tickFactory(func(call int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		st.Raw, st.Dirty = json.RawMessage(fmt.Sprintf(`{"n":%d}`, call)), true
		return nil, nil
	}))
	e.add("s1", testKind, ModeLive, `{}`)
	if _, err := e.db.Exec(`CREATE TRIGGER block BEFORE UPDATE ON triggers BEGIN SELECT RAISE(ABORT, 'locked'); END`); err != nil {
		t.Fatal(err)
	}
	e.eval(kindTick)
	if st := e.r.Stats(); st.SaveFailures != 1 || st.StateSaved != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := e.db.Exec(`DROP TRIGGER block`); err != nil {
		t.Fatal(err)
	}
	e.eval(kindTick)
	if _, st, _ := e.row("s1"); st != `{"n":2}` {
		t.Fatalf("state after the retry = %q", st)
	}
}

func tableCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNewRegistry_CreatesTheTableAndNeedsADatabase(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRegistry(ctx, nil, nil, nil); err == nil {
		t.Fatal("a nil database was accepted")
	}
	db := sqlitextest.Open(t)
	if n := tableCount(t, db); n != 0 {
		t.Fatalf("a fresh database has %d tables", n)
	}
	if _, err := NewRegistry(ctx, db, nil, nil); err != nil {
		t.Fatal(err)
	}
	var comp string
	if err := db.QueryRow(`SELECT component FROM schema_migrations`).Scan(&comp); err != nil || comp != Component {
		t.Fatalf("migration component = %q, %v", comp, err)
	}
	// Building it again is harmless.
	if _, err := NewRegistry(ctx, db, nil, nil); err != nil {
		t.Fatal(err)
	}
	// With no global mode function nothing runs.
	r, err := NewRegistry(ctx, db, nil, tickFactory(fires(testKind)), WithClock(perceptiontest.NewClock(noon).Now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(ctx, Spec{Kind: testKind, Mode: ModeLive, Origin: "t"}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := r.Evaluate(ctx, fakeView{now: noon}, diffOf(kindTick)); len(cs) != 0 {
		t.Fatalf("evaluated with no global mode: %+v", cs)
	}
}
