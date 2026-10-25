package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// csrfSentinel records whether the wrapped handler was reached, which is the
// property that matters: a rejected request must not run the handler at all,
// not merely fail to change anything.
func csrfSentinel() (http.Handler, *bool) {
	reached := false
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	return h, &reached
}

func TestCSRFMutations_RejectsCrossSiteMutations(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, hdr := range []struct{ name, value string }{
			{"Sec-Fetch-Site", "cross-site"},
			{"Sec-Fetch-Site", "same-site"},
			{"Origin", "http://evil.example"},
		} {
			inner, reached := csrfSentinel()
			h := CSRFMutations(inner)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/api/gateway/restart", nil)
			req.Header.Set(hdr.name, hdr.value)
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with %s: %s → status %d, want %d",
					method, hdr.name, hdr.value, rec.Code, http.StatusForbidden)
			}
			if *reached {
				t.Errorf("%s with %s: %s reached the handler", method, hdr.name, hdr.value)
			}
		}
	}
}

// A headerless request is rejected too: the launcher UI always supplies at
// least one identifying header, so an unidentified caller is not the UI.
func TestCSRFMutations_RejectsHeaderlessMutations(t *testing.T) {
	inner, reached := csrfSentinel()
	h := CSRFMutations(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/reset", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if *reached {
		t.Fatal("headerless mutation reached the handler")
	}
}

// The pass-through half, proved generically so the per-route table in package
// api does not have to run handlers that talk to the network.
func TestCSRFMutations_AllowsSameOriginMutations(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		inner, reached := csrfSentinel()
		h := CSRFMutations(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/gateway/restart", nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || !*reached {
			t.Errorf("%s same-origin: status %d reached=%v, want %d reached=true",
				method, rec.Code, *reached, http.StatusOK)
		}
	}
}

// An Origin matching the request's own scheme and host is how a script
// authenticates its origin; httptest requests carry Host "example.com".
func TestCSRFMutations_AllowsMatchingOrigin(t *testing.T) {
	inner, reached := csrfSentinel()
	h := CSRFMutations(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/gateway/restart", nil)
	req.Header.Set("Origin", "http://example.com")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !*reached {
		t.Fatalf("status = %d reached = %v, want %d reached = true", rec.Code, *reached, http.StatusOK)
	}
}

// Reads are never blocked: CSRF defences that cover GET break ordinary
// navigation, and a read changes nothing.
func TestCSRFMutations_AllowsReads(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		inner, reached := csrfSentinel()
		h := CSRFMutations(inner)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/config", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		h.ServeHTTP(rec, req)

		if !*reached {
			t.Errorf("%s: cross-site read was blocked (status %d)", method, rec.Code)
		}
	}
}

// Non-/api paths are the embedded frontend assets; they are not state-changing
// and must not start returning 403.
func TestCSRFMutations_IgnoresNonAPIPaths(t *testing.T) {
	inner, reached := csrfSentinel()
	h := CSRFMutations(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth/callback", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(rec, req)

	if !*reached {
		t.Fatalf("cross-site POST to a non-/api path was blocked (status %d)", rec.Code)
	}
}
