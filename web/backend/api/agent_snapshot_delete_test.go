package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// deleteSnapshotFixture saves one snapshot with two positions and returns the route
// mux, its id and the database path.
func deleteSnapshotFixture(t *testing.T) (mux *http.ServeMux, id int64, dbPath string) {
	t.Helper()
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	t.Cleanup(cleanup)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.NewStore(cfg.WorkspacePath())
	if err != nil {
		t.Fatal(err)
	}
	id, err = store.SaveSnapshot(context.Background(), &snapshot.Snapshot{
		TakenAt: time.Now().UTC(), Quote: "USDT", TotalValue: 100, Label: "daily",
		Positions: []snapshot.Position{
			{Source: "binance", Asset: "BTC", Quantity: 1, Quote: "USDT", Price: 60, Value: 60},
			{Source: "binance", Asset: "ETH", Quantity: 2, Quote: "USDT", Price: 20, Value: 40},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	mux = http.NewServeMux()
	NewHandler(configPath).registerAgentSnapshotRoutes(mux)
	return mux, id, filepath.Join(cfg.WorkspacePath(), "memory", "snapshots", "snapshots.db")
}

func deleteSnapshot(mux *http.ServeMux, id int64) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/agent/snapshots/"+strconv.FormatInt(id, 10), nil))
	return w
}

// Deleting a snapshot takes its positions with it: the delete must run with foreign
// keys on, on the connection that deletes.
func TestDeleteSnapshotCascadesToItsPositions(t *testing.T) {
	mux, id, dbPath := deleteSnapshotFixture(t)

	db, err := sqlitex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	count := func() (n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT COUNT(*) FROM positions WHERE snapshot_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 2 {
		t.Fatalf("positions before the delete = %d, want 2", n)
	}

	if w := deleteSnapshot(mux, id); w.Code != http.StatusOK {
		t.Fatalf("delete: status %d: %s", w.Code, w.Body.String())
	}
	if n := count(); n != 0 {
		t.Errorf("positions after the delete = %d, want 0 (the cascade did not run)", n)
	}
	if w := deleteSnapshot(mux, id); w.Code != http.StatusNotFound {
		t.Errorf("a second delete: status %d, want 404", w.Code)
	}
}

// A delete that meets a writer waits for it: the gateway writes this file on its own,
// and a delete that fails with "database is locked" is a 500 the owner sees.
func TestDeleteSnapshotWaitsForAWriterInsteadOfFailing(t *testing.T) {
	mux, id, dbPath := deleteSnapshotFixture(t)

	holder, err := sqlitex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	var released atomic.Bool
	release := make(chan struct{})
	rolledBack := make(chan struct{})
	go func() {
		defer close(rolledBack)
		<-release
		released.Store(true)
		conn.ExecContext(context.Background(), "ROLLBACK") //nolint:errcheck
		conn.Close()
	}()
	time.AfterFunc(200*time.Millisecond, func() { close(release) })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- deleteSnapshot(mux, id) }()

	select {
	case w := <-done:
		// The writer is released only after 200ms; the delete cannot have been applied
		// before then.
		if w.Code != http.StatusOK {
			t.Fatalf("delete during a write: status %d: %s", w.Code, w.Body.String())
		}
		if !released.Load() {
			t.Fatal("the delete finished while the other handle still held the write lock")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the delete did not finish")
	}
	<-rolledBack
}
