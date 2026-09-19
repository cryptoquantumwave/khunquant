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
	armPolicy(t, false, map[string]ToolPolicy{})

	policyMu.Lock()
	prevApproval := policyApproval
	policyMu.Unlock()
	SetRequiredApprovals(names)

	store := NewApprovalStore(DefaultApprovalTTL)
	prevStore := SetApprovalStore(store)

	t.Cleanup(func() {
		SetApprovalStore(prevStore)
		policyMu.Lock()
		policyApproval = prevApproval
		policyMu.Unlock()
	})
	return store
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
	held := checkToolPolicy(ctx, name, args)
	if held == nil {
		t.Fatalf("%s ran without approval", name)
	}
	m := approvalCodePattern.FindStringSubmatch(held.ForUser)
	if m == nil {
		t.Fatalf("held call gave the operator no code: %q", held.ForUser)
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

	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1)); refusal != nil {
		t.Fatalf("approved call was still held: %q", refusal.ForLLM)
	}

	// A grant authorises one call, not a standing permission.
	requestCode(t, ctx, NameCreateOrder, orderArgs(1))
}

// The code must never reach the model: ForLLM is written to session history.
func TestApproval_CodeIsNotInModelContext(t *testing.T) {
	armApproval(t, NameCreateOrder)
	ctx := approvalCtx("telegram", "chat1", "alice")

	held := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1))
	code := requestCode(t, ctx, NameCreateOrder, orderArgs(1))

	if strings.Contains(held.ForLLM, code) {
		t.Fatalf("ForLLM carries the approval code, so the model can read it: %q", held.ForLLM)
	}
	if held.Silent {
		t.Error("held result is Silent, so the operator never sees the code")
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
	if refusal := checkToolPolicy(ctx, NameCreateOrder, preview); refusal != nil {
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
	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1)); refusal != nil {
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
	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1)); refusal == nil {
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
	if refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1)); refusal == nil {
		t.Fatal("an expired grant still let the call run")
	}
}

func TestApproval_NoIdentityRefusesWithoutACode(t *testing.T) {
	armApproval(t, NameCreateOrder)

	ctx := WithToolContext(WithToolOrigin(context.Background(), OriginRemote), "telegram", "chat1")
	refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1))
	if refusal == nil {
		t.Fatal("a call with no sender ran despite requiring approval")
	}
	if approvalCodePattern.MatchString(refusal.ForUser) {
		t.Fatalf("issued a code nobody can ever redeem: %q", refusal.ForUser)
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
	if refusal := checkToolPolicy(ctx, NameFuturesClosePosition, map[string]any{"confirm": true}); refusal != nil {
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
	refusal := checkToolPolicy(ctx, NameCreateOrder, orderArgs(1))
	if refusal == nil {
		t.Fatal("cron call ran")
	}
	if approvalCodePattern.MatchString(refusal.ForUser) {
		t.Fatalf("a cron-blocked call was offered for approval: %q", refusal.ForUser)
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
	m := approvalCodePattern.FindStringSubmatch(held.ForUser)
	if m == nil {
		t.Fatalf("registry returned no approval code: %q", held.ForUser)
	}

	if _, err := store.Approve(m[1], "telegram", "chat1", "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	reg.ExecuteWithContext(ctx, NameCreateOrder, orderArgs(1), "telegram", "chat1", nil)
	if tool.calls != 1 {
		t.Fatalf("approved call executed %d times, want 1", tool.calls)
	}
}
