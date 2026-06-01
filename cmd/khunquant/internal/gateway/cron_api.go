package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

var interfaceAddrs = net.InterfaceAddrs

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
		if ip == nil || !isLocalManagementIP(ip) {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalManagementIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		switch v := addr.(type) {
		case *net.IPNet:
			if v.IP.Equal(ip) {
				return true
			}
		case *net.IPAddr:
			if v.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// registerCronAPI registers live cron management routes on the gateway HTTP mux.
// These routes mutate the in-memory CronService directly, avoiding the stale-file race.
// dnStore is optional (may be nil); when provided, schedule changes on dn:* jobs sync
// the linked plan's monitor_interval automatically.
func registerCronAPI(mux interface {
	Handle(pattern string, handler http.Handler)
}, cs *cron.CronService, dnStore *deltaneutral.Store) {
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

		// When the schedule interval changes on a dn:* job, sync monitor_interval on the
		// linked delta-neutral plan so both views stay in agreement.
		if req.Schedule != nil && dnStore != nil &&
			strings.HasPrefix(target.Name, "dn:") &&
			target.Schedule.Kind == "every" && target.Schedule.EveryMS != nil {
			if interval, ok := deltaneutral.IntervalFromMS(*target.Schedule.EveryMS); ok {
				_ = syncDNPlanInterval(r.Context(), dnStore, target.ID, interval)
			}
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

// syncDNPlanInterval updates the monitor_interval of the delta-neutral plan linked to
// the given cron job ID. Errors are non-fatal (logged by caller).
func syncDNPlanInterval(ctx context.Context, store *deltaneutral.Store, cronJobID, interval string) error {
	return store.SetMonitorIntervalByCronJobID(ctx, cronJobID, interval)
}
