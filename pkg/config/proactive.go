package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Heartbeat modes.
const (
	HeartbeatModeLegacy   = "legacy"
	HeartbeatModeGoverned = "governed"
	HeartbeatModeSweep    = "sweep"
)

// Modes shared by attention, triggers and policy.
const (
	ProactiveModeOff    = "off"
	ProactiveModeShadow = "shadow"
	ProactiveModeLive   = "live"
)

// ProactiveConfig is the "proactive" section: who proactive output may go to
// (the owner) and an off switch for every proactive behaviour.
//
// Components must read Effective(), never these raw fields, so that a mode
// which delivers to the owner cannot run without an owner to deliver to.
//
// Env naming: leaf types that appear once carry full KHUNQUANT_PROACTIVE_...
// names. ModeConfig and ToggleConfig are shared by several sections, so their
// parent field supplies an envPrefix and the leaf tag is relative to it.
type ProactiveConfig struct {
	Enabled bool `json:"enabled" env:"KHUNQUANT_PROACTIVE_ENABLED"`

	Owner      OwnerConfig              `json:"owner"`
	Heartbeat  ProactiveHeartbeatConfig `json:"heartbeat"`
	Attention  AttentionConfig          `json:"attention"`
	Perception PerceptionConfig         `json:"perception"`
	Triggers   ModeConfig               `json:"triggers"   envPrefix:"KHUNQUANT_PROACTIVE_TRIGGERS_"`
	Intents    ToggleConfig             `json:"intents"    envPrefix:"KHUNQUANT_PROACTIVE_INTENTS_"`
	Approvals  ApprovalsConfig          `json:"approvals"`
	Mandates   ToggleConfig             `json:"mandates"   envPrefix:"KHUNQUANT_PROACTIVE_MANDATES_"`
	Cognition  CognitionConfig          `json:"cognition"`
	Policy     ModeConfig               `json:"policy"     envPrefix:"KHUNQUANT_PROACTIVE_POLICY_"`
	News       ToggleConfig             `json:"news"       envPrefix:"KHUNQUANT_PROACTIVE_NEWS_"`
	Reflection ReflectionConfig         `json:"reflection"`
	Learning   LearningConfig           `json:"learning"`
}

// OwnerConfig identifies the one person proactive output may go to, in a
// direct chat. These are personal identifiers, not credentials.
type OwnerConfig struct {
	Channel  string `json:"channel"   env:"KHUNQUANT_PROACTIVE_OWNER_CHANNEL"`   // "telegram"
	ChatID   string `json:"chat_id"   env:"KHUNQUANT_PROACTIVE_OWNER_CHAT_ID"`   // the owner's DM chat id
	SenderID string `json:"sender_id" env:"KHUNQUANT_PROACTIVE_OWNER_SENDER_ID"` // canonical "telegram:123456"
}

// ProactiveHeartbeatConfig selects how the heartbeat behaves.
type ProactiveHeartbeatConfig struct {
	Mode string `json:"mode" env:"KHUNQUANT_PROACTIVE_HEARTBEAT_MODE"` // legacy | governed | sweep
	// Model is the model tier a governed heartbeat's judgement runs on: primary
	// (the normal model, the default) or light (the cheap one, when the agent has
	// one). It has no effect on the legacy heartbeat.
	Model string `json:"model" env:"KHUNQUANT_PROACTIVE_HEARTBEAT_MODEL"` // primary | light
	// Tools names the tools a governed heartbeat's turn is offered, so that each
	// run does not pay for the definitions of tools it will not use. Empty is the
	// built-in portfolio-check set; ["*"] is every tool a proactive turn may call.
	// The list can only narrow what the turn's ceiling allows: naming a tool the
	// ceiling denies (message, create_order) offers nothing. Unknown names are
	// warned about and ignored. It has no effect on the legacy heartbeat. From
	// the environment it is a comma-separated list.
	Tools []string `json:"tools,omitempty" env:"KHUNQUANT_PROACTIVE_HEARTBEAT_TOOLS" envSeparator:","`
}

// AttentionConfig governs how much proactive output reaches the owner.
type AttentionConfig struct {
	Mode               string `json:"mode"                 env:"KHUNQUANT_PROACTIVE_ATTENTION_MODE"` // off | shadow | live
	DailyBudget        int    `json:"daily_budget"         env:"KHUNQUANT_PROACTIVE_ATTENTION_DAILY_BUDGET"`
	DedupWindowHours   int    `json:"dedup_window_hours"   env:"KHUNQUANT_PROACTIVE_ATTENTION_DEDUP_WINDOW_HOURS"`
	GroupWindowSeconds int    `json:"group_window_seconds" env:"KHUNQUANT_PROACTIVE_ATTENTION_GROUP_WINDOW_SECONDS"`
	Digest             bool   `json:"digest"               env:"KHUNQUANT_PROACTIVE_ATTENTION_DIGEST"`
}

// PerceptionConfig controls background market reads.
type PerceptionConfig struct {
	Enabled             bool `json:"enabled"                env:"KHUNQUANT_PROACTIVE_PERCEPTION_ENABLED"`
	MarketPollSeconds   int  `json:"market_poll_seconds"    env:"KHUNQUANT_PROACTIVE_PERCEPTION_MARKET_POLL_SECONDS"`
	ReadsPerMinuteLimit int  `json:"reads_per_minute_limit" env:"KHUNQUANT_PROACTIVE_PERCEPTION_READS_PER_MINUTE_LIMIT"`
	SnapshotMinutes     int  `json:"snapshot_minutes"       env:"KHUNQUANT_PROACTIVE_PERCEPTION_SNAPSHOT_MINUTES"`
}

// ApprovalsConfig controls owner approval of proposed actions.
type ApprovalsConfig struct {
	Enabled       bool `json:"enabled"        env:"KHUNQUANT_PROACTIVE_APPROVALS_ENABLED"`
	ExpiryMinutes int  `json:"expiry_minutes" env:"KHUNQUANT_PROACTIVE_APPROVALS_EXPIRY_MINUTES"`
}

// CognitionConfig bounds proactive LLM use.
type CognitionConfig struct {
	Enabled          bool   `json:"enabled"            env:"KHUNQUANT_PROACTIVE_COGNITION_ENABLED"`
	MaxConcurrentLLM int    `json:"max_concurrent_llm" env:"KHUNQUANT_PROACTIVE_COGNITION_MAX_CONCURRENT_LLM"`
	WakesPerHour     int    `json:"wakes_per_hour"     env:"KHUNQUANT_PROACTIVE_COGNITION_WAKES_PER_HOUR"`
	TokensPerDay     int    `json:"tokens_per_day"     env:"KHUNQUANT_PROACTIVE_COGNITION_TOKENS_PER_DAY"`
	ProactiveModel   string `json:"proactive_model"    env:"KHUNQUANT_PROACTIVE_COGNITION_PROACTIVE_MODEL"` // "" = routing light model
}

// ReflectionConfig schedules the end-of-day reflection.
type ReflectionConfig struct {
	Enabled bool   `json:"enabled"  env:"KHUNQUANT_PROACTIVE_REFLECTION_ENABLED"`
	AtLocal string `json:"at_local" env:"KHUNQUANT_PROACTIVE_REFLECTION_AT_LOCAL"` // "21:00"
}

// LearningConfig controls learning from owner feedback.
type LearningConfig struct {
	Enabled     bool    `json:"enabled"      env:"KHUNQUANT_PROACTIVE_LEARNING_ENABLED"`
	HoldoutRate float64 `json:"holdout_rate" env:"KHUNQUANT_PROACTIVE_LEARNING_HOLDOUT_RATE"`
}

// ModeConfig is a single off | shadow | live mode.
type ModeConfig struct {
	Mode string `json:"mode" env:"MODE"`
}

// ToggleConfig is a single on/off switch.
type ToggleConfig struct {
	Enabled bool `json:"enabled" env:"ENABLED"`
}

// defaultProactiveConfig is the shipped default: proactive off, everything
// that delivers to a person off or shadow-only.
func defaultProactiveConfig() ProactiveConfig {
	return ProactiveConfig{
		Enabled:   false,
		Heartbeat: ProactiveHeartbeatConfig{Mode: HeartbeatModeLegacy, Model: string(proactive.ModelTierPrimary)},
		Attention: AttentionConfig{
			Mode:               ProactiveModeShadow,
			DailyBudget:        3,
			DedupWindowHours:   24,
			GroupWindowSeconds: 60,
			Digest:             true,
		},
		Perception: PerceptionConfig{
			Enabled:             false,
			MarketPollSeconds:   60,
			ReadsPerMinuteLimit: 30,
			SnapshotMinutes:     15,
		},
		Triggers:   ModeConfig{Mode: ProactiveModeOff},
		Approvals:  ApprovalsConfig{Enabled: false, ExpiryMinutes: 10},
		Cognition:  CognitionConfig{MaxConcurrentLLM: 1, WakesPerHour: 6, TokensPerDay: 200000},
		Policy:     ModeConfig{Mode: ProactiveModeOff},
		Reflection: ReflectionConfig{Enabled: false, AtLocal: "21:00"},
		Learning:   LearningConfig{Enabled: false, HoldoutRate: 0.05},
	}
}

// normMode lowercases and trims a mode string.
func normMode(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// modeOrDefault returns the normalised mode when it is one of allowed, def when
// the field is empty or unknown. ok is false only for a non-empty unknown value.
func modeOrDefault(raw, def string, allowed ...string) (mode string, ok bool) {
	m := normMode(raw)
	if m == "" {
		return def, true
	}
	for _, a := range allowed {
		if m == a {
			return m, true
		}
	}
	return def, false
}

// heartbeatTierOrDefault resolves proactive.heartbeat.model: empty or unknown is
// primary, so a typo can never move the heartbeat to the cheap model.
func heartbeatTierOrDefault(raw string) proactive.ModelTier {
	tier, err := proactive.ParseModelTier(raw)
	if err != nil {
		return proactive.ModelTierPrimary
	}
	return tier
}

var (
	heartbeatModes = []string{HeartbeatModeLegacy, HeartbeatModeGoverned, HeartbeatModeSweep}
	heartbeatTiers = []string{string(proactive.ModelTierPrimary), string(proactive.ModelTierLight)}
	tieredModes    = []string{ProactiveModeOff, ProactiveModeShadow, ProactiveModeLive}
)

// validateOwner reports every problem with the owner block. An entirely empty
// owner is not a problem; a partly filled one is judged field by field (an
// incomplete owner is simply not configured, see OwnerRoute).
func validateOwner(o OwnerConfig) []error {
	var errs []error
	channel := normMode(o.Channel)
	chatID := strings.TrimSpace(o.ChatID)
	sender := strings.TrimSpace(o.SenderID)

	var senderPlatform string
	if sender != "" {
		platform, id, ok := identity.ParseCanonicalID(sender)
		switch {
		case !ok:
			errs = append(errs, errors.New(
				"proactive.owner.sender_id must be canonical \"platform:id\" (for example \"telegram:123456\")"))
		default:
			senderPlatform = strings.ToLower(platform)
			if channel != "" && senderPlatform != channel {
				errs = append(errs, fmt.Errorf(
					"proactive.owner.sender_id platform %q does not match proactive.owner.channel %q",
					platform, channel))
			}
			if senderPlatform == "telegram" {
				if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
					errs = append(errs, errors.New(
						"proactive.owner.sender_id for telegram must be the numeric user id (use /whoami), not a username"))
				}
			}
		}
	}

	if chatID != "" && (channel == "telegram" || (channel == "" && senderPlatform == "telegram")) {
		n, err := strconv.ParseInt(chatID, 10, 64)
		switch {
		case err != nil:
			errs = append(errs, errors.New(
				"proactive.owner.chat_id for telegram must be a plain numeric chat id"))
		case n < 0:
			errs = append(errs, errors.New(
				"proactive.owner.chat_id is a group, supergroup or channel; the owner chat must be a direct chat"))
		case n == 0:
			errs = append(errs, errors.New("proactive.owner.chat_id must not be 0"))
		}
	}
	return errs
}

// ValidateProactive reports every problem in the proactive section as one
// joined error, or nil. An empty owner is valid: proactive delivery is simply
// not available until one is set.
//
// The loaders do not fail on this error (see reportProactive); strictness is
// applied where a human gets immediate feedback, such as `khunquant owner set`.
func ValidateProactive(c ProactiveConfig) error {
	errs := validateOwner(c.Owner)

	checkMode := func(field, raw string, allowed []string) {
		if _, ok := modeOrDefault(raw, "", allowed...); !ok {
			errs = append(errs, fmt.Errorf("%s %q is not one of %s",
				field, raw, strings.Join(allowed, "|")))
		}
	}
	checkMode("proactive.heartbeat.mode", c.Heartbeat.Mode, heartbeatModes)
	checkMode("proactive.heartbeat.model", c.Heartbeat.Model, heartbeatTiers)
	checkMode("proactive.attention.mode", c.Attention.Mode, tieredModes)
	checkMode("proactive.triggers.mode", c.Triggers.Mode, tieredModes)
	checkMode("proactive.policy.mode", c.Policy.Mode, tieredModes)

	for _, n := range []struct {
		field string
		v     int
	}{
		{"proactive.attention.daily_budget", c.Attention.DailyBudget},
		{"proactive.attention.dedup_window_hours", c.Attention.DedupWindowHours},
		{"proactive.attention.group_window_seconds", c.Attention.GroupWindowSeconds},
		{"proactive.perception.market_poll_seconds", c.Perception.MarketPollSeconds},
		{"proactive.perception.reads_per_minute_limit", c.Perception.ReadsPerMinuteLimit},
		{"proactive.perception.snapshot_minutes", c.Perception.SnapshotMinutes},
		{"proactive.approvals.expiry_minutes", c.Approvals.ExpiryMinutes},
		{"proactive.cognition.max_concurrent_llm", c.Cognition.MaxConcurrentLLM},
		{"proactive.cognition.wakes_per_hour", c.Cognition.WakesPerHour},
		{"proactive.cognition.tokens_per_day", c.Cognition.TokensPerDay},
	} {
		if n.v < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative (got %d)", n.field, n.v))
		}
	}

	if r := c.Learning.HoldoutRate; math.IsNaN(r) || r < 0 || r > 1 {
		errs = append(errs, fmt.Errorf("proactive.learning.holdout_rate must be within [0, 1] (got %v)", r))
	}

	if at := strings.TrimSpace(c.Reflection.AtLocal); at != "" && !validClock(at) {
		errs = append(errs, fmt.Errorf("proactive.reflection.at_local %q must be HH:MM (24-hour)", c.Reflection.AtLocal))
	}

	return errors.Join(errs...)
}

// validClock reports whether s is a zero-padded 24-hour "HH:MM".
func validClock(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	_, err := time.Parse("15:04", s)
	return err == nil
}

// OwnerRoute returns where proactive output goes. It is false unless channel,
// chat id and sender id are all set and valid; PeerID is the id part of the
// canonical sender.
func (c ProactiveConfig) OwnerRoute() (proactive.OwnerRoute, bool) {
	o := c.Owner
	channel := normMode(o.Channel)
	chatID := strings.TrimSpace(o.ChatID)
	sender := strings.TrimSpace(o.SenderID)
	if channel == "" || chatID == "" || sender == "" {
		return proactive.OwnerRoute{}, false
	}
	if len(validateOwner(o)) > 0 {
		return proactive.OwnerRoute{}, false
	}
	platform, id, ok := identity.ParseCanonicalID(sender)
	if !ok {
		return proactive.OwnerRoute{}, false
	}
	return proactive.OwnerRoute{
		Channel:  channel,
		ChatID:   chatID,
		SenderID: identity.BuildCanonicalID(platform, id),
		PeerID:   id,
	}, true
}

// ValidateOwnerInput judges an owner on its own, the way `khunquant owner set`
// and the launcher's owner form both must: all three fields are required, an
// unrelated typo elsewhere in the proactive section is none of its business,
// and a group chat, a non-canonical sender or a platform mismatch is refused
// with the reason. On success it returns the normalised owner to store (channel
// lower-cased, sender in canonical "platform:id" form).
func ValidateOwnerInput(channel, chatID, sender string) (OwnerConfig, error) {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"channel", channel}, {"chat id", chatID}, {"sender id", sender},
	} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return OwnerConfig{}, fmt.Errorf("channel, chat id and sender id must all be set (missing: %s)",
			strings.Join(missing, ", "))
	}
	candidate := defaultProactiveConfig()
	candidate.Owner = OwnerConfig{Channel: channel, ChatID: chatID, SenderID: sender}
	if err := ValidateProactive(candidate); err != nil {
		return OwnerConfig{}, err
	}
	route, ok := candidate.OwnerRoute()
	if !ok {
		return OwnerConfig{}, errors.New("channel, chat id and sender id must all be set and valid")
	}
	return OwnerConfig{Channel: route.Channel, ChatID: route.ChatID, SenderID: route.SenderID}, nil
}

// EffectiveModes is what proactive components must apply. Compute it with
// ProactiveConfig.Effective; never read the raw fields.
type EffectiveModes struct {
	Enabled bool
	// OwnerConfigured is true only for a complete, valid owner in a valid
	// section, whether or not proactive is Enabled.
	OwnerConfigured bool
	// Owner is the delivery route; zero unless CanDeliver.
	Owner proactive.OwnerRoute

	HeartbeatMode string // legacy | governed | sweep
	AttentionMode string // off | shadow | live
	TriggersMode  string // off | shadow | live
	PolicyMode    string // off | shadow | live

	// HeartbeatModel is the tier a governed heartbeat's turn runs on. An empty or
	// unknown value is primary, never light.
	HeartbeatModel proactive.ModelTier

	// HeartbeatTools is proactive.heartbeat.tools, cleaned: names trimmed,
	// de-duplicated and, when a tool-name checker is registered
	// (SetToolNameChecker), stripped of names no tool has. Empty means the
	// built-in default set for the turn; a lone "*" means every permitted tool.
	HeartbeatTools []string

	Approvals  bool
	Mandates   bool
	Intents    bool
	News       bool
	Perception bool
	Cognition  bool
	Reflection bool
	Learning   bool

	// Downgrades describes, one entry per change, each mode or switch that was
	// lowered from what the config asked for. It never contains owner IDs.
	Downgrades []string
}

// CanDeliver reports whether proactive output may be delivered to the owner:
// the section is enabled and a valid owner is configured.
func (e EffectiveModes) CanDeliver() bool { return e.Enabled && e.OwnerConfigured }

// Effective returns the modes actually applied. A mode that delivers to the
// owner is downgraded when proactive is disabled or no valid owner is
// configured (an invalid section counts as no owner):
//
//	attention.mode  live     -> shadow
//	triggers.mode   live     -> shadow
//	heartbeat.mode  governed -> legacy, sweep -> legacy
//	approvals, mandates      -> off
//
// A governed heartbeat hands its decisions to the attention outbox, which only
// delivers when attention is live. With attention off or shadow they would be
// recorded and never sent, so even with a valid owner:
//
//	heartbeat.mode  governed -> legacy   unless the effective attention.mode is live
//
// Live triggers deliver the same way: a candidate is decided by attention, and with
// attention off or shadow the decision is logged and nothing is sent. A live
// trigger would take its alerts off the paths that do deliver (the migration
// switches the cron alert jobs off) and then lose them, so even with a valid owner:
//
//	triggers.mode   live     -> shadow   unless the effective attention.mode is live
//
// off and shadow values are returned as configured, an unknown mode string
// falls back to that field's default, and the remaining switches are the
// configured value AND-ed with Enabled. It is a pure function of c.
func (c ProactiveConfig) Effective() EffectiveModes {
	valid := ValidateProactive(c) == nil
	route, haveOwner := c.OwnerRoute()
	ownerOK := valid && haveOwner

	e := EffectiveModes{
		Enabled:         c.Enabled,
		OwnerConfigured: ownerOK,
	}
	if c.Enabled && ownerOK {
		e.Owner = route
	}

	e.HeartbeatMode, _ = modeOrDefault(c.Heartbeat.Mode, HeartbeatModeLegacy, heartbeatModes...)
	e.HeartbeatModel = heartbeatTierOrDefault(c.Heartbeat.Model)
	e.HeartbeatTools, _ = cleanToolNames(c.Heartbeat.Tools)
	e.AttentionMode, _ = modeOrDefault(c.Attention.Mode, ProactiveModeShadow, tieredModes...)
	e.TriggersMode, _ = modeOrDefault(c.Triggers.Mode, ProactiveModeOff, tieredModes...)
	e.PolicyMode, _ = modeOrDefault(c.Policy.Mode, ProactiveModeOff, tieredModes...)

	e.Intents = c.Enabled && c.Intents.Enabled
	e.News = c.Enabled && c.News.Enabled
	e.Perception = c.Enabled && c.Perception.Enabled
	e.Cognition = c.Enabled && c.Cognition.Enabled
	e.Reflection = c.Enabled && c.Reflection.Enabled
	e.Learning = c.Enabled && c.Learning.Enabled
	e.Approvals = c.Enabled && c.Approvals.Enabled
	e.Mandates = c.Enabled && c.Mandates.Enabled

	if c.Enabled && ownerOK {
		// The owner can be reached, but a governed heartbeat still needs attention
		// to deliver: shadow and off record decisions and send nothing, which
		// would silently lose every heartbeat message.
		if e.HeartbeatMode == HeartbeatModeGoverned && e.AttentionMode != ProactiveModeLive {
			e.Downgrades = append(e.Downgrades, fmt.Sprintf(
				"proactive: attention.mode is %s, not live; heartbeat.mode governed → legacy", e.AttentionMode))
			e.HeartbeatMode = HeartbeatModeLegacy
		}
		if e.TriggersMode == ProactiveModeLive && e.AttentionMode != ProactiveModeLive {
			e.Downgrades = append(e.Downgrades, fmt.Sprintf(
				"proactive: attention.mode is %s, not live; triggers.mode live → shadow", e.AttentionMode))
			e.TriggersMode = ProactiveModeShadow
		}
		return e
	}

	var reason string
	switch {
	case !c.Enabled:
		reason = "proactive disabled"
	case !valid:
		reason = "proactive config invalid, treated as no owner"
	default:
		reason = "owner not configured"
	}
	down := func(change string) {
		e.Downgrades = append(e.Downgrades, fmt.Sprintf("proactive: %s; %s", reason, change))
	}

	if e.AttentionMode == ProactiveModeLive {
		e.AttentionMode = ProactiveModeShadow
		down("attention.mode live → shadow")
	}
	if e.TriggersMode == ProactiveModeLive {
		e.TriggersMode = ProactiveModeShadow
		down("triggers.mode live → shadow")
	}
	if e.HeartbeatMode != HeartbeatModeLegacy {
		down(fmt.Sprintf("heartbeat.mode %s → legacy", e.HeartbeatMode))
		e.HeartbeatMode = HeartbeatModeLegacy
	}
	// Reaching here means Enabled is false or there is no valid owner, so both
	// are off whatever was asked for.
	e.Approvals, e.Mandates = false, false
	if c.Approvals.Enabled {
		down("approvals.enabled true → false")
	}
	if c.Mandates.Enabled {
		down("mandates.enabled true → false")
	}
	return e
}

// Test seams for the log sinks.
var (
	proactiveErrorLog = func(msg string, fields map[string]any) { logger.ErrorCF("config", msg, fields) }
	proactiveWarnLog  = func(msg string, fields map[string]any) { logger.WarnCF("config", msg, fields) }
)

// loggedProactive records what has already been logged in this process, keyed
// by message text. Loaders never save, and the web backend loads config per
// request, so without it every load would repeat the same line. A hot reload
// logs only what is new.
var loggedProactive sync.Map

func logProactiveOnce(sink func(string, map[string]any), msg string) {
	if _, seen := loggedProactive.LoadOrStore(msg, struct{}{}); seen {
		return
	}
	sink(msg, nil)
}

// LogProactiveDowngrades logs one WARN per distinct downgrade per process.
// Messages name the setting and the reason, never the owner's IDs.
func LogProactiveDowngrades(c ProactiveConfig) {
	for _, d := range c.Effective().Downgrades {
		logProactiveOnce(proactiveWarnLog, d)
	}
}

// reportProactive is called by both loaders. It never fails the load: a typo in
// "proactive" must not stop the gateway, and with it every alert, from
// starting. An invalid section logs one ERROR per process and is treated as
// having no owner by Effective, so nothing proactive is delivered until it is
// fixed. The error text names fields and values of the offending settings, but
// never an owner ID.
func reportProactive(c ProactiveConfig) {
	if _, unknown := cleanToolNames(c.Heartbeat.Tools); len(unknown) > 0 {
		logProactiveOnce(proactiveWarnLog, fmt.Sprintf(
			"proactive.heartbeat.tools: ignoring unknown tool name(s) %s", strings.Join(unknown, ", ")))
	}
	if err := ValidateProactive(c); err != nil {
		logProactiveOnce(proactiveErrorLog,
			"proactive: invalid config, treating as no owner and disabling delivering modes: "+
				redactOwnerValues(c, err.Error()))
	}
	// Enabled without a usable owner changes no mode when the modes are left at
	// their defaults, so it would otherwise be silent: say it once.
	if c.Enabled && !c.Effective().OwnerConfigured {
		logProactiveOnce(proactiveWarnLog, "proactive: owner not configured; proactive delivery disabled")
	}
	LogProactiveDowngrades(c)
}

// AllToolsName in proactive.heartbeat.tools stands for every tool a proactive
// turn is permitted to call.
const AllToolsName = "*"

var (
	toolNameCheckerMu sync.RWMutex
	toolNameChecker   func(name string) bool
)

// SetToolNameChecker registers the predicate that says whether a name is a
// known tool. The tools package cannot be imported here (it imports this
// package), so it registers itself when it is linked in; a binary without it
// simply cannot tell, and then no name is judged unknown. nil unregisters.
func SetToolNameChecker(f func(name string) bool) {
	toolNameCheckerMu.Lock()
	defer toolNameCheckerMu.Unlock()
	toolNameChecker = f
}

// cleanToolNames trims, drops empties and de-duplicates a tool list, keeping
// the first occurrence's order. A name the registered checker does not know is
// returned in unknown instead of in names; AllToolsName is always kept. A list
// whose every name is unknown comes back empty, which means "the default set":
// a typo must not leave a heartbeat with no tools to read the portfolio with.
func cleanToolNames(raw []string) (names, unknown []string) {
	toolNameCheckerMu.RLock()
	known := toolNameChecker
	toolNameCheckerMu.RUnlock()

	seen := make(map[string]bool, len(raw))
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if n != AllToolsName && known != nil && !known(n) {
			unknown = append(unknown, n)
			continue
		}
		names = append(names, n)
	}
	return names, unknown
}

// redactOwnerValues removes the owner's IDs from s. The validation messages are
// written not to quote them; this is the backstop should one ever do so.
func redactOwnerValues(c ProactiveConfig, s string) string {
	for _, v := range []string{c.Owner.ChatID, c.Owner.SenderID} {
		if v = strings.TrimSpace(v); len(v) >= 3 {
			s = strings.ReplaceAll(s, v, "<owner id>")
		}
	}
	return s
}
