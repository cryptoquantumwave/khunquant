package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

type cronUpdateRequest struct {
	Name     *string            `json:"name,omitempty"`
	Message  *string            `json:"message,omitempty"`
	Enabled  *bool              `json:"enabled,omitempty"`
	Deliver  *bool              `json:"deliver,omitempty"`
	Schedule *cron.CronSchedule `json:"schedule,omitempty"`
	Channel  *string            `json:"channel,omitempty"`
	To       *string            `json:"to,omitempty"`
}

// loopbackOnly rejects requests that do not originate from the loopback interface.
// The gateway HTTP server may bind to 0.0.0.0 (for webhook channels), so we restrict
// the internal management API to localhost explicitly.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.RemoteAddr
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerCronAPI registers live cron management routes on the gateway HTTP mux.
// These routes mutate the in-memory CronService directly, avoiding the stale-file race.
func registerCronAPI(mux interface {
	Handle(pattern string, handler http.Handler)
}, cs *cron.CronService) {
	mux.Handle("GET /api/cron/jobs", loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobs := cs.ListJobs(true)
		if jobs == nil {
			jobs = []cron.CronJob{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
	})))

	mux.Handle("PATCH /api/cron/jobs/{id}", loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}

		var req cronUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}

		jobs := cs.ListJobs(true)
		var target *cron.CronJob
		for i := range jobs {
			if jobs[i].ID == id {
				target = &jobs[i]
				break
			}
		}
		if target == nil {
			http.Error(w, fmt.Sprintf("job %q not found", id), http.StatusNotFound)
			return
		}

		if req.Name != nil {
			target.Name = *req.Name
		}
		if req.Message != nil {
			target.Payload.Message = *req.Message
		}
		if req.Deliver != nil {
			target.Payload.Deliver = *req.Deliver
		}
		if req.Channel != nil {
			target.Payload.Channel = *req.Channel
		}
		if req.To != nil {
			target.Payload.To = *req.To
		}
		if req.Schedule != nil {
			target.Schedule = *req.Schedule
		}
		if req.Enabled != nil {
			if *req.Enabled != target.Enabled {
				cs.EnableJob(id, *req.Enabled)
				// EnableJob already persists; only call UpdateJob if other fields changed.
				if req.Name != nil || req.Message != nil {
					target.Enabled = *req.Enabled
					target.UpdatedAtMS = time.Now().UnixMilli()
					if err := cs.UpdateJob(target); err != nil {
						http.Error(w, fmt.Sprintf("failed to update job: %v", err), http.StatusInternalServerError)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				return
			}
		}

		target.UpdatedAtMS = time.Now().UnixMilli()
		if err := cs.UpdateJob(target); err != nil {
			http.Error(w, fmt.Sprintf("failed to update job: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})))

	mux.Handle("POST /api/cron/jobs/{id}/run", loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if !cs.RunJobNow(id) {
			http.Error(w, fmt.Sprintf("job %q not found", id), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})))

	mux.Handle("DELETE /api/cron/jobs/{id}", loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}

		if !cs.RemoveJob(id) {
			http.Error(w, fmt.Sprintf("job %q not found", id), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})))
}
