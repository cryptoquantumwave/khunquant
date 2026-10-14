package control_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// The kill flag is what approval.Service asks.
var _ approval.Killer = (*control.State)(nil)

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newState(t *testing.T, db *sql.DB, clock *stepClock) *control.State {
	t.Helper()
	s, err := control.NewState(context.Background(), db, control.WithStateClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestState_EngageAndClear(t *testing.T) {
	ctx := context.Background()
	clock := &stepClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	s := newState(t, openDB(t, filepath.Join(t.TempDir(), "p.db")), clock)

	if s.Killed() {
		t.Fatal("engaged on a fresh database")
	}
	if changed, err := s.Clear(ctx, "x"); err != nil || changed {
		t.Errorf("Clear when off: %v, %v", changed, err)
	}
	if changed, err := s.Engage(ctx, "telegram:1", "first"); err != nil || !changed {
		t.Fatalf("Engage: %v, %v", changed, err)
	}
	if !s.Killed() {
		t.Error("not killed right after Engage in the same process")
	}
	// Engaging again keeps the first author and time.
	clock.Add(time.Minute)
	if changed, err := s.Engage(ctx, "telegram:2", "second"); err != nil || changed {
		t.Errorf("second Engage: %v, %v", changed, err)
	}
	info, on, err := s.Info(ctx)
	if err != nil || !on || info.By != "telegram:1" || info.Reason != "first" ||
		!info.At.Equal(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("Info = %+v, %v, %v", info, on, err)
	}
	if changed, err := s.Clear(ctx, "telegram:1"); err != nil || !changed {
		t.Fatalf("Clear: %v, %v", changed, err)
	}
	if s.Killed() {
		t.Error("still killed after Clear")
	}
	if _, on, _ := s.Info(ctx); on {
		t.Error("Info still engaged")
	}
}

// The flag survives a restart: a new State over the same file starts engaged.
func TestState_SurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	clock := &stepClock{t: time.Now()}
	db1 := openDB(t, path)
	s1 := newState(t, db1, clock)
	if _, err := s1.Engage(context.Background(), "telegram:1", "/stop"); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := newState(t, openDB(t, path), clock)
	if !s2.Killed() {
		t.Fatal("the kill flag did not survive a restart")
	}
	if _, err := s2.Clear(context.Background(), "telegram:1"); err != nil {
		t.Fatal(err)
	}
	if newState(t, openDB(t, path), clock).Killed() {
		t.Error("the cleared flag came back")
	}
}

// A second handle (the CLI, or the gateway route's process) sees a change within
// the refresh bound, and not before it: Killed is a memory read in between.
func TestState_SecondHandleSeesItWithinTheRefreshBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	clock := &stepClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	a := newState(t, openDB(t, path), clock)
	b := newState(t, openDB(t, path), clock)

	if _, err := a.Engage(context.Background(), "api", "test"); err != nil {
		t.Fatal(err)
	}
	if b.Killed() {
		t.Fatal("the second handle read the table again before the refresh interval")
	}
	clock.Add(control.DefaultRefresh - time.Millisecond)
	if b.Killed() {
		t.Fatal("seen before the bound")
	}
	clock.Add(time.Millisecond)
	if !b.Killed() {
		t.Fatalf("not seen after %v", control.DefaultRefresh)
	}

	if _, err := a.Clear(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	clock.Add(control.DefaultRefresh)
	if b.Killed() {
		t.Error("the clear was not seen within the bound")
	}
}

// An unreadable row is treated as engaged; a vanished row as not engaged.
func TestState_UnreadableRowIsEngaged(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "p.db"))
	clock := &stepClock{t: time.Now()}
	s := newState(t, db, clock)
	if _, err := db.Exec(`UPDATE control_state SET value_json = '{not json' WHERE key = ?`, control.KillKey); err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	if !s.Killed() {
		t.Error("an unreadable kill switch row read as off")
	}
	if _, err := db.Exec(`DELETE FROM control_state`); err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	if s.Killed() {
		t.Error("a missing row read as engaged")
	}
	// And Engage works from a missing row.
	if changed, err := s.Engage(context.Background(), "x", ""); err != nil || !changed || !s.Killed() {
		t.Errorf("Engage from a missing row: %v, %v", changed, err)
	}
}

// A read that fails means "engaged", not "keep the last answer": the switch exists to
// stop the assistant acting, so a switch that cannot be read refuses acts. It
// recovers on the next good read, with one WARN when it starts and one INFO when it
// ends.
func TestState_FailsClosedOnAReadError(t *testing.T) {
	var buf lockedBuffer
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	db := openDB(t, filepath.Join(t.TempDir(), "p.db"))
	clock := &stepClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	s := newState(t, db, clock)
	if s.Killed() {
		t.Fatal("killed on a fresh database")
	}

	// The database starts failing: the table is gone.
	if _, err := db.Exec(`DROP TABLE control_state`); err != nil {
		t.Fatal(err)
	}
	if s.Killed() {
		t.Fatal("answered from the database before the refresh interval")
	}
	clock.Add(control.DefaultRefresh)
	for i := 0; i < 3; i++ {
		if !s.Killed() {
			t.Fatalf("read %d: not killed although the switch cannot be read", i)
		}
		clock.Add(control.DefaultRefresh) // each one a failing refresh
	}

	// It recovers.
	if _, err := db.Exec(`CREATE TABLE control_state (key TEXT PRIMARY KEY, value_json TEXT NOT NULL);
		INSERT INTO control_state (key, value_json) VALUES ('kill_engaged_at', 'null')`); err != nil {
		t.Fatal(err)
	}
	if s.Killed() { // the next refresh is due: a good read
		t.Fatal("still killed after a good read")
	}

	out := ansi.ReplaceAllString(buf.String(), "")
	if n := strings.Count(out, "Could not read the kill switch"); n != 1 {
		t.Errorf("%d WARN lines for one outage, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "The kill switch can be read again"); n != 1 {
		t.Errorf("%d recovery lines, want 1:\n%s", n, out)
	}
}

func TestState_NilIsNeverKilled(t *testing.T) {
	var s *control.State
	if s.Killed() {
		t.Error("a nil State is killed")
	}
}

// Many callers at once: one engages, everyone sees it, and the race detector is quiet.
func TestState_Concurrent(t *testing.T) {
	s := newState(t, openDB(t, filepath.Join(t.TempDir(), "p.db")), &stepClock{t: time.Now()})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Killed()
			}
		}()
	}
	if _, err := s.Engage(context.Background(), "x", ""); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !s.Killed() {
		t.Error("not killed")
	}
}

// The kill flag blocks a risk-increasing approval through the real State, and
// lets a reducing one through: the approval package's own tests cover the service
// with a fake killer; this pins that State answers the way that fake does.
func TestState_DrivesTheApprovalKillerContract(t *testing.T) {
	s := newState(t, openDB(t, filepath.Join(t.TempDir(), "p.db")), &stepClock{t: time.Now()})
	var k approval.Killer = s
	if k.Killed() {
		t.Fatal("killed before /stop")
	}
	if _, err := s.Engage(context.Background(), "x", ""); err != nil {
		t.Fatal(err)
	}
	if !k.Killed() {
		t.Fatal("not killed after /stop")
	}
}
