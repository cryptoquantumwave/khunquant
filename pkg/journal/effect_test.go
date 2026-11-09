package journal

import (
	"context"
	"encoding/json"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// markSteps is a one-step workflow whose function records whether it marked before its
// "send", and what Prior said on each attempt.
type markSteps struct {
	mu       sync.Mutex
	priors   []bool
	attempts []int
	sent     int
	mark     bool  // mark before the send
	failOnce error // the first attempt fails with this after the send
}

func (m *markSteps) def() Definition {
	return Definition{Kind: "mark", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("place", nil, func(ctx context.Context, attempt int) (any, error) {
			eff := EffectFrom(ctx)
			if eff == nil {
				return nil, errors.New("no Effect on the step context")
			}
			m.mu.Lock()
			m.priors = append(m.priors, eff.Prior)
			m.attempts = append(m.attempts, attempt)
			m.mu.Unlock()
			if m.mark {
				if err := eff.Mark(ctx); err != nil {
					return nil, err
				}
			}
			m.mu.Lock()
			m.sent++
			fail := m.failOnce
			m.failOnce = nil
			m.mu.Unlock()
			return nil, fail
		}, Retries(2))
	}}
}

func (m *markSteps) seen() ([]bool, []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.priors...), append([]int(nil), m.attempts...)
}

func eqBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A crash after the mark and before the output: the resumed attempt sees Prior.
func TestEffect_CrashAfterMark_ResumeSeesPrior(t *testing.T) {
	m := &markSteps{mark: true}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{m.def()})
	id := mustStart(t, a.Engine, "mark", nil)
	a.armKill(func(p, step string, _ int) bool { return p == pointAfterFn && step == "place" })
	a.exec(id)
	b := newNode(t, path, t0, []Definition{m.def()})
	b.clock.Advance(testLease + testGrace)
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("%+v", w)
	}
	if priors, attempts := m.seen(); !eqBools(priors, []bool{false, true}) || len(attempts) != 2 {
		t.Fatalf("priors %v attempts %v, want [false true] over two attempts", priors, attempts)
	}
}

// A crash after the step was journaled started and before the function reached its mark:
// nothing was sent, and the resumed attempt does not see Prior.
func TestEffect_CrashBeforeMark_ResumeSeesNoPrior(t *testing.T) {
	m := &markSteps{mark: true}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{m.def()})
	id := mustStart(t, a.Engine, "mark", nil)
	a.armKill(func(p, step string, _ int) bool { return p == pointAfterStarted && step == "place" })
	a.exec(id)
	b := newNode(t, path, t0, []Definition{m.def()})
	b.clock.Advance(testLease + testGrace)
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("%+v", w)
	}
	priors, attempts := m.seen()
	if !eqBools(priors, []bool{false}) || len(attempts) != 1 || attempts[0] != 2 {
		t.Fatalf("priors %v attempts %v, want one attempt (the 2nd) with Prior false", priors, attempts)
	}
}

// An in-process retry after an error: Prior follows whether the failed attempt marked.
func TestEffect_RetryAfterError_PriorFollowsTheMark(t *testing.T) {
	for _, mark := range []bool{true, false} {
		m := &markSteps{mark: mark, failOnce: errors.New("transient")}
		a := newNode(t, dbPath(t), t0, []Definition{m.def()})
		id := mustStart(t, a.Engine, "mark", nil)
		a.exec(id)
		if w := status(t, a, id); w.Status != StatusSucceeded {
			t.Fatalf("mark=%v: %+v", mark, w)
		}
		if priors, _ := m.seen(); !eqBools(priors, []bool{false, mark}) {
			t.Errorf("mark=%v: priors %v, want [false %v]", mark, priors, mark)
		}
	}
}

// A mark that cannot be committed is an error the function sees, and nothing is recorded.
func TestEffect_FailedMarkIsAnErrorAndLeavesNoMark(t *testing.T) {
	m := &markSteps{mark: true}
	a := newNode(t, dbPath(t), t0, []Definition{m.def()})
	id := mustStart(t, a.Engine, "mark", nil)
	a.st.fault = func(op string) error {
		if op == "mark_step" {
			return errors.New("disk full")
		}
		return nil
	}
	a.exec(id)
	var sent sql.NullInt64
	if err := a.db.QueryRow(`SELECT sent_at_ms FROM workflow_steps WHERE workflow_id = ?`, id).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent.Valid {
		t.Error("a mark that failed to commit left a mark")
	}
}

// The migration marks the steps that were unfinished, and only those: they may have sent an
// order under the code that had no marks, so a resume must treat them as possibly sent.
func TestEffect_MigrationMarksUnfinishedSteps(t *testing.T) {
	db := openAt(t, dbPath(t))
	ctx := context.Background()
	if err := sqlitex.Migrate(ctx, db, migrations[:1]...); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO workflows (id, kind, input, status, created_at_ms, updated_at_ms) VALUES ('w', 'k', '{}', 'running', 1, 1)`)
	exec(t, db, `INSERT INTO workflow_steps (workflow_id, seq, name, attempts, started_at_ms, first_started_at_ms, finished_at_ms, output) VALUES ('w', 1, 'done', 1, 5, 5, 6, '{}')`)
	exec(t, db, `INSERT INTO workflow_steps (workflow_id, seq, name, attempts, started_at_ms, first_started_at_ms) VALUES ('w', 2, 'open', 1, 7, 7)`)
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		t.Fatal(err)
	}
	var done, open sql.NullInt64
	if err := db.QueryRow(`SELECT sent_at_ms FROM workflow_steps WHERE seq = 1`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT sent_at_ms FROM workflow_steps WHERE seq = 2`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if done.Valid || !open.Valid || open.Int64 != 7 {
		t.Errorf("finished step mark %+v, unfinished step mark %+v; want none and 7", done, open)
	}
}
