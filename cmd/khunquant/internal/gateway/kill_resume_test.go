package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// With no valid owner configured nobody can be "the web owner", so the launcher's Stop
// (always allowed) must not leave a flag nothing can clear: both clearing routes work,
// whatever the launcher gate says. With an owner configured the gate stays (403).
func TestKillClear_NoOwnerConfiguredIsOpen(t *testing.T) {
	for name, owner := range map[string]config.OwnerConfig{
		"absent":  {},
		"invalid": {Channel: "telegram", ChatID: rtOwnerCht, SenderID: "not a canonical id"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newManageRig(t, false)
			registerControlAPI(r.mux, r.kill, nil, testMgmtToken, r.cfg)
			r.cfg.Proactive.Owner = owner
			armGate(t, false)
			if _, ok := r.cfg.Proactive.OwnerRoute(); ok {
				t.Fatal("the test needs no valid owner")
			}

			if code, _ := r.do(http.MethodPost, "/api/control/kill"); code != http.StatusOK || !r.kill.engaged {
				t.Fatalf("engage: %d, engaged %v", code, r.kill.engaged)
			}
			code, body := r.do(http.MethodDelete, "/api/control/kill")
			if code != http.StatusOK || body["killed"] != false || r.kill.engaged {
				t.Fatalf("DELETE with no owner: %d %v, engaged %v", code, body, r.kill.engaged)
			}
			if code, _ := r.do(http.MethodPost, "/api/control/kill"); code != http.StatusOK || !r.kill.engaged {
				t.Fatalf("engage again: %d, engaged %v", code, r.kill.engaged)
			}
			code, body = r.do(http.MethodPost, "/api/proactive/resume")
			if code != http.StatusOK || body["killed"] != false || r.kill.engaged {
				t.Fatalf("resume with no owner: %d %v, engaged %v", code, body, r.kill.engaged)
			}
		})
	}
}

// An owner IS configured but the web is not that owner (the launcher has no
// authentication): both clearing routes stay 403, as before.
func TestKillClear_OwnerConfiguredButNotWebOwnerIsRefused(t *testing.T) {
	r := newManageRig(t, false)
	registerControlAPI(r.mux, r.kill, nil, testMgmtToken, r.cfg)
	r.kill.engaged = true
	armGate(t, false)
	if _, ok := r.cfg.Proactive.OwnerRoute(); !ok {
		t.Fatal("the test needs a valid owner")
	}
	if code, _ := r.do(http.MethodDelete, "/api/control/kill"); code != http.StatusForbidden || !r.kill.engaged {
		t.Fatalf("DELETE: %d, engaged %v", code, r.kill.engaged)
	}
	if code, _ := r.do(http.MethodPost, "/api/proactive/resume"); code != http.StatusForbidden || !r.kill.engaged {
		t.Fatalf("resume: %d, engaged %v", code, r.kill.engaged)
	}
}

// A nil configuration is a wiring fault, not "no owner": clearing is refused.
func TestMayClearKill_NilConfigRefuses(t *testing.T) {
	if mayClearKill(nil) {
		t.Fatal("a nil config may clear the kill switch")
	}
}

// TestKillProxy: the kill switch needs only the guard (the dashboard's authentication,
// which the launcher enforces), never the web owner; resume, which lets the assistant
// act again, needs the web owner. Both are the same flag: the kill the launcher's
// /api/agent/proactive/kill proxies to is the existing /api/control/kill.
func TestKillProxy(t *testing.T) {
	r := newManageRig(t, false)
	registerControlAPI(r.mux, r.kill, nil, testMgmtToken, r.cfg)
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

// DELETE /api/control/kill (resume) needs the web owner, as POST /api/proactive/resume
// does; POST (engage) never does. The token, loopback and browser guards still apply.
func TestControlKillDelete_NeedsTheWebOwner(t *testing.T) {
	r := newManageRig(t, false)
	registerControlAPI(r.mux, r.kill, nil, testMgmtToken, r.cfg)
	r.kill.engaged = true
	armGate(t, false)

	code, body := r.do(http.MethodDelete, "/api/control/kill")
	if code != http.StatusForbidden || !r.kill.engaged || body["web_owner_missing"] == "" {
		t.Fatalf("DELETE without the web owner: %d %v, engaged %v", code, body, r.kill.engaged)
	}
	if code, _ := r.do(http.MethodPost, "/api/control/kill"); code != http.StatusOK || !r.kill.engaged {
		t.Fatalf("engage without the web owner: %d, engaged %v", code, r.kill.engaged)
	}

	armGate(t, true)
	// The guards still come first: with the web owner active, each of these changes nothing.
	for name, mod := range map[string]func(*http.Request){
		"no token":    func(q *http.Request) { q.Header.Del("Authorization") },
		"wrong token": func(q *http.Request) { q.Header.Set("Authorization", "Bearer wrong") },
		"browser":     func(q *http.Request) { q.Header.Set("Origin", "http://evil.example") },
		"non-local":   func(q *http.Request) { q.RemoteAddr = "192.0.2.10:50000" },
	} {
		req := httptest.NewRequest(http.MethodDelete, "/api/control/kill", nil)
		req.RemoteAddr = "127.0.0.1:50000"
		req.Header.Set("Authorization", "Bearer "+testMgmtToken)
		mod(req)
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, req)
		if rec.Code < 400 || !r.kill.engaged {
			t.Errorf("DELETE %s: %d, engaged %v", name, rec.Code, r.kill.engaged)
		}
	}

	code, body = r.do(http.MethodDelete, "/api/control/kill")
	if code != http.StatusOK || body["killed"] != false || r.kill.engaged {
		t.Fatalf("DELETE with the web owner: %d %v, engaged %v", code, body, r.kill.engaged)
	}
}
