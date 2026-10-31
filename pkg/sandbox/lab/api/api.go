// Package api is the HTTP front-end of the lab group registry: POST
// /api/sandbox/lab/<group> with a body {"op": ..., ...args}.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

const maxBody = 1 << 20

var statusOf = map[string]int{
	lab.CodeUnknownGroup: http.StatusNotFound,
	lab.CodeUnknownOp:    http.StatusBadRequest,
	lab.CodeBadArgs:      http.StatusBadRequest,
	lab.CodeUnavailable:  http.StatusServiceUnavailable,
	lab.CodeFailed:       http.StatusInternalServerError,
}

// groupInfo and opInfo are the JSON view of a lab.Group (a lab.Op carries a func, which
// does not marshal): what GET /api/sandbox/lab returns.
type groupInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Ops         []opInfo `json:"ops"`
}

type opInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Args        json.RawMessage `json:"args,omitempty"` // JSON Schema "properties" of the op
	Required    []string        `json:"required,omitempty"`
}

// Register mounts the lab routes on mux: POST /api/sandbox/lab/<group> and GET
// /api/sandbox/lab, the list of groups with their ops and argument schemas and the
// journal kinds the instance declares. guard wraps
// both handlers: the caller supplies the loopback and instance-token checks, so this
// package knows no secret. A call runs under lab.CallTimeout and a body is capped at 1 MiB.
func Register(mux interface {
	Handle(pattern string, h http.Handler)
}, l *lab.Lab, guard func(http.Handler) http.Handler) {
	mux.Handle("GET /api/sandbox/lab", guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		groups := []groupInfo{}
		for _, g := range l.Groups() {
			gi := groupInfo{Name: g.Name, Description: g.Description, Ops: []opInfo{}}
			for _, o := range g.Ops {
				gi.Ops = append(gi.Ops, opInfo{Name: o.Name, Description: o.Description, Args: o.Args, Required: o.Required})
			}
			groups = append(groups, gi)
		}
		reply(w, http.StatusOK, map[string]any{"groups": groups, "kinds": l.Kinds()})
	})))
	mux.Handle("POST /api/sandbox/lab/{group}", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error(), "code": lab.CodeBadArgs})
			return
		}
		res, err := l.Call(r.Context(), r.PathValue("group"), lab.OpOf(body), body)
		if err != nil {
			var le *lab.Error
			if !errors.As(err, &le) {
				le = &lab.Error{Code: lab.CodeFailed, Msg: err.Error()}
			}
			reply(w, statusOf[le.Code], map[string]string{"error": le.Msg, "code": le.Code})
			return
		}
		reply(w, http.StatusOK, res)
	})))
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
