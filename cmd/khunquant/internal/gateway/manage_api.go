package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/intents"
)

// The management routes the web launcher's Proactive page uses to see and manage
// what the assistant keeps track of and asks permission for: intents, pending
// proposals and mandates.
//
// The gateway holds every store and every piece of in-memory state, so it is the
// one authority: the launcher opens no database and only proxies these routes,
// adding the management token server-side. Every route is guarded exactly like
// /api/control/kill (loopback callers only, the management bearer token, nothing a
// browser could have sent, and a refusal while no token is configured).
//
// Who may do what. An action that raises risk needs the web to be the owner at the
// moment of the call (cfg.WebOwnerActive: an owner is configured, the launcher
// requires its dashboard authentication, web_is_owner is on); otherwise the answer
// is 403 with the missing condition:
//
//	approve a proposal, reject a proposal, confirm a proposed intent, resume a paused intent
//
// An action that only reduces risk needs the launcher's dashboard authentication
// alone, as the kill switch does: pause or cancel an intent, revoke a mandate, tighten
// the autonomy dial, remove a mute. Resume (clear the kill switch) and a loosening of
// the dial need the web owner. All are recorded as "web".

// manageDeps is what the routes act on, for one generation of services. Nothing is
// opened here: each store is the one the running services already use. A part that
// does not exist is nil, and its routes say so.
type manageDeps struct {
	cfg       *config.Config
	disp      *control.Dispatcher
	approvals *approval.Service
	mandates  *dca.Store
	intents   *intent.Store
	compile   func() // asks the intent compiler for a pass; nil without one
	// profile is the owner's profile, where the autonomy dial lives; it is open whenever
	// proactive is, whatever proactive.intents says. dialMu orders the dial's writes.
	profile *intent.Store
	dialMu  sync.Mutex
	// db is the shared proactive database the dashboard reads (dashboard_api.go); now is
	// its clock, nil for the wall clock.
	db  *sql.DB
	now func() time.Time
}

// attachManage gives the routes the services of this generation. Called after the
// services exist; detach withdraws them before the stores close.
func (rt *controlRuntime) attachManage(d assemblyDeps, p *perceptionRuntime) {
	if rt == nil || !d.Cfg.Proactive.Enabled {
		return
	}
	m := &manageDeps{cfg: d.Cfg, disp: rt.disp}
	// Approvals are listed only under the rule attach applies to the dispatcher: an
	// approval service with no engine could record a decision and execute nothing.
	if d.Approval != nil && d.Approval.svc != nil && d.Approval.cancel != nil {
		m.approvals = d.Approval.svc
	}
	if d.Approval != nil {
		m.mandates = d.Approval.mandateStore
	}
	if db, err := proactiveDB(d); err == nil {
		m.db = db
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		st, err := intent.NewStore(ctx, db)
		cancel()
		if err != nil {
			logger.WarnCF("gateway", "Intent and autonomy management are unavailable: the store could not be opened", map[string]any{"error": err.Error()})
		} else {
			m.profile = st
			if d.Cfg.Proactive.Effective().Intents {
				m.intents = st
			}
		}
	}
	if p != nil && p.intentComp != nil {
		m.compile = p.intentComp.Request
	}
	rt.manage.Store(m)
}

func (rt *controlRuntime) managed() *manageDeps {
	if rt == nil {
		return nil
	}
	return rt.manage.Load()
}

// registerManageAPI registers the management routes. The services are read when a
// request arrives, because the routes are registered before they are built.
func registerManageAPI(mux handleMux, rt *controlRuntime, token string) {
	guard := func(h http.HandlerFunc) http.Handler {
		return loopbackOnly(bearerTokenMiddleware(token, noBrowser(requireManagementToken(token, h))))
	}
	mux.Handle("GET /api/proactive/intents", guard(rt.handleIntentsList))
	mux.Handle("POST /api/proactive/intents/{id}/{action}", guard(rt.handleIntentAction))
	mux.Handle("GET /api/proactive/proposals", guard(rt.handleProposalsList))
	mux.Handle("POST /api/proactive/proposals/{code}/{action}", guard(rt.handleProposalAction))
	mux.Handle("GET /api/proactive/mandates", guard(rt.handleMandatesList))
	mux.Handle("POST /api/proactive/mandates/{id}/revoke", guard(rt.handleMandateRevoke))
	mux.Handle("GET /api/proactive/autonomy", guard(rt.handleAutonomyGet))
	mux.Handle("PUT /api/proactive/autonomy", guard(rt.handleAutonomyPut))
	mux.Handle("POST /api/proactive/resume", guard(rt.handleResume))
	mux.Handle("GET /api/proactive/mutes", guard(rt.handleMutesList))
	mux.Handle("DELETE /api/proactive/mutes/{id}", guard(rt.handleMuteDelete))
	// The dashboard's reads (dashboard_api.go).
	mux.Handle("GET /api/proactive/interventions", guard(rt.handleInterventions))
	mux.Handle("GET /api/proactive/metrics", guard(rt.handleMetrics))
	mux.Handle("GET /api/proactive/changes", guard(rt.handleChanges))
	mux.Handle("GET /api/proactive/state", guard(rt.handleState))
}

// requireManagementToken refuses every request while no token is configured: the
// bearer middleware lets everything through then.
func requireManagementToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeManage(w, http.StatusServiceUnavailable, map[string]string{"error": "no management token is configured"})
			return
		}
		next(w, r)
	}
}

func writeManage(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func manageError(w http.ResponseWriter, status int, msg string) {
	writeManage(w, status, map[string]string{"error": msg})
}

// ownerInfo is the web-owner fields every read carries and a refusal repeats.
func ownerInfo(cfg *config.Config) map[string]any {
	active, missing := cfg.WebOwnerStatus()
	if !active && missing == "" {
		missing = "proactive.owner.web_is_owner is off"
	}
	return map[string]any{"web_owner_active": active, "web_owner_missing": missing}
}

// refuseNotWebOwner answers 403 for a risk-increasing action while the web is not the owner.
func refuseNotWebOwner(w http.ResponseWriter, cfg *config.Config) {
	body := ownerInfo(cfg)
	body["error"] = "the web launcher is not the owner right now, so this cannot be done from the web: " + body["web_owner_missing"].(string)
	writeManage(w, http.StatusForbidden, body)
}

// unavailableRead is the answer of a read whose service does not exist: one clear
// empty state, not an error.
func unavailableRead(w http.ResponseWriter, reason string) {
	writeManage(w, http.StatusOK, map[string]any{"available": false, "reason": reason})
}

const (
	reasonOff        = "Proactive is off or the gateway is still starting."
	reasonNoIntents  = "Intents are off (proactive.intents)."
	reasonNoApproval = "Approvals are off, or the workflow engine is not running."
	reasonNoMandates = "Mandates are off (proactive.mandates)."
)

// ids and codes are checked before they reach a store.
var manageIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)

// webProvenance is who the store records for a change made from the launcher: the
// owner, by a user-authored act, as "web".
var webProvenance = intent.Provenance{SenderID: identity.WebOwner, Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser}

// ---- intents ----------------------------------------------------------------

type intentView struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Title     string     `json:"title"` // owner- or model-written: plain text
	Status    string     `json:"status"`
	Source    string     `json:"source"`
	TurnKind  string     `json:"turn_kind"`
	ViaWeb    bool       `json:"via_web"`
	Triggers  int        `json:"triggers"`
	FiresUsed int        `json:"fires_used"`
	Budget    int        `json:"fire_budget"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func viewIntent(it intent.Intent) intentView {
	v := intentView{
		ID: it.ID, Type: string(it.Type), Title: it.Title, Status: string(it.Status), Source: string(it.Source),
		TurnKind: string(it.Provenance.TurnKind), ViaWeb: it.Provenance.SenderID == identity.WebOwner,
		FiresUsed: it.FiresUsed, Budget: it.FireBudget, CreatedAt: it.CreatedAt, ExpiresAt: it.ExpiresAt,
	}
	if intents.Compiles(it) { // a trigger is compiled from it while it is active
		v.Triggers = 1
	}
	return v
}

func (rt *controlRuntime) handleIntentsList(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	switch {
	case m == nil:
		unavailableRead(w, reasonOff)
		return
	case m.intents == nil:
		unavailableRead(w, reasonNoIntents)
		return
	}
	list, err := m.intents.ListIntents(r.Context(), intent.IntentFilter{
		Statuses:    []intent.Status{intent.StatusProposed, intent.StatusActive, intent.StatusPaused},
		NewestFirst: true, Limit: 100,
	})
	if err != nil {
		logger.WarnCF("gateway", "Listing intents for the launcher failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the intents could not be read")
		return
	}
	out := make([]intentView, 0, len(list))
	for _, it := range list {
		out = append(out, viewIntent(it))
	}
	body := ownerInfo(m.cfg)
	body["available"], body["server_time"], body["intents"] = true, time.Now().UTC(), out
	writeManage(w, http.StatusOK, body)
}

func (rt *controlRuntime) handleIntentAction(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.intents == nil {
		manageError(w, http.StatusServiceUnavailable, reasonNoIntents)
		return
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	if !manageIDPattern.MatchString(id) {
		manageError(w, http.StatusBadRequest, "not an intent id")
		return
	}
	var want, from intent.Status
	var raisesRisk bool
	switch action {
	case "pause":
		want, from = intent.StatusPaused, intent.StatusActive
	case "cancel":
		want = intent.StatusCancelled
	case "resume":
		want, from, raisesRisk = intent.StatusActive, intent.StatusPaused, true
	case "confirm":
		want, from, raisesRisk = intent.StatusActive, intent.StatusProposed, true
	default:
		manageError(w, http.StatusNotFound, "unknown action")
		return
	}
	if raisesRisk && !m.cfg.WebOwnerActive() {
		refuseNotWebOwner(w, m.cfg)
		return
	}
	cur, err := m.intents.GetIntent(r.Context(), id)
	if errors.Is(err, intent.ErrNotFound) {
		manageError(w, http.StatusNotFound, "there is no such intent")
		return
	} else if err != nil {
		manageError(w, http.StatusInternalServerError, "the intent could not be read")
		return
	}
	// Only the move the action names: "resume" must not confirm a suggestion.
	if from != "" && cur.Status != from {
		manageError(w, http.StatusConflict, "this intent is "+string(cur.Status)+", so the "+action+" action does not apply")
		return
	}
	if action == "confirm" { // a suggestion becomes active only if it can be carried out
		if err := intents.Validate(r.Context(), m.intents, cur, time.Now()); err != nil {
			manageError(w, http.StatusUnprocessableEntity, "not confirmed: "+err.Error())
			return
		}
	}
	if err := m.intents.Transition(r.Context(), id, want, webProvenance, "web: "+action); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, intent.ErrBadTransition) || errors.Is(err, intent.ErrTooManyOpen) {
			status = http.StatusConflict
		}
		manageError(w, status, err.Error())
		return
	}
	if m.compile != nil {
		m.compile() // a paused watch must stop, and a confirmed one start, now
	}
	done, err := m.intents.GetIntent(r.Context(), id)
	if err != nil {
		writeManage(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeManage(w, http.StatusOK, map[string]any{"ok": true, "intent": viewIntent(done)})
}

// ---- proposals --------------------------------------------------------------

type proposalView struct {
	Code      string    `json:"code"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary"` // rendered by code
	Reducing  bool      `json:"reducing"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// ExpiresInS is the seconds left by the gateway's own clock.
	ExpiresInS int64 `json:"expires_in_s"`
	// Quote is model-written ("suggested by the assistant") and must be shown as text.
	Quote     string `json:"quote,omitempty"`
	QuoteKind string `json:"quote_kind,omitempty"`
}

func (rt *controlRuntime) handleProposalsList(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	switch {
	case m == nil:
		unavailableRead(w, reasonOff)
		return
	case m.approvals == nil:
		unavailableRead(w, reasonNoApproval)
		return
	}
	if s := r.URL.Query().Get("status"); s != "" && s != string(approval.StatusPending) {
		manageError(w, http.StatusBadRequest, "only pending proposals are listed")
		return
	}
	list, now, err := m.approvals.ListPending(r.Context())
	if err != nil {
		logger.WarnCF("gateway", "Listing proposals for the launcher failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the proposals could not be read")
		return
	}
	out := make([]proposalView, 0, len(list))
	for _, p := range list {
		out = append(out, proposalView{
			Code: p.Code, Kind: string(p.Kind), Status: string(p.Status), Summary: p.Summary, Reducing: p.Reducing,
			CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, ExpiresInS: int64(p.ExpiresAt.Sub(now).Seconds()),
			Quote: p.Quote, QuoteKind: p.QuoteKind,
		})
	}
	body := ownerInfo(m.disp.Deps().Cfg)
	body["available"], body["server_time"], body["proposals"] = true, now.UTC(), out
	writeManage(w, http.StatusOK, body)
}

func (rt *controlRuntime) handleProposalAction(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil {
		manageError(w, http.StatusServiceUnavailable, reasonOff)
		return
	}
	var approve bool
	switch r.PathValue("action") {
	case "approve":
		approve = true
	case "reject":
	default:
		manageError(w, http.StatusNotFound, "unknown action")
		return
	}
	// Both go through the control path's one decision, which needs the web owner: a
	// proposal nobody rejects simply expires.
	res := m.disp.DecideFromLauncher(r.Context(), r.PathValue("code"), approve)
	status, ok := map[control.LauncherResult]int{
		control.LauncherDecided: http.StatusOK, control.LauncherNotOwner: http.StatusForbidden,
		control.LauncherHeld: http.StatusConflict, control.LauncherSettled: http.StatusConflict,
		control.LauncherNotFound: http.StatusNotFound, control.LauncherUnavailable: http.StatusServiceUnavailable,
		control.LauncherBadCode: http.StatusBadRequest,
	}[res.Result]
	if !ok {
		status = http.StatusInternalServerError
	}
	body := ownerInfo(m.disp.Deps().Cfg)
	body["result"], body["text"], body["status"] = string(res.Result), res.Text, res.Status
	if res.Result != control.LauncherDecided {
		body["error"] = res.Text
	}
	writeManage(w, status, body)
}

// ---- mandates ---------------------------------------------------------------

type mandateView struct {
	ID              string     `json:"id"`
	Symbol          string     `json:"symbol"`
	Side            string     `json:"side"`
	Provider        string     `json:"provider"`
	Account         string     `json:"account"`
	Status          string     `json:"status"`
	Period          string     `json:"period"`
	MaxPerPeriodUSD float64    `json:"max_per_period_usd"`
	MaxPerRunUSD    float64    `json:"max_per_run_usd"`
	UsedUSD         float64    `json:"used_usd"`
	UsageKnown      bool       `json:"usage_known"`
	ValidFrom       time.Time  `json:"valid_from"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

func (rt *controlRuntime) handleMandatesList(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	switch {
	case m == nil:
		unavailableRead(w, reasonOff)
		return
	case m.mandates == nil:
		unavailableRead(w, reasonNoMandates)
		return
	}
	list, err := m.mandates.ListMandates(r.Context(), "")
	if err != nil {
		logger.WarnCF("gateway", "Listing mandates for the launcher failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the mandates could not be read")
		return
	}
	if len(list) > 50 {
		list = list[len(list)-50:] // the newest
	}
	now, paper := time.Now(), broker.IsPaperMode(m.cfg)
	out := make([]mandateView, 0, len(list))
	for _, md := range list {
		v := mandateView{
			ID: md.ID, Symbol: md.Symbol, Side: md.Side, Provider: md.Provider, Account: md.Account, Status: md.Status,
			Period: md.Period, MaxPerPeriodUSD: md.MaxPerPeriodUSD, MaxPerRunUSD: md.MaxPerExecutionUSD, ValidFrom: md.ValidFrom,
		}
		if !md.ExpiresAt.IsZero() {
			exp := md.ExpiresAt
			v.ExpiresAt = &exp
		}
		if used, err := m.mandates.PeriodUsage(r.Context(), md, now, paper); err == nil {
			v.UsedUSD, v.UsageKnown = used, true
		}
		out = append(out, v)
	}
	writeManage(w, http.StatusOK, map[string]any{"available": true, "server_time": now.UTC(), "mandates": out})
}

func (rt *controlRuntime) handleMandateRevoke(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.mandates == nil {
		manageError(w, http.StatusServiceUnavailable, reasonNoMandates)
		return
	}
	id := r.PathValue("id")
	if !manageIDPattern.MatchString(id) {
		manageError(w, http.StatusBadRequest, "not a mandate id")
		return
	}
	md, err := m.mandates.GetMandate(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		manageError(w, http.StatusNotFound, "there is no such mandate")
		return
	} else if err != nil {
		manageError(w, http.StatusInternalServerError, "the mandate could not be read")
		return
	}
	if md.Status != "active" {
		manageError(w, http.StatusConflict, "this mandate is "+md.Status)
		return
	}
	// The owner's revoke path, as update_dca_plan takes it: revoke, then drop a pending
	// request. Risk-reducing, so the dashboard's authentication is enough.
	if _, err := m.mandates.RevokePlanMandate(r.Context(), md.PlanID, "revoked by the owner from the web launcher (web)"); err != nil {
		logger.WarnCF("gateway", "Revoking a mandate for the launcher failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the mandate could not be revoked")
		return
	}
	_ = m.mandates.ClearMandatePending(r.Context(), md.PlanID)
	writeManage(w, http.StatusOK, map[string]any{"ok": true})
}
