package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
)

// The owner's autonomy dial, resume and the mutes list, beside the other management
// routes (manage_api.go) and guarded exactly like them.
//
// The dial can only TIGHTEN what the assistant does on its own, per tier T1..T3:
// default (today), ask, notify, off. A change that only tightens needs the dashboard's
// authentication alone; a change that loosens any tier toward default needs the web to
// be the owner (cfg.WebOwnerActive), else 403 with the missing condition. The write is
// a new profile version authored "web" (intent.Store.SetAutonomy, the only writer of the
// dial), read by the policy engine and by dca.execute on their next decision.

// dialStore is the profile store the routes read and write. It is the one the intents
// routes use when they are on, and is opened whenever proactive is.
func (m *manageDeps) dialStore() *intent.Store {
	if m == nil {
		return nil
	}
	return m.profile
}

// autonomyReader reads the owner's setting for a tier from the profile on every call
// (no cache, so a change applies to the very next decision). When the store cannot be
// opened the reader always fails, which both consumers read as "off".
func autonomyReader(d assemblyDeps) func(ctx context.Context, tier string) (string, error) {
	var store *intent.Store
	var openErr error = errors.New("the proactive database is unavailable")
	if db, err := proactiveDB(d); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		store, openErr = intent.NewStore(ctx, db)
		cancel()
	}
	return func(ctx context.Context, tier string) (string, error) {
		if store == nil {
			return "", openErr
		}
		p, err := store.Profile(ctx)
		if err != nil {
			return "", err
		}
		return p.AutonomyOf(tier), nil
	}
}

var dialTierInfo = []struct {
	tier  policy.Tier
	name  string
	about string
}{
	{policy.T1Protective, "protective", "Orders that only reduce exposure (close, cancel, reduce-only)."},
	{policy.T2Mandated, "mandated", "Buys of a DCA plan inside a mandate you signed."},
	{policy.T3Discretionary, "discretionary", "Any other consequential action, such as a new order."},
}

var modeWords = map[policy.Mode]string{
	policy.ModeAct:        "acts on its own",
	policy.ModeAsk:        "asks you first",
	policy.ModeNotifyOnly: "only tells you; does nothing",
	policy.ModeDeny:       "refused",
}

var dialColumns = []struct {
	col  policy.DialColumn
	name string
}{
	{policy.ColumnOwner, "your own message"},
	{policy.ColumnTrusted, "an autonomous turn on exchange data"},
	{policy.ColumnOther, "an autonomous turn on other text, or another person"},
}

type dialColumnView struct {
	Column      string `json:"column"`
	Label       string `json:"label"`
	Default     string `json:"default"`
	DefaultText string `json:"default_text"`
	Effective   string `json:"effective"`
	Text        string `json:"effective_text"`
}

type dialTierView struct {
	Tier    string           `json:"tier"`
	Name    string           `json:"name"`
	About   string           `json:"about"`
	Setting string           `json:"setting"`
	Columns []dialColumnView `json:"columns"`
}

// policyModeNow is the engine's armed mode: off, shadow, or live (the configured live
// can be lowered), not the configured value.
func policyModeNow() string {
	if rt := policyLive.Load(); rt != nil {
		return rt.mode
	}
	return "off"
}

func (m *manageDeps) autonomyBody(p intent.Profile) map[string]any {
	tiers := make([]dialTierView, 0, len(dialTierInfo))
	for _, ti := range dialTierInfo {
		setting := p.AutonomyOf(ti.tier.String())
		owner, _ := policy.DialMode(setting)
		v := dialTierView{Tier: ti.tier.String(), Name: ti.name, About: ti.about, Setting: setting}
		for _, c := range dialColumns {
			def := policy.DefaultMode(ti.tier, c.col, ti.tier == policy.T2Mandated)
			eff := def
			if c.col != policy.ColumnOwner { // your own message is never dialled
				eff = policy.Tighten(def, owner)
			}
			v.Columns = append(v.Columns, dialColumnView{string(c.col), c.name, string(def), modeWords[def], string(eff), modeWords[eff]})
		}
		tiers = append(tiers, v)
	}
	body := ownerInfo(m.cfg)
	body["available"], body["policy_mode"], body["tiers"] = true, policyModeNow(), tiers
	body["settings"] = []string{intent.AutonomyDefault, intent.AutonomyAsk, intent.AutonomyNotify, intent.AutonomyOff}
	body["note"] = "The dial only tightens. In the policy's live mode it applies to tool calls; in shadow it is only recorded; " +
		"with the policy off it does nothing. Mandated buys (T2) obey it always."
	return body
}

func (rt *controlRuntime) handleAutonomyGet(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.dialStore() == nil {
		unavailableRead(w, reasonOff)
		return
	}
	p, err := m.dialStore().Profile(r.Context())
	if err != nil {
		manageError(w, http.StatusInternalServerError, "the autonomy setting could not be read")
		return
	}
	writeManage(w, http.StatusOK, m.autonomyBody(p))
}

const autonomyBodyLimit = 4 << 10

func (rt *controlRuntime) handleAutonomyPut(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.dialStore() == nil {
		manageError(w, http.StatusServiceUnavailable, reasonOff)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, autonomyBodyLimit+1))
	if err != nil || len(raw) > autonomyBodyLimit {
		manageError(w, http.StatusBadRequest, "the request body is too large or unreadable")
		return
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil || len(want) == 0 {
		manageError(w, http.StatusBadRequest, `give a JSON object of tier settings such as {"T3": "ask"}`)
		return
	}
	for tier, v := range want {
		if tier != "T1" && tier != "T2" && tier != "T3" {
			manageError(w, http.StatusBadRequest, "unknown tier: only T1, T2 and T3 can be set")
			return
		}
		if _, ok := intent.AutonomyRank(v); !ok {
			manageError(w, http.StatusBadRequest, "unknown setting for "+tier+": use default, ask, notify or off (the dial cannot do more than today)")
			return
		}
	}

	// Read, compare and write under one lock: a loosening is judged against what is stored now.
	m.dialMu.Lock()
	defer m.dialMu.Unlock()
	cur, err := m.dialStore().Profile(r.Context())
	if err != nil {
		manageError(w, http.StatusInternalServerError, "the autonomy setting could not be read")
		return
	}
	next := map[string]string{}
	var changed, loosens bool
	for _, tier := range intent.AutonomyTiers {
		before := cur.AutonomyOf(tier)
		after := before
		if v, ok := want[tier]; ok {
			after = v
		}
		next[tier] = after
		if after != before {
			changed = true
			rb, _ := intent.AutonomyRank(before)
			ra, _ := intent.AutonomyRank(after)
			loosens = loosens || ra < rb
		}
	}
	if changed {
		if loosens && !m.cfg.WebOwnerActive() {
			refuseNotWebOwner(w, m.cfg)
			return
		}
		if _, err := m.dialStore().SetAutonomy(r.Context(), next, "web"); err != nil {
			logger.WarnCF("gateway", "Saving the autonomy setting failed", map[string]any{"error": err.Error()})
			manageError(w, http.StatusInternalServerError, "the autonomy setting could not be saved")
			return
		}
		logger.InfoCF("gateway", "The owner changed the autonomy dial from the web launcher", map[string]any{"loosened": loosens})
		cur.Autonomy, _ = intent.NormalizeAutonomy(next)
	}
	writeManage(w, http.StatusOK, m.autonomyBody(cur))
}

// handleResume clears the kill switch. Resuming lets the assistant act again, so with
// an owner configured it needs the web to be the owner (the gateway alone can tell);
// with none configured nothing could ever clear the flag, so it is open (mayClearKill).
func (rt *controlRuntime) handleResume(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.disp.Deps().Kill == nil {
		manageError(w, http.StatusServiceUnavailable, "the kill switch is not available (proactive is off)")
		return
	}
	if !mayClearKill(m.cfg) {
		refuseNotWebOwner(w, m.cfg)
		return
	}
	kill := m.disp.Deps().Kill
	changed, err := kill.Clear(r.Context(), "web")
	if err != nil {
		logger.ErrorCF("gateway", "Resuming from the launcher failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the kill switch could not be cleared")
		return
	}
	writeManage(w, http.StatusOK, map[string]any{"killed": kill.Killed(), "changed": changed})
}

// ---- mutes ------------------------------------------------------------------

type muteView struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // "prefix" (a trigger kind or id) or "exact" (one alert)
	Key     string    `json:"key"`  // plain text: shown, never interpreted
	Reason  string    `json:"reason"`
	Until   time.Time `json:"until"`
	Created time.Time `json:"created"`
}

type muteSource interface {
	ListMutes(ctx context.Context) ([]attention.MuteInfo, error)
	Unmute(ctx context.Context, keyPrefix string) error
}

var muteIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,400}$`)

func (m *manageDeps) muteSource() muteSource {
	if m == nil || m.disp == nil {
		return nil
	}
	s, _ := m.disp.Deps().Attention.(muteSource)
	return s
}

func (rt *controlRuntime) handleMutesList(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.muteSource() == nil {
		unavailableRead(w, "Attention is off, so nothing is muted.")
		return
	}
	list, err := m.muteSource().ListMutes(r.Context())
	if err != nil {
		logger.WarnCF("gateway", "Listing mutes for the launcher failed", map[string]any{"error": err.Error()})
		manageError(w, http.StatusInternalServerError, "the mutes could not be read")
		return
	}
	out := make([]muteView, 0, len(list))
	for _, mi := range list {
		kind, key := "prefix", mi.Key
		if mi.Exact {
			kind, key = "exact", strings.TrimPrefix(mi.Key, "=")
		}
		out = append(out, muteView{base64.RawURLEncoding.EncodeToString([]byte(mi.Key)), kind, key, mi.Reason, mi.Until, mi.Created})
	}
	writeManage(w, http.StatusOK, map[string]any{"available": true, "server_time": time.Now().UTC(), "mutes": out})
}

// handleMuteDelete unmutes. It only lets more messages through, so the dashboard's
// authentication is enough. The id is the opaque id of a listed mute.
func (rt *controlRuntime) handleMuteDelete(w http.ResponseWriter, r *http.Request) {
	m := rt.managed()
	if m == nil || m.muteSource() == nil {
		manageError(w, http.StatusServiceUnavailable, "Attention is off, so nothing is muted.")
		return
	}
	id := r.PathValue("id")
	if !muteIDPattern.MatchString(id) {
		manageError(w, http.StatusBadRequest, "not a mute id")
		return
	}
	key, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		manageError(w, http.StatusBadRequest, "not a mute id")
		return
	}
	list, err := m.muteSource().ListMutes(r.Context())
	if err != nil {
		manageError(w, http.StatusInternalServerError, "the mutes could not be read")
		return
	}
	found := false
	for _, mi := range list {
		found = found || mi.Key == string(key)
	}
	if !found {
		manageError(w, http.StatusNotFound, "there is no such mute")
		return
	}
	if err := m.muteSource().Unmute(r.Context(), string(key)); err != nil {
		manageError(w, http.StatusInternalServerError, "the mute could not be removed")
		return
	}
	writeManage(w, http.StatusOK, map[string]any{"ok": true})
}
