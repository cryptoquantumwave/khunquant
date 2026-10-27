package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
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
	// stop is the whole of /stop, set by attachStop once the services it acts on exist
	// and cleared by detach. The gateway route reads it at request time, because the
	// route is registered before the services are built.
	stop atomic.Pointer[control.Kill]
	// manage is what the launcher's management routes act on (manage_api.go), set by
	// attachManage and withdrawn by detach.
	manage atomic.Pointer[manageDeps]
}

// stopper is the /stop sequence, or nil while none is attached. Safe on nil.
func (rt *controlRuntime) stopper() control.Stopper {
	if rt == nil {
		return nil
	}
	if k := rt.stop.Load(); k != nil {
		return k
	}
	return nil
}

// attachStop gives the dispatcher and the route the whole /stop sequence, over what
// this generation of services has: the agent loop, the workflow engine, the
// mandate store, the approval service and the brokers. A part that does not exist
// is left nil, and its step is skipped. Called after attach, with d carrying the
// approval, delta-neutral and DCA sides.
func (rt *controlRuntime) attachStop(d assemblyDeps, j *journalRuntime, ns *notify.Service) {
	if rt == nil || rt.state == nil {
		return
	}
	k := &control.Kill{Flag: rt.state, Desk: control.BrokerDesk{Cfg: d.Cfg}, Clock: d.Shared.clk()}
	if d.AgentLoop != nil {
		k.Turns = d.AgentLoop
	}
	k.Queue = d.Cognition.canceller() // nil unless cognition is on
	if j != nil {
		k.Flows = j.engine
	}
	var (
		svc  *approval.Service
		dcaD *dca.Deps
		dnD  *deltaneutral.Deps
	)
	if d.Approval != nil {
		svc = d.Approval.svc
		if svc != nil {
			k.Proposals = svc
		}
		if d.Approval.mandateStore != nil {
			k.Mandates = d.Approval.mandateStore
		}
	}
	if d.DCAFlow != nil {
		dcaD = d.DCAFlow.deps
	}
	if d.DNFlow != nil {
		dnD = d.DNFlow.deps
	}
	k.Orders = func(ctx context.Context, w journal.Workflow, steps []journal.StepRecord) (broker.FlowOrders, error) {
		switch {
		case w.Kind == approval.WorkflowKind && svc != nil:
			return svc.FlowOrders(ctx, w, steps)
		case w.Kind == dca.KindExecute && dcaD != nil:
			return dcaD.FlowOrders(ctx, w, steps)
		case (w.Kind == deltaneutral.KindOpen || w.Kind == deltaneutral.KindResize) && dnD != nil:
			return dnD.FlowOrders(ctx, w, steps)
		}
		return broker.FlowOrders{}, nil
	}
	if ns != nil {
		k.Followup = func(ctx context.Context, text string) {
			res, err := ns.Deliver(context.WithoutCancel(ctx), notify.Notification{
				Severity: proactive.SeverityWarning, Title: "Stop: update", Body: text,
				DedupKey: "kill-stop:" + strconv.FormatInt(time.Now().UnixNano(), 10),
			})
			if err != nil || !res.Delivered {
				logger.WarnCF("control", "Could not send the second message of /stop", nil)
			}
		}
	}
	rt.stop.Store(k)
	deps := rt.disp.Deps()
	deps.Stop = k
	rt.disp.SetDeps(deps)
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

	// Tools that can act on their own (a scheduled DCA job) read the same flag instance.
	// Armed on every build and reload; nil (proactive off) disarms it.
	if rt.state != nil {
		tools.SetKillFlag(rt.state.Killed)
	} else {
		tools.SetKillFlag(nil)
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

// attachFeedback gives the dispatcher the response recorder, when there is one.
func (rt *controlRuntime) attachFeedback(f *feedbackRuntime) {
	if rt == nil || f == nil || f.svc == nil {
		return
	}
	deps := rt.disp.Deps()
	deps.Feedback = f.svc
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
	deps.Approvals, deps.Sent, deps.Attention, deps.Stop, deps.Feedback = nil, nil, nil, nil, nil
	rt.stop.Store(nil)
	rt.manage.Store(nil)
	rt.disp.SetDeps(deps)
}

// registerControlAPI registers POST and DELETE /api/control/kill: engage and clear
// the kill switch from outside a chat (the web launcher proxies them in a later
// change). Like the other management routes they accept loopback callers carrying
// the management token and refuse requests a browser could have made. Unlike the
// token middleware, which lets everything through when the configured token is
// empty, they refuse every request while no token is configured. Clearing (DELETE)
// additionally needs cfg.WebOwnerActive() when an owner is configured, as POST
// /api/proactive/resume does (see mayClearKill; with no owner configured it is open,
// or nothing could clear a flag Stop set): engaging the brake is never gated, letting
// the assistant act again is.
func registerControlAPI(mux handleMux, kill control.KillSwitch, stop func() control.Stopper, token string, cfg *config.Config) {
	h := controlKillHandler(kill, stop, token, cfg)
	guarded := loopbackOnly(bearerTokenMiddleware(token, noBrowser(h)))
	mux.Handle("POST /api/control/kill", guarded)
	mux.Handle("DELETE /api/control/kill", guarded)
}

func controlKillHandler(kill control.KillSwitch, stop func() control.Stopper, token string, cfg *config.Config) http.Handler {
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
			// The whole of /stop when it is attached, else the flag alone.
			if stop != nil {
				if st := stop(); st != nil {
					sum := st.Stop(r.Context(), "api", "gateway route")
					if !sum.Engaged {
						// The flag did not write: say so with an error status, so the button
						// does not show "stopped" over a warning.
						w.WriteHeader(http.StatusInternalServerError)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"killed": kill.Killed(), "changed": !sum.AlreadyEngaged, "summary": sum, "text": sum.Text()})
					return
				}
			}
			changed, err = kill.Engage(r.Context(), "api", "gateway route")
		case http.MethodDelete:
			if !mayClearKill(cfg) { // nil-safe: no configuration may not clear
				refuseNotWebOwner(w, cfg)
				return
			}
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
