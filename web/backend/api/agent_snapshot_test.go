package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// The web list shows the owner's snapshots; the scheduled automatic ones appear only
// when label=auto is asked for, and any of them can still be opened by id.
func TestSnapshotListHidesAutomaticRowsUnlessAsked(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshot.NewStore(cfg.WorkspacePath())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	ownerID, err := store.SaveSnapshot(ctx, &snapshot.Snapshot{TakenAt: now.Add(-time.Hour), Quote: "USDT", TotalValue: 111, Label: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	autoID, err := store.SaveAutoSnapshot(ctx, &snapshot.Snapshot{TakenAt: now, Quote: "USDT", TotalValue: 999})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	mux := http.NewServeMux()
	NewHandler(configPath).registerAgentSnapshotRoutes(mux)
	get := func(path string) (int, []byte) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code, w.Body.Bytes()
	}
	list := func(path string) []snapshotListItem {
		t.Helper()
		code, body := get(path)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, code, body)
		}
		var items []snapshotListItem
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatal(err)
		}
		return items
	}

	if items := list("/api/agent/snapshots"); len(items) != 1 || items[0].ID != ownerID {
		t.Errorf("default list = %+v, want only the owner's snapshot %d", items, ownerID)
	}
	if items := list("/api/agent/snapshots?label=auto"); len(items) != 1 || items[0].ID != autoID {
		t.Errorf("label=auto list = %+v, want only the automatic snapshot %d", items, autoID)
	}
	if code, _ := get("/api/agent/snapshots/" + strconv.FormatInt(autoID, 10)); code != http.StatusOK {
		t.Errorf("an automatic snapshot cannot be opened by id: status %d", code)
	}
}
