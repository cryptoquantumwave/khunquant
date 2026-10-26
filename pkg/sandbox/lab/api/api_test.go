package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func newMux(t *testing.T, guard func(http.Handler) http.Handler) *http.ServeMux {
	t.Helper()
	l := &lab.Lab{}
	err := l.AddGroup(lab.Group{Name: "g", Ops: []lab.Op{
		{Name: "echo", Run: func(_ context.Context, a json.RawMessage) (any, error) {
			return map[string]any{"got": a}, nil
		}},
		{Name: "bad", Run: func(context.Context, json.RawMessage) (any, error) { return nil, lab.Errorf(lab.CodeBadArgs, "nope") }},
		{Name: "gone", Run: func(context.Context, json.RawMessage) (any, error) {
			return nil, lab.Errorf(lab.CodeUnavailable, "later")
		}},
		{Name: "boom", Run: func(context.Context, json.RawMessage) (any, error) { panic("x") }},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, l, guard)
	return mux
}

func post(mux http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestRegisterRoutesCallsAndMapsErrors(t *testing.T) {
	guarded := 0
	mux := newMux(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; h.ServeHTTP(w, r) })
	})
	for _, tc := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/api/sandbox/lab/g", `{"op":"echo","x":1}`, 200, ""},
		{"/api/sandbox/lab/g", `{"op":"bad"}`, 400, lab.CodeBadArgs},
		{"/api/sandbox/lab/g", `{"op":"nope"}`, 400, lab.CodeUnknownOp},
		{"/api/sandbox/lab/g", `{"op":"gone"}`, 503, lab.CodeUnavailable},
		{"/api/sandbox/lab/g", `{"op":"boom"}`, 500, lab.CodeFailed},
		{"/api/sandbox/lab/nope", `{"op":"echo"}`, 404, lab.CodeUnknownGroup},
	} {
		rec := post(mux, tc.path, tc.body)
		if rec.Code != tc.status {
			t.Errorf("%s %s = %d (%s), want %d", tc.path, tc.body, rec.Code, rec.Body, tc.status)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || (tc.code != "" && out["code"] != tc.code) {
			t.Errorf("%s %s body = %s, want code %q", tc.path, tc.body, rec.Body, tc.code)
		}
	}
	if guarded != 6 {
		t.Errorf("the guard ran %d times, want once per request", guarded)
	}
	if got := post(mux, "/api/sandbox/lab/g", `{"op":"echo","x":1}`).Body.String(); !strings.Contains(got, `"x":1`) {
		t.Errorf("the handler did not pass the whole request object: %s", got)
	}
}

func TestRegisterRefusesGetAndHugeBody(t *testing.T) {
	mux := newMux(t, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sandbox/lab/g", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
	if rec := post(mux, "/api/sandbox/lab/g", `{"op":"echo","pad":"`+strings.Repeat("x", maxBody)+`"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d, want 413", rec.Code)
	}
}
