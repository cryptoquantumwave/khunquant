package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
)

// controlRuntime is the control path of one generation of services: the kill flag
// and the dispatcher every channel offers a message to before the agent sees it.
//
// It starts no goroutine. The dispatcher is built and installed on the channel
// manager BEFORE the channels start, with what exists by then (the configuration,
// the event bus and the kill flag), so that /whoami and the refusals of the
// owner-only commands work from the first message; attach adds the rest as it is
// built, and detach takes it away again before the services it uses stop.
type controlRuntime struct {
	// state is the persisted kill flag; nil when proactive is off or the shared
	// database could not be opened (then /stop says so, and nothing is killable).
	state *control.State
	disp  *control.Dispatcher
}

// buildControl builds the kill flag (when proactive is on) and the dispatcher, and
// installs the dispatcher on mgr. It never fails: a state that cannot be opened is
// logged, and the dispatcher then answers /whoami and refuses the rest.
//
// The router exists in every configuration, because /whoami is how an owner is
// configured in the first place.
func buildControl(d assemblyDeps, mgr *channels.Manager) *controlRuntime {
	rt := &controlRuntime{}
	deps := control.Deps{Cfg: d.Cfg, Events: d.Shared.eventBus()}

	if d.Cfg.Proactive.Enabled {
		if db, err := proactiveDB(d); err != nil {
			logger.ErrorCF("gateway", "The kill switch is unavailable: the shared proactive database cannot be opened",
				map[string]any{"error": err.Error()})
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			st, err := control.NewState(ctx, db)
			cancel()
			if err != nil {
				logger.ErrorCF("gateway", "The kill switch is unavailable", map[string]any{"error": err.Error()})
			} else {
				rt.state = st
				deps.Kill = st // only ever a non-nil *State: a typed nil in the interface would not read as nil
			}
		}
	}

	rt.disp = control.New(deps)
	if mgr != nil {
		mgr.SetControlRouter(rt.disp)
	}
	return rt
}

// killer is the kill flag as the approval service wants it, or nil. Safe on nil.
func (rt *controlRuntime) killer() approval.Killer {
	if rt == nil || rt.state == nil {
		return nil
	}
	return rt.state
}

// killSwitch is the kill flag as the gateway route wants it, or nil. Safe on nil.
func (rt *controlRuntime) killSwitch() control.KillSwitch {
	if rt == nil || rt.state == nil {
		return nil
	}
	return rt.state
}

// attach gives the dispatcher the services it acts on. Each one is assigned only
// when it exists: a nil pointer in an interface would not read as nil, and the
// handlers' "not available" answers would never fire.
func (rt *controlRuntime) attach(d assemblyDeps, ns *notify.Service, at *attention.Outbox, ap *approvalRuntime) {
	if rt == nil {
		return
	}
	deps := rt.disp.Deps()
	if ns != nil {
		deps.Sent = ns
	}
	if at != nil {
		deps.Attention = at
	}
	// cancel is set only by a successful attach: without the workflow engine nothing
	// executes, and an approval would only be recorded.
	if ap != nil && ap.svc != nil && ap.cancel != nil {
		deps.Approvals = ap.svc
	}
	deps.Cfg = d.Cfg
	rt.disp.SetDeps(deps)
}

// detach takes the approval, notify and attention services away from the
// dispatcher. stopServices calls it before it stops them, so that an /approve
// arriving during shutdown is answered "not available" rather than approving an
// order nothing is left to execute. The dispatcher stays installed on the channels
// until they have stopped: removing it earlier would hand a late /approve or
// /stop to the agent.
func (rt *controlRuntime) detach() {
	if rt == nil {
		return
	}
	deps := rt.disp.Deps()
	deps.Approvals, deps.Sent, deps.Attention = nil, nil, nil
	rt.disp.SetDeps(deps)
}

// registerControlAPI registers POST and DELETE /api/control/kill: engage and clear
// the kill switch from outside a chat (the web launcher proxies them in a later
// change). Like the other management routes they accept loopback callers carrying
// the management token and refuse requests a browser could have made. Unlike the
// token middleware, which lets everything through when the configured token is
// empty, they refuse every request while no token is configured.
func registerControlAPI(mux handleMux, kill control.KillSwitch, token string) {
	h := controlKillHandler(kill, token)
	guarded := loopbackOnly(bearerTokenMiddleware(token, noBrowser(h)))
	mux.Handle("POST /api/control/kill", guarded)
	mux.Handle("DELETE /api/control/kill", guarded)
}

func controlKillHandler(kill control.KillSwitch, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if token == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no management token is configured"})
			return
		}
		if kill == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the kill switch is not available (proactive is off)"})
			return
		}
		var changed bool
		var err error
		switch r.Method {
		case http.MethodPost:
			changed, err = kill.Engage(r.Context(), "api", "gateway route")
		case http.MethodDelete:
			changed, err = kill.Clear(r.Context(), "api")
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err != nil {
			logger.ErrorCF("gateway", "Kill switch route failed", map[string]any{"error": err.Error()})
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the kill switch could not be changed"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"killed": kill.Killed(), "changed": changed})
	})
}
