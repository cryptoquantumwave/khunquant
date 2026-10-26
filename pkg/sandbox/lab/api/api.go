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

// Register mounts the lab route on mux. guard wraps the handler: the caller supplies
// the loopback and instance-token checks, so this package knows no secret. A call
// runs under lab.CallTimeout and a body is capped at 1 MiB.
func Register(mux interface{ Handle(string, http.Handler) }, l *lab.Lab, guard func(http.Handler) http.Handler) {
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
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
