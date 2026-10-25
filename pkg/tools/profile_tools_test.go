package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

const (
	testOwnerSender = "telegram:7001234567"
	testOwnerChat   = "7001234567"
)

func profileTestStore(t *testing.T) *intent.Store {
	t.Helper()
	if _, ok := intent.DefaultProfile().Location(); !ok {
		t.Skip("host has no zoneinfo for Asia/Bangkok")
	}
	s, err := intent.NewStore(context.Background(), sqlitextest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ownerConfig(owned bool) *config.Config {
	cfg := config.DefaultConfig()
	if owned {
		cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: testOwnerChat, SenderID: testOwnerSender}
	}
	return cfg
}

func turnCtx(origin, channel, sender string) context.Context {
	ctx := context.Background()
	ctx = WithToolOrigin(ctx, origin)
	ctx = WithToolSender(ctx, sender)
	return WithToolContext(ctx, channel, "chat")
}

func decodeResult(t *testing.T, r *ToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.ForLLM), &m); err != nil {
		t.Fatalf("result is not JSON: %q (%v)", r.ForLLM, err)
	}
	return m
}

func TestGetProfileTool(t *testing.T) {
	store := profileTestStore(t)
	tool := NewGetProfileTool(store)
	ctx := context.Background()

	res := tool.Execute(ctx, nil)
	if res.IsError {
		t.Fatalf("error: %s", res.ForLLM)
	}
	m := decodeResult(t, res)
	if m["stored"] != false || m["profile"].(map[string]any)["timezone"] != "Asia/Bangkok" {
		t.Fatalf("empty store should show the defaults: %s", res.ForLLM)
	}

	p := intent.DefaultProfile()
	p.RiskTolerance = "moderate"
	p.NotificationBudgetPerDay = 0
	for i := 0; i < 3; i++ { // history must not be echoed
		if _, err := store.PutProfile(ctx, p, testOwnerSender, proactive.TrustUserAuthored); err != nil {
			t.Fatal(err)
		}
	}
	res = tool.Execute(ctx, nil)
	m = decodeResult(t, res)
	if m["stored"] != true || m["version"].(float64) != 3 || m["updated_by"] != testOwnerSender {
		t.Fatalf("version metadata wrong: %s", res.ForLLM)
	}
	prof := m["profile"].(map[string]any)
	if prof["risk_tolerance"] != "moderate" || prof["notification_budget_per_day"].(float64) != 0 {
		t.Fatalf("profile wrong: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "history") || strings.Count(res.ForLLM, `"profile"`) != 1 {
		t.Fatalf("the tool must not echo history: %s", res.ForLLM)
	}
	if res := NewGetProfileTool(nil).Execute(ctx, nil); !res.IsError {
		t.Fatal("a missing store must be an error, not a panic")
	}
}

func TestUpdateProfileTool_OwnerOnly(t *testing.T) {
	budget := map[string]any{"notification_budget_per_day": float64(2)}
	tests := []struct {
		name      string
		owned     bool
		ctx       context.Context
		wantOK    bool
		wantInErr string
	}{
		{"remote owner", true, turnCtx(OriginRemote, "telegram", testOwnerSender), true, ""},
		{"remote owner, upper-case sender", true, turnCtx(OriginRemote, "telegram", strings.ToUpper(testOwnerSender)), true, ""},
		{"remote owner, raw id on the owner channel", true, turnCtx(OriginRemote, "telegram", "7001234567"), true, ""},
		{"remote non-owner", true, turnCtx(OriginRemote, "telegram", "telegram:999"), false, "/whoami"},
		{"remote same id on another platform", true, turnCtx(OriginRemote, "discord", "discord:7001234567"), false, "owner set"},
		{"remote no sender", true, turnCtx(OriginRemote, "telegram", ""), false, "owner"},
		{"remote, no owner configured", false, turnCtx(OriginRemote, "telegram", testOwnerSender), false, "/whoami"},
		{"internal at the CLI", false, turnCtx(OriginInternal, "cli", "cron"), true, ""},
		{"internal system turn", true, turnCtx(OriginInternal, "system", "async:spawn"), false, "automated"},
		{"internal subagent turn", true, turnCtx(OriginInternal, "subagent", "x"), false, "automated"},
		{"cron", true, turnCtx(OriginCron, "telegram", testOwnerSender), false, "background"},
		{"heartbeat", true, turnCtx(OriginHeartbeat, "telegram", testOwnerSender), false, "background"},
		{"untagged", true, WithToolSender(context.Background(), testOwnerSender), false, "background"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := profileTestStore(t)
			tool := NewUpdateProfileTool(store, ownerConfig(tc.owned))
			res := tool.Execute(tc.ctx, budget)
			if res.IsError == tc.wantOK {
				t.Fatalf("IsError=%v, want %v: %s", res.IsError, !tc.wantOK, res.ForLLM)
			}
			cur, _ := store.Profile(context.Background())
			if tc.wantOK {
				if cur.NotificationBudgetPerDay != 2 {
					t.Fatalf("budget = %d, want 2", cur.NotificationBudgetPerDay)
				}
				return
			}
			if cur.NotificationBudgetPerDay != 3 {
				t.Fatalf("a refused call changed the profile: %+v", cur)
			}
			if !strings.Contains(res.ForLLM, tc.wantInErr) {
				t.Errorf("refusal %q should mention %q", res.ForLLM, tc.wantInErr)
			}
			for _, secret := range []string{"7001234567", "telegram:"} {
				if strings.Contains(res.ForLLM, secret) {
					t.Errorf("refusal leaks %q: %s", secret, res.ForLLM)
				}
			}
		})
	}
}

func TestUpdateProfileTool_RecordsAuthor(t *testing.T) {
	store := profileTestStore(t)
	tool := NewUpdateProfileTool(store, ownerConfig(true))
	if res := tool.Execute(turnCtx(OriginRemote, "telegram", strings.ToUpper(testOwnerSender)), map[string]any{"language": "th"}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	v, ok, _ := store.Latest(context.Background())
	if !ok || v.UpdatedBy != testOwnerSender {
		t.Fatalf("updated_by = %q", v.UpdatedBy)
	}
	if v.Provenance != proactive.TrustUserAuthored {
		t.Fatalf("provenance = %v", v.Provenance)
	}
}

func TestUpdateProfileTool_PatchSemantics(t *testing.T) {
	store := profileTestStore(t)
	tool := NewUpdateProfileTool(store, ownerConfig(true))
	ctx := turnCtx(OriginRemote, "telegram", testOwnerSender)
	bg := context.Background()

	exec := func(args map[string]any) *ToolResult { return tool.Execute(ctx, args) }

	// Partial: only the given field changes.
	res := exec(map[string]any{"risk_tolerance": "aggressive", "assets_to_avoid": []any{"doge", "shib"}})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	m := decodeResult(t, res)
	if got := m["changed"].([]any); len(got) != 2 {
		t.Fatalf("changed = %v", got)
	}
	cur, _ := store.Profile(bg)
	if cur.RiskTolerance != "aggressive" || cur.NotificationBudgetPerDay != 3 || len(cur.AssetsToAvoid) != 2 || cur.AssetsToAvoid[0] != "DOGE" {
		t.Fatalf("partial patch wrong: %+v", cur)
	}

	// Second patch keeps the first one's fields.
	if res := exec(map[string]any{"active_hours": map[string]any{"end": "21:00"}}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	cur, _ = store.Profile(bg)
	if cur.ActiveHours.End != "21:00" || cur.ActiveHours.Start != "07:00" || cur.RiskTolerance != "aggressive" {
		t.Fatalf("second patch lost data: %+v", cur)
	}

	// reset clears back to the default; 0 is a real value.
	if res := exec(map[string]any{"reset": []any{"risk_tolerance"}, "notification_budget_per_day": float64(0)}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	cur, _ = store.Profile(bg)
	if cur.RiskTolerance != "" || cur.NotificationBudgetPerDay != 0 || cur.ActiveHours.End != "21:00" {
		t.Fatalf("reset/0 wrong: %+v", cur)
	}
	if res := exec(map[string]any{"reset": []any{"notification_budget_per_day", "active_hours", "assets_to_avoid"}}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	cur, _ = store.Profile(bg)
	if cur.NotificationBudgetPerDay != 3 || cur.ActiveHours != (intent.HoursRange{Start: "07:00", End: "22:00"}) || len(cur.AssetsToAvoid) != 0 {
		t.Fatalf("reset should restore the defaults: %+v", cur)
	}

	// Errors name the problem and leave the store untouched.
	hist0, _ := store.ProfileHistory(bg, 0)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown field", map[string]any{"favourite_colour": "red", "risk_tolerance": "moderate"}, "favourite_colour"},
		{"invalid merged result", map[string]any{"allocation_targets": map[string]any{"crypto": float64(80), "cash": float64(40)}}, "add up to"},
		{"bad zone", map[string]any{"timezone": "Mars/Olympus"}, "Mars/Olympus"},
		{"budget out of range", map[string]any{"notification_budget_per_day": float64(99)}, "notification_budget_per_day"},
		{"wrong type", map[string]any{"language": float64(5)}, "language"},
		{"empty", map[string]any{}, "nothing to update"},
		{"reset unknown name", map[string]any{"reset": []any{"nickname"}}, `"nickname"`},
		{"reset not a list", map[string]any{"reset": "language"}, "array"},
		{"set and reset together", map[string]any{"language": "en", "reset": []any{"language"}}, `"language" is both set and listed in reset`},
	} {
		res := exec(tc.args)
		if !res.IsError || !strings.Contains(res.ForLLM, tc.want) {
			t.Errorf("%s: got %q, want an error mentioning %q", tc.name, res.ForLLM, tc.want)
		}
	}
	if hist1, _ := store.ProfileHistory(bg, 0); len(hist1) != len(hist0) {
		t.Fatalf("failed updates stored %d new versions", len(hist1)-len(hist0))
	}

	// A patch that changes nothing stores nothing.
	if res := exec(map[string]any{"timezone": "Asia/Bangkok"}); res.IsError || len(decodeResult(t, res)["changed"].([]any)) != 0 {
		t.Fatalf("no-op patch: %s", res.ForLLM)
	}
	if hist2, _ := store.ProfileHistory(bg, 0); len(hist2) != len(hist0) {
		t.Fatal("a no-op patch stored a version")
	}
}

// The typed schema goes through the registry's argument validator before the
// tool runs. These are the shapes the tool relies on.
func TestUpdateProfileTool_TypedSchemaThroughRegistry(t *testing.T) {
	store := profileTestStore(t)
	reg := NewToolRegistry()
	reg.Register(NewUpdateProfileTool(store, ownerConfig(true)))
	ctx := turnCtx(OriginRemote, "telegram", testOwnerSender)
	run := func(args map[string]any) *ToolResult {
		return reg.ExecuteWithContext(ctx, NameUpdateProfile, args, "telegram", "chat", nil)
	}
	bg := context.Background()

	// Only "end" of active_hours: the nested schema declares no required keys.
	if res := run(map[string]any{"active_hours": map[string]any{"end": "21:00"}}); res.IsError {
		t.Fatalf("partial active_hours refused: %s", res.ForLLM)
	}
	// A whole number that arrives as a JSON float (5.0) is an integer.
	if res := run(map[string]any{"notification_budget_per_day": float64(5)}); res.IsError {
		t.Fatalf("5.0 refused for an integer: %s", res.ForLLM)
	}
	// An empty allocation_targets object is a valid (empty) map.
	if res := run(map[string]any{"allocation_targets": map[string]any{"crypto": float64(30)}}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	if res := run(map[string]any{"allocation_targets": map[string]any{}}); res.IsError {
		t.Fatalf("empty allocation_targets refused: %s", res.ForLLM)
	}
	cur, _ := store.Profile(bg)
	if cur.ActiveHours.End != "21:00" || cur.NotificationBudgetPerDay != 5 || len(cur.AllocationTargets) != 0 {
		t.Fatalf("stored profile = %+v", cur)
	}

	// reset passes the validator and does its job.
	if res := run(map[string]any{"reset": []any{"active_hours", "notification_budget_per_day"}}); res.IsError {
		t.Fatalf("reset refused: %s", res.ForLLM)
	}
	if cur, _ = store.Profile(bg); cur.ActiveHours.End != "22:00" || cur.NotificationBudgetPerDay != 3 {
		t.Fatalf("reset through the registry did not restore defaults: %+v", cur)
	}

	// What the validator refuses, and how it says so.
	for name, args := range map[string]map[string]any{
		"fractional budget":  {"notification_budget_per_day": 2.5},
		"string budget":      {"notification_budget_per_day": "many"},
		"null (not offered)": {"language": nil},
		"hours as string":    {"active_hours": "09:00-21:00"},
		"assets as string":   {"assets_to_avoid": "DOGE"},
		"asset not string":   {"assets_to_avoid": []any{5.0}},
		"reset unknown name": {"reset": []any{"nickname"}},
		"hours unknown key":  {"active_hours": map[string]any{"end": 21}},
	} {
		if res := run(args); !res.IsError {
			t.Errorf("%s: the registry let %v through", name, args)
		}
	}
}

// Tool definitions go out on every request and the Gemini family rejects a
// property schema without a type, so the profile tools must not have one.
func TestProfileTools_SchemaFullyTyped(t *testing.T) {
	for _, tool := range []Tool{NewGetProfileTool(nil), NewUpdateProfileTool(nil, nil)} {
		if bad := untypedSchemaPaths(tool.Parameters()); len(bad) != 0 {
			t.Errorf("%s has untyped schema nodes: %v", tool.Name(), bad)
		}
	}
	// And the descriptions must not advertise null.
	up := NewUpdateProfileTool(nil, nil)
	b, _ := json.Marshal(up.Parameters())
	if strings.Contains(string(b), "null") || strings.Contains(up.Description(), "null") {
		t.Error("update_profile must not advertise null; clearing is the reset array")
	}
}

// TestUpdateProfile_DeniedByAutonomousCeiling goes through the registry with
// the policy armed: the class table must hide the tool from, and refuse it to,
// every autonomous turn, while a person's turn still gets it.
func TestUpdateProfile_DeniedByAutonomousCeiling(t *testing.T) {
	armPolicy(t, false, nil)
	if got := ClassOf(NameUpdateProfile); got != ClassAutomation {
		t.Fatalf("update_profile class = %v, want automation", got)
	}
	if got := ClassOf(NameGetProfile); got != ClassRead {
		t.Fatalf("get_profile class = %v, want read", got)
	}

	store := profileTestStore(t)
	reg := NewToolRegistry()
	reg.Register(NewUpdateProfileTool(store, ownerConfig(true)))
	reg.Register(NewGetProfileTool(store))
	args := map[string]any{"notification_budget_per_day": float64(20)}

	for _, origin := range []string{OriginCron, OriginHeartbeat} {
		ctx := turnCtx(origin, "telegram", testOwnerSender)
		res := reg.ExecuteWithContext(ctx, NameUpdateProfile, args, "telegram", "chat", nil)
		if !res.IsError {
			t.Errorf("%s turn changed the profile: %s", origin, res.ForLLM)
		}
		names := map[string]bool{}
		for _, d := range reg.ToProviderDefsFor(ctx) {
			names[d.Function.Name] = true
		}
		if names[NameUpdateProfile] || !names[NameGetProfile] {
			t.Errorf("%s turn sees %v; want get_profile but not update_profile", origin, names)
		}
		if res := reg.ExecuteWithContext(ctx, NameGetProfile, nil, "telegram", "chat", nil); res.IsError {
			t.Errorf("%s turn may read the profile: %s", origin, res.ForLLM)
		}
	}
	if cur, _ := store.Profile(context.Background()); cur.NotificationBudgetPerDay != 3 {
		t.Fatalf("background turn changed the budget: %+v", cur)
	}

	// The owner's own turn goes through, reset included.
	ctx := turnCtx(OriginRemote, "telegram", testOwnerSender)
	if res := reg.ExecuteWithContext(ctx, NameUpdateProfile, args, "telegram", "chat", nil); res.IsError {
		t.Fatalf("owner turn refused: %s", res.ForLLM)
	}
	if res := reg.ExecuteWithContext(ctx, NameUpdateProfile, map[string]any{"reset": []any{"digest_time"}}, "telegram", "chat", nil); res.IsError {
		t.Fatalf("reset through the registry refused: %s", res.ForLLM)
	}
}

// Without the policy armed the tool must still refuse a background turn.
func TestUpdateProfile_RefusesBackgroundWithoutPolicy(t *testing.T) {
	policyMu.Lock()
	prev := policyEnabled
	policyEnabled = false
	policyMu.Unlock()
	t.Cleanup(func() { policyMu.Lock(); policyEnabled = prev; policyMu.Unlock() })

	store := profileTestStore(t)
	res := NewUpdateProfileTool(store, ownerConfig(true)).Execute(
		turnCtx(OriginHeartbeat, "telegram", testOwnerSender), map[string]any{"notification_budget_per_day": float64(9)})
	if !res.IsError {
		t.Fatal("the in-tool check must not depend on the policy being armed")
	}
}
