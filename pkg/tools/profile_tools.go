package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// cliChannel is the channel of a turn typed at the local console.
const cliChannel = "cli"

// GetProfileTool shows the owner's structured profile.
type GetProfileTool struct {
	store *intent.Store
}

// NewGetProfileTool creates the get_profile tool over store.
func NewGetProfileTool(store *intent.Store) *GetProfileTool {
	return &GetProfileTool{store: store}
}

func (t *GetProfileTool) Name() string        { return NameGetProfile }
func (t *GetProfileTool) Description() string { return DescGetProfile }

func (t *GetProfileTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *GetProfileTool) Execute(ctx context.Context, _ map[string]any) *ToolResult {
	if t.store == nil {
		return ErrorResult("the profile store is not available")
	}
	v, ok, err := t.store.Latest(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("could not read the profile: %v", err))
	}
	if !ok {
		return NewToolResult(profileJSON(map[string]any{
			"stored":  false,
			"note":    "no profile has been saved yet; these are the defaults",
			"profile": intent.DefaultProfile(),
		}))
	}
	return NewToolResult(profileJSON(map[string]any{
		"stored":     true,
		"version":    v.Version,
		"updated_at": v.UpdatedAt.Format(time.RFC3339),
		"updated_by": v.UpdatedBy,
		"profile":    v.Profile,
	}))
}

func profileJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("could not encode the profile: %v", err)
	}
	return string(b)
}

// UpdateProfileTool changes the owner's structured profile.
//
// Who may call it, strictest first:
//   - never an autonomous turn (cron, heartbeat) and never an untagged one: the
//     profile sets the quiet hours and notification budget that bound every later
//     background message, so a background turn must not be able to move them. The
//     class table denies this tool to autonomous turns when the policy is armed;
//     the check here does not depend on the policy being armed;
//   - an internal turn only when it is typed at the local console (channel "cli");
//     a system or subagent turn can carry text a web page put there;
//   - a remote turn only when an owner is configured and the turn's sender is
//     that owner.
type UpdateProfileTool struct {
	store *intent.Store
	cfg   *config.Config

	mu sync.Mutex // one read-modify-write at a time, so two patches cannot lose each other
}

// NewUpdateProfileTool creates the update_profile tool. cfg is read on every
// call for the owner, so the tool follows whatever config the agent loop
// registered it with (see registerSharedTools: reload rebuilds the tool).
func NewUpdateProfileTool(store *intent.Store, cfg *config.Config) *UpdateProfileTool {
	return &UpdateProfileTool{store: store, cfg: cfg}
}

func (t *UpdateProfileTool) Name() string        { return NameUpdateProfile }
func (t *UpdateProfileTool) Description() string { return DescUpdateProfile }

// Parameters lists the profile fields as optional, fully typed properties
// (every property, nested ones included, carries a JSON "type": the Gemini
// family of APIs rejects a request whose tool schema has an untyped property,
// and tool definitions go out on every request). Clearing a field is the
// "reset" array rather than a JSON null, which a typed property cannot carry.
func (t *UpdateProfileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timezone": map[string]any{
				"type":        "string",
				"description": `IANA time zone name, for example "Asia/Bangkok".`,
			},
			"active_hours": map[string]any{
				"type": "object",
				"description": `Local hours in which messages are welcome, zero-padded 24-hour HH:MM. Either key may be given alone, ` +
					`so "don't message me after 9pm" is {"end":"21:00"}. An overnight range such as 22:00-06:00 is allowed.`,
				"properties": map[string]any{
					"start": map[string]any{"type": "string", "description": `Start of the window, inclusive, for example "07:00".`},
					"end":   map[string]any{"type": "string", "description": `End of the window, exclusive, for example "22:00".`},
				},
			},
			"language": map[string]any{
				"type":        "string",
				"description": `"th" or "en".`,
			},
			"risk_tolerance": map[string]any{
				"type":        "string",
				"description": `"conservative", "moderate" or "aggressive".`,
			},
			"horizon": map[string]any{
				"type":        "string",
				"description": "Investment horizon in a few words (max 200 characters).",
			},
			"primary_goal": map[string]any{
				"type":        "string",
				"description": "Primary investment goal in a few words (max 200 characters).",
			},
			"allocation_targets": map[string]any{
				"type": "object",
				"description": `Target allocation in percent by asset class, for example {"crypto":30,"th_equity":50,"cash":20}. ` +
					`Each value 0-100, total at most 100. Replaces the whole map.`,
			},
			"assets_to_avoid": map[string]any{
				"type":        "array",
				"description": `Symbols never to suggest, for example ["DOGE","SHIB"]. Replaces the whole list.`,
				"items":       map[string]any{"type": "string"},
			},
			"notification_budget_per_day": map[string]any{
				"type":        "integer",
				"description": "How many non-critical messages per day the assistant may send on its own, 0-20 (0 means none).",
			},
			"digest_time": map[string]any{
				"type":        "string",
				"description": `Local time of the daily digest, zero-padded HH:MM, for example "08:00".`,
			},
			"reset": map[string]any{
				"type": "array",
				"description": "To clear a field, list it here: it returns to its default (time zone Asia/Bangkok, active hours 07:00-22:00, " +
					"budget 3, digest 08:00) or to empty. A field cannot be both set and reset in one call.",
				"items": map[string]any{"type": "string", "enum": intent.FieldNames()},
			},
		},
	}
}

// resetField is the argument that clears fields.
const resetField = "reset"

// buildPatch turns the call's arguments into the JSON patch ApplyPatch takes:
// the profile fields as given, and every name in "reset" as null. A name that is
// not a profile field, or that is both set and reset, is an error.
func buildPatch(args map[string]any) ([]byte, error) {
	patch := make(map[string]any, len(args))
	for k, v := range args {
		if k != resetField {
			patch[k] = v
		}
	}
	var names []string
	switch r := args[resetField].(type) {
	case nil:
	case []string:
		names = r
	case []any:
		for _, v := range r {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("reset must list field names as strings, got %v", v)
			}
			names = append(names, s)
		}
	default:
		return nil, fmt.Errorf("reset must be an array of field names, got %T", args[resetField])
	}
	valid := make(map[string]bool)
	for _, n := range intent.FieldNames() {
		valid[n] = true
	}
	for _, n := range names {
		if !valid[n] {
			return nil, fmt.Errorf("reset names an unknown field %q; valid fields: %s", n, strings.Join(intent.FieldNames(), ", "))
		}
		if _, set := patch[n]; set {
			return nil, fmt.Errorf("%q is both set and listed in reset; do one or the other", n)
		}
		patch[n] = nil
	}
	return json.Marshal(patch)
}

func (t *UpdateProfileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	by, refusal := t.authorize(ctx)
	if refusal != "" {
		return ErrorResult(refusal)
	}
	if t.store == nil {
		return ErrorResult("the profile store is not available")
	}
	if len(args) == 0 {
		return ErrorResult("nothing to update: give at least one profile field, for example notification_budget_per_day")
	}
	patch, err := buildPatch(args)
	if err != nil {
		return ErrorResult(fmt.Sprintf("profile not changed: %v", err))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	before, err := t.store.Profile(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("could not read the current profile: %v", err))
	}
	after, err := before.ApplyPatch(patch)
	if err != nil {
		return ErrorResult(fmt.Sprintf("profile not changed: %v", err))
	}
	changed := changedProfileFields(before, after)
	if len(changed) == 0 {
		return NewToolResult(profileJSON(map[string]any{
			"changed": []string{},
			"note":    "no change: the profile already had these values",
			"profile": after,
		}))
	}
	version, err := t.store.PutProfile(ctx, after, by, proactive.TrustUserAuthored)
	if err != nil {
		return ErrorResult(fmt.Sprintf("profile not changed: %v", err))
	}
	return NewToolResult(profileJSON(map[string]any{
		"changed": changed,
		"version": version,
		"profile": after,
	}))
}

// authorize decides whether this turn may change the profile. It returns the
// name to record as the author, or a refusal message for the model.
func (t *UpdateProfileTool) authorize(ctx context.Context) (by, refusal string) {
	by, verdict := checkOwnerTurn(ctx, t.cfg, true)
	switch verdict {
	case ownerYes:
		return by, ""
	case ownerBackground:
		return "", "update_profile is not available in background or scheduled turns: only the owner, in a conversation, can change the profile"
	case ownerAutomated:
		return "", "update_profile can only be used by the owner in a chat or at the command line, not from an automated turn"
	case ownerSubTurn:
		return "", "update_profile can only be used by the owner in their own message, not by a delegated task (a subagent): the profile can only be changed by the owner in their own chat or at the command line"
	case ownerNoConfig:
		return "", ownerSetupHint
	case ownerNoOwner:
		return "", "No owner is configured, so the profile cannot be changed from a chat. " + ownerSetupHint
	case ownerOther:
		return "", "Only the owner can change the profile, and this message is not from the configured owner. " + ownerSetupHint
	}
	return "", "update_profile is not available in this kind of turn"
}

const ownerSetupHint = "To become the owner, send /whoami in your chat to see your sender id, then run `khunquant owner set` on the machine running KhunQuant."

func changedProfileFields(a, b intent.Profile) []string {
	type field struct {
		name string
		a, b any
	}
	fields := []field{
		{"timezone", a.Timezone, b.Timezone},
		{"active_hours", a.ActiveHours, b.ActiveHours},
		{"language", a.Language, b.Language},
		{"risk_tolerance", a.RiskTolerance, b.RiskTolerance},
		{"horizon", a.Horizon, b.Horizon},
		{"primary_goal", a.PrimaryGoal, b.PrimaryGoal},
		{"allocation_targets", a.AllocationTargets, b.AllocationTargets},
		{"assets_to_avoid", a.AssetsToAvoid, b.AssetsToAvoid},
		{"notification_budget_per_day", a.NotificationBudgetPerDay, b.NotificationBudgetPerDay},
		{"digest_time", a.DigestTime, b.DigestTime},
	}
	out := []string{}
	for _, f := range fields {
		if !reflect.DeepEqual(f.a, f.b) {
			out = append(out, f.name)
		}
	}
	return out
}
