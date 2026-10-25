package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

type silentNotifier struct{}

func (silentNotifier) Deliver(context.Context, notify.Notification) (notify.Result, error) {
	return notify.Result{}, nil
}

type recordedDigest struct {
	mu sync.Mutex
	cs []proactive.Candidate
}

func (d *recordedDigest) QueueForDigest(_ context.Context, c proactive.Candidate) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cs = append(d.cs, c)
	return nil
}

func promptText(t *testing.T, msgs []providers.Message) string {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMemorySuggestion_FactIsInNoPromptBeforeApproval: a fact a background turn suggested is
// text a model wrote. Until the owner has approved it, it lives in the proposal store and
// in the digest the owner reads, and in no prompt of any mode, and not in the answer the
// suggesting turn gets back. After the owner approves it, it is in MEMORY.md and so in the
// prompts: the positive control that says the absence above means something.
func TestMemorySuggestion_FactIsInNoPromptBeforeApproval(t *testing.T) {
	const marker = "ZXQ-FACT-7731"
	cb, ws := goldenFixture(t)

	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	cfg.Agents.Defaults.Workspace = ws
	tools.SetProposalConfig(cfg)
	t.Cleanup(func() { tools.SetProposalConfig(nil); tools.SetMemoryProposer(nil) })

	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	digest := &recordedDigest{}
	svc, err := approval.New(context.Background(), approval.Options{
		DB: db, Cfg: cfg, Backend: tools.NewApprovalOrderBackend(cfg), Notifier: silentNotifier{},
		Memories: tools.NewMemorySink(ws), Digest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	tools.SetMemoryProposer(svc)

	reg := tools.NewToolRegistry()
	reg.Register(tools.NewRememberTool(ws))
	ctx := tools.WithProactiveTurn(tools.WithToolOrigin(context.Background(), tools.OriginProactive), proactive.TurnProactive)
	res := reg.ExecuteWithContext(ctx, tools.NameRemember,
		map[string]any{"fact": "the owner wants " + marker, "category": "fact"}, "telegram", "chat", nil)
	if res.IsError {
		t.Fatalf("remember: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM+res.ForUser, marker) {
		t.Error("the answer to the suggesting turn repeats the fact")
	}
	if len(digest.cs) != 1 || digest.cs[0].Quote == "" {
		t.Fatalf("the suggestion was not queued for the digest: %+v", digest.cs)
	}

	allModes := map[string]BuildOptions{
		"full":                  {Mode: PromptFull},
		"light":                 {Mode: PromptLight},
		"light, forced skills":  {Mode: PromptLight, ForcedSkills: []string{"alpha"}, MemoryCapBytes: 1 << 20},
		"proactive":             {Mode: PromptProactive},
		"proactive, load_skill": {Mode: PromptProactive, OfferLoadSkill: true},
	}
	prompts := func(cb *ContextBuilder) map[string]string {
		out := map[string]string{}
		for name, o := range allModes {
			out[name] = promptText(t, cb.BuildMessagesWithOptions(nil, "", "hello", nil, "telegram", "chat42", o))
		}
		return out
	}
	for name, text := range prompts(cb) {
		if strings.Contains(text, marker) {
			t.Errorf("the %s prompt carries a fact nobody approved", name)
		}
	}

	code := strings.TrimPrefix(digest.cs[0].TriggerID, proactive.MemoryTriggerPrefix)
	p, err := svc.ByCode(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(context.Background(), code, p.Nonce, true, "telegram:1001"); err != nil {
		t.Fatal(err)
	}
	fresh := NewContextBuilder(ws) // a new builder: the old one caches its static prompt
	for name, text := range prompts(fresh) {
		if !strings.Contains(text, marker) {
			t.Errorf("after approval the %s prompt does not carry the memory: the test above proves nothing", name)
		}
	}
}
