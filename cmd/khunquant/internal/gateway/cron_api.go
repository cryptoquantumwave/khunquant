package gateway

import (
	"encoding/json"
	"fmt"
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
}

// registerCronAPI registers live cron management routes on the gateway HTTP mux.
// These routes mutate the in-memory CronService directly, avoiding the stale-file race.
func registerCronAPI(mux interface {
	Handle(pattern string, handler http.Handler)
}, cs *cron.CronService) {
	mux.Handle("GET /api/cron/jobs", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobs := cs.ListJobs(true)
		if jobs == nil {
			jobs = []cron.CronJob{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
	}))

	mux.Handle("PATCH /api/cron/jobs/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	mux.Handle("POST /api/cron/jobs/{id}/run", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	mux.Handle("DELETE /api/cron/jobs/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
}
