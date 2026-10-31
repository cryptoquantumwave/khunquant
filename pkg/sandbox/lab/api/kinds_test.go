package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func TestGroupListCarriesDeclaredKinds(t *testing.T) {
	l := &lab.Lab{}
	l.DeclareKinds("chat.out", "event.*")
	mux := http.NewServeMux()
	Register(mux, l, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sandbox/lab", nil))
	var out struct {
		Groups []any    `json:"groups"`
		Kinds  []string `json:"kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Kinds) != 2 || out.Kinds[1] != "event.*" {
		t.Fatalf("GET /api/sandbox/lab = %s (%v), want the declared kinds", rec.Body, err)
	}
}
