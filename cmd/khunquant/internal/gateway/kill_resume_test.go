package gateway

import (
	"net/http"
	"testing"
)

// TestKillProxy: the kill switch needs only the guard (the dashboard's authentication,
// which the launcher enforces), never the web owner; resume, which lets the assistant
// act again, needs the web owner. Both are the same flag: the kill the launcher's
// /api/agent/proactive/kill proxies to is the existing /api/control/kill.
func TestKillProxy(t *testing.T) {
	r := newManageRig(t, false)
	registerControlAPI(r.mux, r.kill, nil, testMgmtToken)
	armGate(t, false) // no web owner

	code, body := r.do(http.MethodPost, "/api/control/kill")
	if code != http.StatusOK || body["killed"] != true || !r.kill.engaged {
		t.Fatalf("kill without the web owner: %d %v", code, body)
	}

	code, body = r.do(http.MethodPost, "/api/proactive/resume")
	if code != http.StatusForbidden || !r.kill.engaged || body["web_owner_missing"] == "" {
		t.Fatalf("resume without the web owner: %d %v, engaged %v", code, body, r.kill.engaged)
	}

	armGate(t, true)
	code, body = r.do(http.MethodPost, "/api/proactive/resume")
	if code != http.StatusOK || body["killed"] != false || r.kill.engaged {
		t.Fatalf("resume with the web owner: %d %v, engaged %v", code, body, r.kill.engaged)
	}

	// The kill route is unchanged by the web owner being off or on.
	armGate(t, false)
	if code, _ := r.do(http.MethodPost, "/api/control/kill"); code != http.StatusOK || !r.kill.engaged {
		t.Fatalf("kill again: %d, engaged %v", code, r.kill.engaged)
	}
}
