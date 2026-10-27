package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// GET /api/sandbox/lab lists the registry under the lab guard: the instance token, and
// the same groups the POST route serves.
func TestGroupsRoute_ListsGroupsUnderTheLabGuard(t *testing.T) {
	f := newLockedFixture(t)
	addProbeGroup(t, f.lab)

	f.expectStatus("/api/sandbox/lab", "", http.StatusUnauthorized)
	f.expectStatus("/api/sandbox/lab", "wrong-token", http.StatusUnauthorized)
	body := f.expectStatus("/api/sandbox/lab", lockTestToken, http.StatusOK)

	var out struct {
		Groups []struct {
			Name string
			Ops  []struct{ Name string }
		}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	names := map[string]bool{}
	for _, g := range out.Groups {
		names[g.Name] = true
	}
	for _, want := range []string{"probe", "inspect", "market", "chat"} {
		if !names[want] {
			t.Errorf("the group list lacks %q: %s", want, body)
		}
	}
}

// An ordinary gateway has no lab route at all, the group list included.
func TestGroupsRoute_NotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	// The token is good for the sandbox API, so the 404 below is the missing route.
	f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusOK)
	if code, body, err := getStatus(f.url("/api/sandbox/lab"), testSandboxToken); err != nil || code != http.StatusNotFound {
		t.Fatalf("GET /api/sandbox/lab = %d (%v) %s, want 404", code, err, body)
	}
}
