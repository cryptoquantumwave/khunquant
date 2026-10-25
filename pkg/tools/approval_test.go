package tools

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

var approvalCodePattern = regexp.MustCompile(`/approve ([A-Z2-9]{8})`)

// armApproval arms policy with an empty table, flags names for approval and
// installs a fresh store, restoring all three afterwards.
func armApproval(t *testing.T, names ...string) *ApprovalStore {
	t.Helper()
	armApprovalWithNotifier(t, func(context.Context, string, string, string) error { return nil }, names...)
	return Approvals()
}

// sentApprovals records what the notifier delivered, so tests read the code
// from where the operator would.
type sentApprovals struct{ texts []string }

func (s *sentApprovals) notify(_ context.Context, channel, chatID, text string) error {
	s.texts = append(s.texts, channel+"|"+chatID+"|"+text)
	return nil
}

func (s *sentApprovals) last() string {
	if len(s.texts) == 0 {
		return ""
	}
	return s.texts[len(s.texts)-1]
}

var lastSent *sentApprovals

func armApprovalWithNotifier(t *testing.T, notify ApprovalNotifier, names ...string) {
	t.Helper()
	armPolicy(t, false, map[string]ToolPolicy{})

	policyMu.Lock()
	prevApproval := policyApproval
	policyMu.Unlock()
	SetRequiredApprovals(names)

	store := NewApprovalStore(DefaultApprovalTTL)
	prevStore := SetApprovalStore(store)

	sent := &sentApprovals{}
	lastSent = sent
	prevNotifier := SetApprovalNotifier(func(ctx context.Context, channel, chatID, text string) error {
		_ = sent.notify(ctx, channel, chatID, text)
		return notify(ctx, channel, chatID, text)
	})

	t.Cleanup(func() {
		SetApprovalNotifier(prevNotifier)
		SetApprovalStore(prevStore)
		policyMu.Lock()
		policyApproval = prevApproval
		policyMu.Unlock()
	})
}

func approvalCtx(channel, chatID, sender string) context.Context {
	ctx := WithToolOrigin(context.Background(), OriginRemote)
	ctx = WithToolContext(ctx, channel, chatID)
	return WithToolSender(ctx, sender)
}

// requestCode makes a gated call, asserts it was held, and returns the code the
// operator was sent.
func requestCode(t *testing.T, ctx context.Context, name string, args map[string]any) string {
	t.Helper()
	before := len(lastSent.texts)
	held := checkToolPolicy(ctx, name, args, false)
	if held == nil {
		t.Fatalf("%s ran without approval", name)
	}
	if len(lastSent.texts) != before+1 {
		t.Fatalf("held call sent the operator %d messages, want 1", len(lastSent.texts)-before)
	}
	m := approvalCodePattern.FindStringSubmatch(lastSent.last())
	if m == nil {
		t.Fatalf("held call gave the operator no code: %q", lastSent.last())
	}
	return m[1]
}

func orderArgs(amount float64) map[string]any {
	return map[string]any{
		"provider": "binance", "symbol": "BTC/USDT", "side": "buy",
		"type": "market", "amount": amount, "confirm": true,
	}
}

// The end-to-end path: held, approved, runs once, held again. Every negative
// test below leans on this one to show the gate can actually be passed.
func TestApproval_ApproveThenIdenticalCallRunsOnce(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))

	tool, err := store.Approve(code, "telegram", "chat1", "alice")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if tool != NameCreateOrder {
		t.Errorf("Approve returned tool %q, want %q", tool, NameCreateOrder)
	}

	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false); refusal != nil {
		t.Fatalf("approved call was still held: %q", refusal.ForLLM)
	}

	// A grant authorises one call, not a standing permission.
	requestCode(t, ctx, NameCreateOrder, orderArgs(1))
}

// The code must never reach the model: ForLLM is written to session history.
func TestApproval_CodeIsNotInModelContext(t *testing.T) {
	armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	held := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false)
	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))
	// held and code only correspond because Request reuses the pending entry
	// for an identical call. Pin that, or this test could compare a stale
	// result against a fresh code and pass vacuously.
	if codes := approvalCodePattern.FindAllStringSubmatch(strings.Join(lastSent.texts, "\n"), -1); len(codes) != 2 || codes[0][1] != codes[1][1] {
		t.Fatalf("repeating an identical held call did not reuse its code: %q", lastSent.texts)
	}

	for field, v := range map[string]string{"ForLLM": held.ForLLM, "ForUser": held.ForUser} {
		if strings.Contains(v, code) {
			t.Errorf("%s carries the approval code: %q", field, v)
		}
	}
	// Delivered to the chat the call came from, and nowhere else.
	if !strings.HasPrefix(lastSent.last(), "telegram|chat1|") {
		t.Errorf("code delivered to the wrong place: %q", lastSent.last())
	}
}

// confirm=false previews and confirm=true executes. Hashing confirm would make
// the approved call's hash differ from the executing one's.
func TestApproval_ConfirmIsExcludedFromTheHash(t *testing.T) {
	a, err := ApprovalArgsHash(map[string]any{"amount": 1.0, "confirm": false})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ApprovalArgsHash(map[string]any{"amount": 1.0, "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("confirm changes the approval hash; an approval can never match its executing call")
	}

	c, err := ApprovalArgsHash(map[string]any{"amount": 2.0, "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("different amounts hash the same; the hash is not binding the arguments")
	}
}

func TestApproval_PreviewIsNotHeld(t *testing.T) {
	armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	preview := orderArgs(1)
	preview["confirm"] = false
	if refusal := checkToolPolicy(ctx, NameCreateOrder, preview, true); refusal != nil {
		t.Fatalf("a confirm=false preview was held: %q", refusal.ForLLM)
	}
	// And the executing form of the same call is held.
	requestCode(t, ctx, NameCreateOrder, orderArgs(1))
}

func TestApproval_ChangedArgumentsNeedANewApproval(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))
	if _, err := store.Approve(code, "telegram", "chat1", "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	bigger := requestCode(t, ctx, NameCreateOrder, orderArgs(100))
	if bigger == code {
		t.Fatal("a changed amount reused the approved code")
	}

	// The original approval is untouched and still usable for its own call.
	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false); refusal != nil {
		t.Fatalf("the approved call was held after an unrelated request: %q", refusal.ForLLM)
	}
}

func TestApproval_OnlyTheRequesterCanApprove(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")
	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))

	for _, tc := range []struct{ name, channel, chatID, sender string }{
		{"other chat", "telegram", "chat2", "alice"},
		{"other sender in the same chat", "telegram", "chat1", "mallory"},
		{"other channel", "discord", "chat1", "alice"},
		// Async tool results are re-published inbound on "system" with
		// model-influenced content.
		{"system channel", "system", "chat1", "async:web_fetch"},
		{"cron", "telegram", "chat1", "cron"},
	} {
		if _, err := store.Approve(code, tc.channel, tc.chatID, tc.sender); !errors.Is(err, ErrApprovalWrongUser) {
			t.Errorf("%s: Approve err = %v, want ErrApprovalWrongUser", tc.name, err)
		}
		if _, err := store.Deny(code, tc.channel, tc.chatID, tc.sender); !errors.Is(err, ErrApprovalWrongUser) {
			t.Errorf("%s: Deny err = %v, want ErrApprovalWrongUser", tc.name, err)
		}
	}

	// None of those attempts consumed or granted it; the requester still can.
	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false); refusal == nil {
		t.Fatal("a failed approval attempt granted the call")
	}
	if _, err := store.Approve(code, "telegram", "chat1", "alice"); err != nil {
		t.Fatalf("rightful approval failed after rejected attempts: %v", err)
	}
}

func TestApproval_DenyDiscardsTheRequest(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")
	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))

	if _, err := store.Deny(code, "telegram", "chat1", "alice"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if _, err := store.Approve(code, "telegram", "chat1", "alice"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("approve after deny: err = %v, want ErrApprovalNotFound", err)
	}
	if next := requestCode(t, ctx, NameCreateOrder, orderArgs(1)); next == code {
		t.Fatal("a denied code was reissued")
	}
}

func TestApproval_Expiry(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	ctx := approvalCtx("telegram", "chat1", "alice")

	// An unanswered request expires.
	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))
	now = now.Add(DefaultApprovalTTL + time.Second)
	if _, err := store.Approve(code, "telegram", "chat1", "alice"); err == nil {
		t.Fatal("an expired request was approvable")
	}

	// So does an unused grant.
	code = requestCode(t, ctx, NameCreateOrder, orderArgs(1))
	if _, err := store.Approve(code, "telegram", "chat1", "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	now = now.Add(DefaultApprovalTTL + time.Second)
	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false); refusal == nil {
		t.Fatal("an expired grant still let the call run")
	}
}

func TestApproval_NoIdentityRefusesWithoutACode(t *testing.T) {
	armApproval(t, NameCreateOrder)

	ctx := WithToolContext(WithToolOrigin(context.Background(), OriginRemote), "telegram", "chat1")
	refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false)
	if refusal == nil {
		t.Fatal("a call with no sender ran despite requiring approval")
	}
	if len(lastSent.texts) != 0 {
		t.Fatalf("issued a code nobody can ever redeem: %q", lastSent.texts)
	}
}

func TestApproval_NoNotifierRefusesWithoutACode(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	SetApprovalNotifier(nil)

	if refusal := checkToolPolicy(approvalCtx("telegram", "chat1", "alice"), NameCreateOrder, orderArgs(1), false); refusal == nil {
		t.Fatal("a gated call ran with no way to reach the operator")
	}
	if n := len(store.entries); n != 0 {
		t.Fatalf("left %d pending approval(s) that nobody was told about", n)
	}
}

func TestApproval_DeliveryFailureIsReported(t *testing.T) {
	armApprovalWithNotifier(t, func(context.Context, string, string, string) error {
		return errors.New("channel down")
	}, NameCreateOrder)

	refusal := checkToolPolicy(approvalCtx("telegram", "chat1", "alice"), NameCreateOrder, orderArgs(1), false)
	if refusal == nil || !strings.Contains(refusal.ForLLM, "channel down") {
		t.Fatalf("delivery failure not reported: %+v", refusal)
	}
	if n := len(Approvals().entries); n != 0 {
		t.Fatalf("undelivered request left %d pending approval(s)", n)
	}
}

// Approval is opt-in: no shipped policy asks for it.
func TestApproval_OffByDefault(t *testing.T) {
	for name, p := range defaultToolPolicies() {
		if p.RequiresApproval {
			t.Errorf("%s requires approval by default; approval must be opt-in", name)
		}
	}
}

func TestApproval_RiskReducingToolsCannotBeGated(t *testing.T) {
	armApproval(t)

	ignored := SetRequiredApprovals([]string{NameFuturesClosePosition, NameCreateOrder})
	if len(ignored) != 1 || ignored[0] != NameFuturesClosePosition {
		t.Fatalf("ignored = %v, want [%s]", ignored, NameFuturesClosePosition)
	}

	ctx := approvalCtx("telegram", "chat1", "alice")
	if refusal := checkToolPolicy(ctx, NameFuturesClosePosition, map[string]any{"confirm": true}, false); refusal != nil {
		t.Fatalf("closing a position was held for approval: %q", refusal.ForLLM)
	}
	// The same call honoured the other name, so the ignore was selective.
	requestCode(t, ctx, NameCreateOrder, orderArgs(1))
}

// A call that another rule refuses must not create a pending approval the
// operator could grant but never use.
func TestApproval_BlockedOriginWinsOverApproval(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	SetToolPolicies(map[string]ToolPolicy{NameCreateOrder: {BlockedOrigins: []string{OriginCron}}})

	ctx := WithToolOrigin(context.Background(), OriginCron)
	ctx = WithToolSender(WithToolContext(ctx, "telegram", "chat1"), "cron")
	refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1), false)
	if refusal == nil {
		t.Fatal("cron call ran")
	}
	if len(lastSent.texts) != 0 {
		t.Fatalf("a cron-blocked call was offered for approval: %q", lastSent.texts)
	}
	if n := len(store.entries); n != 0 {
		t.Fatalf("blocked call left %d pending approval(s)", n)
	}
}

type countingTool struct {
	name  string
	calls int
}

func (c *countingTool) Name() string               { return c.name }
func (c *countingTool) Description() string        { return "counts calls" }
func (c *countingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (c *countingTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	c.calls++
	return NewToolResult("executed")
}

// Through the registry: the hook receives the real arguments, and a held call
// never reaches Execute.
func TestApproval_RegistryHoldsThenRuns(t *testing.T) {
	store := armApproval(t, NameCreateOrder)
	tool := &countingTool{name: NameCreateOrder}
	reg := NewToolRegistry()
	reg.Register(tool)

	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "alice")

	held := reg.ExecuteWithContext(ctx, NameCreateOrder, orderArgs(1), "telegram", "chat1", nil)
	if tool.calls != 0 {
		t.Fatal("a held call reached Execute")
	}
	if held == nil || !held.IsError {
		t.Fatalf("registry did not hold the call: %+v", held)
	}
	m := approvalCodePattern.FindStringSubmatch(lastSent.last())
	if m == nil {
		t.Fatalf("registry sent no approval code: %q", lastSent.last())
	}

	if _, err := store.Approve(m[1], "telegram", "chat1", "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	reg.ExecuteWithContext(ctx, NameCreateOrder, orderArgs(1), "telegram", "chat1", nil)
	if tool.calls != 1 {
		t.Fatalf("approved call executed %d times, want 1", tool.calls)
	}
}

// confirmCountingTool is a countingTool whose schema may or may not declare a
// boolean "confirm", which is what decides whether confirm=false is a preview.
type confirmCountingTool struct {
	countingTool
	declaresConfirm bool
}

func (c *confirmCountingTool) Parameters() map[string]any {
	props := map[string]any{"amount": map[string]any{"type": "number"}}
	if c.declaresConfirm {
		props["confirm"] = map[string]any{"type": "boolean"}
	}
	return map[string]any{"type": "object", "properties": props}
}

func TestToolDeclaresConfirm(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   bool
	}{
		{"nil schema", nil, false},
		{"no properties", map[string]any{"type": "object"}, false},
		{"other properties only", map[string]any{"properties": map[string]any{"amount": map[string]any{"type": "number"}}}, false},
		{"boolean confirm", map[string]any{"properties": map[string]any{"confirm": map[string]any{"type": "boolean"}}}, true},
		{"non-boolean confirm", map[string]any{"properties": map[string]any{"confirm": map[string]any{"type": "string"}}}, false},
		{"confirm without a type", map[string]any{"properties": map[string]any{"confirm": map[string]any{}}}, false},
	}
	for _, tc := range cases {
		if got := toolDeclaresConfirm(tc.params); got != tc.want {
			t.Errorf("%s: toolDeclaresConfirm = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsPreviewCall(t *testing.T) {
	cases := []struct {
		name            string
		args            map[string]any
		declaresConfirm bool
		want            bool
	}{
		{"declared, confirm false", map[string]any{"confirm": false}, true, true},
		{"declared, confirm true", map[string]any{"confirm": true}, true, false},
		{"declared, confirm absent", map[string]any{}, true, false},
		{"declared, string false", map[string]any{"confirm": "false"}, true, false},
		{"declared, nil confirm", map[string]any{"confirm": nil}, true, false},
		{"undeclared, confirm false", map[string]any{"confirm": false}, false, false},
		{"undeclared, confirm true", map[string]any{"confirm": true}, false, false},
	}
	for _, tc := range cases {
		if got := isPreviewCall(tc.args, tc.declaresConfirm); got != tc.want {
			t.Errorf("%s: isPreviewCall = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A model can add "confirm":false to a tool that has no such parameter -
// argument validation lets undeclared properties through. It must not read as
// a preview, or it would skip the gate on a tool that executes regardless.
func TestApproval_ConfirmFalseOnUndeclaringToolIsStillGated(t *testing.T) {
	armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	args := map[string]any{"amount": 1.0, "confirm": false}
	if refusal := checkToolPolicy(ctx, NameCreateOrder, args, false); refusal == nil {
		t.Fatal("confirm=false on a tool that does not declare confirm skipped approval")
	}
	// Identical to the call with no confirm argument at all: same hold, and an
	// operator-visible code.
	requestCode(t, ctx, NameCreateOrder, args)
	requestCode(t, ctx, NameCreateOrder, map[string]any{"amount": 1.0})
	codes := approvalCodePattern.FindAllStringSubmatch(strings.Join(lastSent.texts, "\n"), -1)
	if len(codes) != 3 || codes[0][1] != codes[1][1] || codes[1][1] != codes[2][1] {
		t.Fatalf("confirm=false and no-confirm calls were not treated as the same call: %q", lastSent.texts)
	}
}

// Through the registry, where "declares confirm" is computed from the
// registered tool: the undeclaring tool never executes.
func TestApproval_RegistryConfirmFalseUndeclaringToolNeverRuns(t *testing.T) {
	armApproval(t, NameCreateOrder)
	tool := &confirmCountingTool{countingTool: countingTool{name: NameCreateOrder}}
	reg := NewToolRegistry()
	reg.Register(tool)
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "alice")

	res := reg.ExecuteWithContext(ctx, NameCreateOrder,
		map[string]any{"amount": 1.0, "confirm": false}, "telegram", "chat1", nil)
	if tool.calls != 0 {
		t.Fatal("confirm=false executed a tool that requires approval and does not declare confirm")
	}
	if res == nil || !res.IsError {
		t.Fatalf("registry did not hold the call: %+v", res)
	}
	if approvalCodePattern.FindStringSubmatch(lastSent.last()) == nil {
		t.Fatalf("operator was not asked to approve: %q", lastSent.last())
	}
}

// The existing preview behaviour for tools that do declare confirm, via the
// registry: confirm=false is not gated, confirm=true is.
func TestApproval_RegistryConfirmFalseDeclaringToolPreviews(t *testing.T) {
	armApproval(t, NameCreateOrder)
	tool := &confirmCountingTool{countingTool: countingTool{name: NameCreateOrder}, declaresConfirm: true}
	reg := NewToolRegistry()
	reg.Register(tool)
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "alice")

	reg.ExecuteWithContext(ctx, NameCreateOrder,
		map[string]any{"amount": 1.0, "confirm": false}, "telegram", "chat1", nil)
	if tool.calls != 1 {
		t.Fatalf("confirm=false preview ran %d times, want 1 (ungated)", tool.calls)
	}
	if len(lastSent.texts) != 0 {
		t.Fatalf("a preview asked the operator for approval: %q", lastSent.texts)
	}

	res := reg.ExecuteWithContext(ctx, NameCreateOrder,
		map[string]any{"amount": 1.0, "confirm": true}, "telegram", "chat1", nil)
	if tool.calls != 1 {
		t.Fatal("confirm=true on a declaring tool ran without approval")
	}
	if res == nil || !res.IsError || len(lastSent.texts) != 1 {
		t.Fatalf("confirm=true was not held and offered for approval: %+v / %q", res, lastSent.texts)
	}
}

// A non-boolean confirm is not a preview even on a declaring tool. Checked at
// the policy layer: the registry's own validation rejects it earlier.
func TestApproval_NonBoolConfirmIsNotAPreview(t *testing.T) {
	armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	args := map[string]any{"amount": 1.0, "confirm": "false"}
	if refusal := checkToolPolicy(ctx, NameCreateOrder, args, true); refusal == nil {
		t.Fatal(`confirm="false" (a string) skipped approval on a declaring tool`)
	}
}
