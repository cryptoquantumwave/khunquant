package gateway

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/dashboard"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// The read routes behind the launcher's Proactive dashboard (pkg/dashboard), guarded
// exactly like the other management routes (manage_api.go): loopback callers, the
// management bearer token, nothing a browser could have sent. They only READ, and what
// they return is a projection (see pkg/dashboard); the launcher proxies them and opens no
// database itself. The live stream is not here: the gateway has no SSE, and the launcher
// polls /api/proactive/changes.
//
//	GET /api/proactive/interventions?from&to&disposition&trigger&cursor&limit
//	GET /api/proactive/metrics?days=30
//	GET /api/proactive/changes?cursor=&limit=
//	GET /api/proactive/state

const reasonNoDB = "The proactive database is unavailable."

var (
	dispositionParam = regexp.MustCompile(`^[a-z_]{1,32}$`)
	triggerParam     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	cursorParam      = regexp.MustCompile(`^[0-9.]{1,80}$`)
)

// reader is the dashboard's view of the shared database, or nil without one. The owner's
// zone and the harm source are read when a request arrives: the feedback service is built
// after the routes' services, and exists only while learning is on.
func (m *manageDeps) reader() *dashboard.Reader {
	if m == nil || m.db == nil {
		return nil
	}
	r := &dashboard.Reader{DB: m.db, Now: m.now}
	if m.profile != nil {
		r.Zone = ownerZone(m.profile)
	}
	if h, ok := m.disp.Deps().Feedback.(dashboard.HarmSource); ok {
		r.Harm = h
	}
	return r
}

// dashboardRead answers a request with fn's body, or the one empty state of a gateway
// that has no database yet. A bad request is 400; any other failure is logged and answered
// without its detail.
func (rt *controlRuntime) dashboardRead(w http.ResponseWriter, fn func(r *dashboard.Reader) (map[string]any, error)) {
	m := rt.managed()
	if m == nil {
		unavailableRead(w, reasonOff)
		return
	}
	rd := m.reader()
	if rd == nil {
		unavailableRead(w, reasonNoDB)
		return
	}
	body, err := fn(rd)
	switch {
	case errors.Is(err, dashboard.ErrBadRequest):
		manageError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		logger.WarnCF("gateway", "A dashboard read failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the dashboard could not be read")
		return
	}
	body["available"], body["server_time"] = true, m.clock().UTC()
	writeManage(w, http.StatusOK, body)
}

func parseTimeParam(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("from and to are RFC 3339 times")
	}
	return t, nil
}

func (rt *controlRuntime) handleInterventions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rt.dashboardRead(w, func(rd *dashboard.Reader) (map[string]any, error) {
		f := dashboard.Filter{Disposition: q.Get("disposition"), Trigger: q.Get("trigger"), Cursor: q.Get("cursor")}
		var err error
		if f.From, err = parseTimeParam(q.Get("from")); err != nil {
			return nil, errors.Join(dashboard.ErrBadRequest, err)
		}
		if f.To, err = parseTimeParam(q.Get("to")); err != nil {
			return nil, errors.Join(dashboard.ErrBadRequest, err)
		}
		if (f.Disposition != "" && !dispositionParam.MatchString(f.Disposition)) ||
			(f.Trigger != "" && !triggerParam.MatchString(f.Trigger)) ||
			(f.Cursor != "" && !cursorParam.MatchString(f.Cursor)) {
			return nil, errors.Join(dashboard.ErrBadRequest, errors.New("a filter is malformed"))
		}
		if f.Limit, err = intParam(q.Get("limit"), 0); err != nil {
			return nil, errors.Join(dashboard.ErrBadRequest, err)
		}
		items, next, err := rd.Interventions(r.Context(), f)
		return map[string]any{"items": items, "next_cursor": next}, err
	})
}

func (rt *controlRuntime) handleMetrics(w http.ResponseWriter, r *http.Request) {
	rt.dashboardRead(w, func(rd *dashboard.Reader) (map[string]any, error) {
		days, err := intParam(r.URL.Query().Get("days"), 30)
		if err != nil {
			return nil, errors.Join(dashboard.ErrBadRequest, err)
		}
		m, err := rd.Metrics(r.Context(), days)
		return map[string]any{"metrics": m}, err
	})
}

func (rt *controlRuntime) handleChanges(w http.ResponseWriter, r *http.Request) {
	rt.dashboardRead(w, func(rd *dashboard.Reader) (map[string]any, error) {
		q := r.URL.Query()
		if c := q.Get("cursor"); c != "" && !cursorParam.MatchString(c) {
			return nil, errors.Join(dashboard.ErrBadRequest, errors.New("not a cursor"))
		}
		limit, err := intParam(q.Get("limit"), 0)
		if err != nil {
			return nil, errors.Join(dashboard.ErrBadRequest, err)
		}
		items, next, err := rd.Changes(r.Context(), q.Get("cursor"), limit)
		return map[string]any{"items": items, "cursor": next}, err
	})
}

// handleState is the small read the Stop bar polls: whether the kill switch is engaged,
// and whether the web may resume it. It reads the gateway's own state, not the database.
func (rt *controlRuntime) handleState(w http.ResponseWriter, _ *http.Request) {
	m := rt.managed()
	if m == nil || m.disp.Deps().Kill == nil {
		unavailableRead(w, "The kill switch is not available (proactive is off).")
		return
	}
	body := ownerInfo(m.cfg)
	body["available"], body["killed"], body["server_time"] = true, m.disp.Deps().Kill.Killed(), m.clock().UTC()
	writeManage(w, http.StatusOK, body)
}

func intParam(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("a number is malformed")
	}
	return n, nil
}
