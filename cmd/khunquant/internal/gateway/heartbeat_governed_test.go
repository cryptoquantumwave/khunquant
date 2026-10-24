package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/attention/attentiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

type fakeRunner struct {
	mu    sync.Mutex
	reqs  []agent.ProactiveTurnRequest
	res   agent.ProactiveTurnResult
	err   error
	block func(ctx context.Context) // runs inside the turn, before it answers
}

func (f *fakeRunner) RunProactiveTurn(ctx context.Context, req agent.ProactiveTurnRequest) (agent.ProactiveTurnResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	res, err, block := f.res, f.err, f.block
	f.mu.Unlock()
	if block != nil {
		block(ctx)
	}
	return res, err
}

func (f *fakeRunner) requests() []agent.ProactiveTurnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.ProactiveTurnRequest(nil), f.reqs...)
}

type fakeSubmitter struct {
	mu    sync.Mutex
	items []attention.Item
	err   error
}

func (f *fakeSubmitter) Submit(_ context.Context, it attention.Item) (attention.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, it)
	return attention.Decision{Candidate: it.Candidate}, f.err
}

func (f *fakeSubmitter) submitted() []attention.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]attention.Item(nil), f.items...)
}

// plainLogSink collects the console log with its colour codes removed.
type plainLogSink struct{ syncBuf }

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func (l *plainLogSink) String() string { return ansiEscape.ReplaceAllString(l.syncBuf.String(), "") }

func captureLogs(t *testing.T) *plainLogSink {
	t.Helper()
	sink := &plainLogSink{}
	logger.SetAdditionalWriter(&sink.syncBuf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })
	return sink
}

func runHandler(t *testing.T, r *fakeRunner, s *fakeSubmitter) {
	t.Helper()
	res := governedHeartbeatHandler(r, s, 30*time.Minute, proactive.ModelTierPrimary, nil)(context.Background(), "the prompt", "", "")
	if res == nil || res.IsError {
		t.Fatalf("handler result = %+v", res)
	}
	if !res.Silent {
		t.Fatalf("a governed handler must return a silent result so nothing is sent from the service: %+v", res)
	}
}

func TestHeartbeatGoverned_DecisionsGoThroughAttention(t *testing.T) {
	r := &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{
		{Kind: proactive.DecideSilent, Severity: proactive.SeverityInfo},
		{
			Kind: proactive.DecideNotify, Severity: proactive.SeverityWarning, DedupKey: "sol:drop",
			Text: "SOL is down hard today\nIt is a large share of your crypto book.\nConsider reviewing the stop.",
		},
		{
			Kind: proactive.DecideAsk, Severity: proactive.SeverityNotice, DedupKey: "allocation:drift",
			Text:     "Allocation drifted from target\nEquities are well above plan.",
			Question: "Do you want a rebalance plan?",
		},
		{Kind: proactive.DecideNotify, Severity: proactive.SeverityInfo, Text: "One-line note"},
	}}}
	s := &fakeSubmitter{}
	runHandler(t, r, s)

	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d turns, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Kind != proactive.TurnHeartbeatLegacy || got.Prompt != "the prompt" || got.ForcedSkills != nil ||
		got.Model != proactive.ModelTierPrimary || got.Deadline != 5*time.Minute || got.Candidates != nil {
		t.Fatalf("turn request = %+v", got)
	}

	want := []attention.Item{
		{
			Candidate: proactive.Candidate{
				TriggerID: "heartbeat", DedupKey: "heartbeat:sol:drop", Severity: proactive.SeverityWarning,
				Compose: proactive.ComposeTemplate, Title: "SOL is down hard today",
				Trust: proactive.TrustUntrusted,
			},
			Prose: "It is a large share of your crypto book.\nConsider reviewing the stop.",
		},
		{
			Candidate: proactive.Candidate{
				TriggerID: "heartbeat", DedupKey: "heartbeat:allocation:drift", Severity: proactive.SeverityNotice,
				Compose: proactive.ComposeTemplate, Title: "Allocation drifted from target",
				Trust: proactive.TrustUntrusted,
			},
			// The question stands on its own last line.
			Prose: "Equities are well above plan.\nDo you want a rebalance plan?",
		},
		{
			Candidate: proactive.Candidate{
				TriggerID: "heartbeat", DedupKey: "heartbeat:one-line-note", Severity: proactive.SeverityInfo,
				Compose: proactive.ComposeTemplate, Title: "One-line note",
				Trust: proactive.TrustUntrusted,
			},
		},
	}
	gotItems := s.submitted()
	if !reflect.DeepEqual(gotItems, want) {
		t.Fatalf("submitted items:\n%+v\nwant\n%+v", gotItems, want)
	}
	for _, it := range gotItems {
		c := it.Candidate
		if c.Subject != "" || c.Evidence != nil || c.Protective || c.ID != "" || c.IntentID != "" {
			t.Errorf("candidate carries more than the heartbeat sets: %+v", c)
		}
		if c.Trust == proactive.TrustUserAuthored {
			t.Error("a model-authored candidate must never be user-authored")
		}
	}
}

func TestHeartbeatGoverned_SilentNoDelivery(t *testing.T) {
	s := &fakeSubmitter{}
	r := &fakeRunner{res: agent.ProactiveTurnResult{
		Decisions: []proactive.Decision{{Kind: proactive.DecideSilent, Severity: proactive.SeverityInfo}},
	}}
	runHandler(t, r, s)
	if got := s.submitted(); len(got) != 0 {
		t.Fatalf("a silent heartbeat submitted %+v", got)
	}
}

func TestHeartbeatGoverned_DerivedKeyIsStable(t *testing.T) {
	mk := func(text string) proactive.Decision {
		return proactive.Decision{Kind: proactive.DecideNotify, Severity: proactive.SeverityNotice, Text: text}
	}
	run := func(text string) string {
		s := &fakeSubmitter{}
		runHandler(t, &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{mk(text)}}}, s)
		got := s.submitted()
		if len(got) != 1 {
			t.Fatalf("%d items for %q", len(got), text)
		}
		return got[0].Candidate.DedupKey
	}

	// The first 40 characters decide the key, so a longer tail does not change it.
	head := "SOL fell 12% today; review your stop-loss"
	a := run(head + " and the funding rate is high.")
	b := run(head + " and more text that differs entirely.")
	if a != b {
		t.Fatalf("keys differ for the same first 40 characters: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "heartbeat:") {
		t.Fatalf("key = %q", a)
	}
	if other := run("BTC funding turned negative today, review"); other == a {
		t.Fatalf("different topics share the key %q", a)
	}

	for text, want := range map[string]string{
		"SOL dropped 12% today":                          "heartbeat:sol-dropped-12-today",
		"  Margin!!  call --- risk  ":                    "heartbeat:margin-call-risk",
		"BTC/USDT: stop_loss hit":                        "heartbeat:btc-usdt:-stop_loss-hit",
		"UPPER and lower":                                "heartbeat:upper-and-lower",
		"0123456789012345678901234567890123456789ABCDEF": "heartbeat:0123456789012345678901234567890123456789",
	} {
		if got := run(text); got != want {
			t.Errorf("key for %q = %q, want %q", text, got, want)
		}
	}
}

// Text with nothing in [a-z0-9] must not collapse into one shared key, or
// attention would drop every heartbeat after the first as a repeat.
func TestHeartbeatGoverned_NonLatinTextKeysByHash(t *testing.T) {
	key := func(text string) string {
		it, ok, why := heartbeatItem(proactive.Decision{Kind: proactive.DecideNotify, Text: text})
		if !ok {
			t.Fatalf("no item for %q: %s", text, why)
		}
		return it.Candidate.DedupKey
	}
	a, b := key("ราคา SOL ลดลงมากวันนี้ ควรตรวจสอบ"), key("พอร์ตเสี่ยงสูงเกินเป้าหมายที่ตั้งไว้")
	if a == b {
		t.Fatalf("different Thai topics share the key %q", a)
	}
	if again := key("ราคา SOL ลดลงมากวันนี้ ควรตรวจสอบ"); again != a {
		t.Fatalf("the key is not stable: %q vs %q", a, again)
	}
	for _, k := range []string{key("!!!"), key("ราคา")} {
		if k == "heartbeat:" || strings.HasSuffix(k, ":-") || strings.Contains(k, "--") {
			t.Fatalf("degenerate key %q", k)
		}
	}
}

func TestHeartbeatGoverned_ContractViolationIsDeliveredAndCounted(t *testing.T) {
	logs := captureLogs(t)
	s := &fakeSubmitter{}
	r := &fakeRunner{res: agent.ProactiveTurnResult{
		ContractViolated: true,
		Decisions: []proactive.Decision{{
			Kind: proactive.DecideNotify, Severity: proactive.SeverityNotice,
			Text: "The model wrote free text\nand did not call the tool",
		}},
	}}
	runHandler(t, r, s)

	got := s.submitted()
	if len(got) != 1 || got[0].Candidate.Title != "The model wrote free text" ||
		got[0].Prose != "and did not call the tool" || got[0].Candidate.Severity != proactive.SeverityNotice {
		t.Fatalf("submitted %+v", got)
	}
	out := logs.String()
	if !strings.Contains(out, "free text instead of respond_proactive") {
		t.Errorf("the contract violation was not logged:\n%s", out)
	}
	if !strings.Contains(out, "contract_violated=true") {
		t.Errorf("the summary line does not count the violation:\n%s", out)
	}
}

// The handler asks for exactly the tier it was built with, and says which tier
// that was next to the model that answered.
func TestHeartbeatGoverned_PassesTheTierItWasBuiltWith(t *testing.T) {
	for _, tier := range []proactive.ModelTier{proactive.ModelTierPrimary, proactive.ModelTierLight} {
		logs := captureLogs(t)
		r := &fakeRunner{res: agent.ProactiveTurnResult{Model: "answering-model"}}
		res := governedHeartbeatHandler(r, &fakeSubmitter{}, 30*time.Minute, tier, nil)(
			context.Background(), "p", "", "")
		if res == nil || res.IsError {
			t.Fatalf("tier %q: result %+v", tier, res)
		}
		reqs := r.requests()
		if len(reqs) != 1 || reqs[0].Model != tier {
			t.Fatalf("tier %q: requests %+v", tier, reqs)
		}
		out := logs.String()
		if !strings.Contains(out, "model_tier="+string(tier)) || !strings.Contains(out, "model=answering-model") {
			t.Errorf("tier %q: the summary line does not carry the tier and the model:\n%s", tier, out)
		}
	}
}

// The handler asks for exactly the tool list it was built with (nil: the turn's
// own default), and only for a heartbeat-kind turn.
func TestHeartbeatGoverned_PassesTheToolListItWasBuiltWith(t *testing.T) {
	for name, list := range map[string][]string{
		"default":  nil,
		"explicit": {"get_ticker", "web_search"},
		"all":      {"*"},
	} {
		r := &fakeRunner{}
		res := governedHeartbeatHandler(r, &fakeSubmitter{}, 30*time.Minute, proactive.ModelTierPrimary, list)(
			context.Background(), "p", "", "")
		if res == nil || res.IsError {
			t.Fatalf("%s: result %+v", name, res)
		}
		reqs := r.requests()
		if len(reqs) != 1 || fmt.Sprint(reqs[0].Tools) != fmt.Sprint(list) || reqs[0].Kind != proactive.TurnHeartbeatLegacy {
			t.Fatalf("%s: requests %+v, want tools %v", name, reqs, list)
		}
	}
}

func TestHeartbeatGoverned_TurnErrorDeliversNothing(t *testing.T) {
	logs := captureLogs(t)
	for name, err := range map[string]error{
		"error":   errors.New("provider unavailable"),
		"timeout": context.DeadlineExceeded,
	} {
		s := &fakeSubmitter{}
		r := &fakeRunner{err: err, res: agent.ProactiveTurnResult{
			// Whatever came back alongside an error is not a decision.
			Decisions: []proactive.Decision{{Kind: proactive.DecideNotify, Text: "must not be sent"}},
		}}
		res := governedHeartbeatHandler(r, s, 30*time.Minute, proactive.ModelTierPrimary, nil)(context.Background(), "p", "", "")
		if res == nil || !res.IsError {
			t.Fatalf("%s: result = %+v, want an error result so the service logs it", name, res)
		}
		if got := s.submitted(); len(got) != 0 {
			t.Fatalf("%s: submitted %+v after a failed turn", name, got)
		}
		if n := len(r.requests()); n != 1 {
			t.Fatalf("%s: %d turns in a tick, want exactly 1 (no fallback to the legacy path)", name, n)
		}
	}
	if !strings.Contains(logs.String(), "Governed heartbeat turn failed") {
		t.Errorf("no WARN for a failed turn:\n%s", logs.String())
	}
}

// A decision that attention refuses does not stop the others, and a decision
// with nothing to say is skipped with a log line rather than sent empty.
func TestHeartbeatGoverned_OneRefusalDoesNotStopTheRest(t *testing.T) {
	s := &fakeSubmitter{err: errors.New("governor down")}
	r := &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{
		{Kind: proactive.DecideNotify, Text: "a"},
		{Kind: proactive.DecideNotify, Text: ""},
		{Kind: proactive.DecideNotify, Text: "b"},
	}}}
	runHandler(t, r, s)
	if got := len(s.submitted()); got != 2 {
		t.Fatalf("%d submit attempts, want 2 (the empty decision is skipped)", got)
	}
}

// propose is not in the brief's list; if approvals are on one can come back, and
// it must not be lost.
func TestHeartbeatGoverned_ProposeIsDeliveredAsANotify(t *testing.T) {
	s := &fakeSubmitter{}
	r := &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{
		{Kind: proactive.DecidePropose, Severity: proactive.SeverityNotice, Text: "Trim SOL?\nIt is overweight.", Question: "ignored"},
	}}}
	runHandler(t, r, s)
	got := s.submitted()
	if len(got) != 1 || got[0].Candidate.Title != "Trim SOL?" || got[0].Prose != "It is overweight." {
		t.Fatalf("submitted %+v", got)
	}
}

// When the service stops while a turn is running, the turn's context is
// cancelled and what it decided afterwards is dropped with a log line.
func TestHeartbeatGoverned_LateDecisionsAreDroppedAfterStop(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeSubmitter{}
	r := &fakeRunner{
		res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{
			{Kind: proactive.DecideNotify, Text: "late one"},
			{Kind: proactive.DecideSilent},
			{Kind: proactive.DecideAsk, Text: "late two", Question: "q"},
		}},
		block: func(context.Context) { cancel() }, // the stop arrives as the turn ends
	}
	res := governedHeartbeatHandler(r, s, 30*time.Minute, proactive.ModelTierPrimary, nil)(ctx, "p", "", "")
	if res == nil || res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if got := s.submitted(); len(got) != 0 {
		t.Fatalf("submitted %+v after the context ended", got)
	}
	if !strings.Contains(logs.String(), "dropped=2") {
		t.Errorf("the dropped decisions were not logged:\n%s", logs.String())
	}
}

// Over a real outbox (on the attention fakes, no database) the governed handler
// reaches the governor with exactly the candidate, and nothing reaches the bus or
// the last active chat.
func TestHeartbeatGoverned_OverARealOutbox_NothingOnTheBusNothingToLastChat(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	if err := st.SetLastChannel("telegram:-1009876543210"); err != nil { // a group chat
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- scan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mb := bus.NewMessageBus()
	defer mb.Close()

	gov := &attentiontest.Fake{}
	dlv := &attentiontest.Deliverer{}
	ob := attention.NewOutbox(gov, dlv, 0, nil)
	r := &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{
		{Kind: proactive.DecideNotify, Severity: proactive.SeverityNotice, DedupKey: "sol:drop", Text: "SOL is weak\nwatch it"},
	}}}

	hs := heartbeat.NewHeartbeatService(ws, 30, true, st)
	hs.SetContract(heartbeat.ContractGoverned)
	hs.SetBus(mb)
	var seenChannel, seenChat string
	inner := governedHeartbeatHandler(r, ob, 30*time.Minute, proactive.ModelTierPrimary, nil)
	var seen sync.Mutex
	hs.SetContextHandler(func(ctx context.Context, prompt, ch, chat string) *tools.ToolResult {
		seen.Lock()
		seenChannel, seenChat = ch, chat
		seen.Unlock()
		return inner(ctx, prompt, ch, chat)
	})
	// A first-ever start runs one second after Start, on the real clock: this is
	// the one end-to-end test here that waits for it.
	if err := hs.Start(); err != nil {
		t.Fatal(err)
	}
	defer hs.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for len(gov.Candidates()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat never reached the governor")
		}
		time.Sleep(10 * time.Millisecond)
	}
	seen.Lock()
	defer seen.Unlock()

	if seenChannel != "" || seenChat != "" {
		t.Fatalf("the handler was addressed to (%q, %q), want nothing", seenChannel, seenChat)
	}
	select {
	case m := <-mb.OutboundChan():
		t.Fatalf("published on the outbound bus: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}

	cands := gov.Candidates()
	if len(cands) != 1 || cands[0].TriggerID != "heartbeat" || cands[0].DedupKey != "heartbeat:sol:drop" {
		t.Fatalf("the governor saw %+v", cands)
	}
	// The outbox, not the heartbeat, delivers: flush it.
	if err := ob.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := dlv.Messages()
	if len(msgs) != 1 || msgs[0].Title != "SOL is weak" || !strings.Contains(msgs[0].Body, "watch it") {
		t.Fatalf("delivered %+v", msgs)
	}
}

func TestHeartbeatContract_Selection(t *testing.T) {
	cfgFor := func(mode, attn string, owner bool) *config.Config {
		c := config.DefaultConfig()
		c.Heartbeat.Enabled = true
		c.Proactive.Enabled = true
		if owner {
			c.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
		}
		c.Proactive.Heartbeat.Mode = mode
		c.Proactive.Attention.Mode = attn
		return c
	}
	deliverable := &gatewayServices{Attention: &attention.Outbox{}, attentionRun: &attentionRunner{}}
	const warning = "attention cannot deliver"

	for name, tc := range map[string]struct {
		cfg      *config.Config
		services *gatewayServices
		want     heartbeat.Contract
		warns    bool
	}{
		"legacy":                       {cfgFor("legacy", "live", true), deliverable, heartbeat.ContractLegacy, false},
		"sweep needs perception":       {cfgFor("sweep", "live", true), deliverable, heartbeat.ContractGoverned, false}, // Effective makes it governed
		"governed + live + outbox":     {cfgFor("governed", "live", true), deliverable, heartbeat.ContractGoverned, false},
		"governed + shadow attention":  {cfgFor("governed", "shadow", true), deliverable, heartbeat.ContractLegacy, false},
		"governed + no owner":          {cfgFor("governed", "live", false), deliverable, heartbeat.ContractLegacy, false},
		"governed, outbox missing":     {cfgFor("governed", "live", true), &gatewayServices{}, heartbeat.ContractLegacy, true},
		"governed, outbox not running": {cfgFor("governed", "live", true), &gatewayServices{Attention: &attention.Outbox{}}, heartbeat.ContractLegacy, true},
	} {
		logs := captureLogs(t)
		if got := heartbeatContract(tc.cfg, tc.services); got != tc.want {
			t.Errorf("%s: contract = %s, want %s", name, got, tc.want)
		}
		if has := strings.Contains(logs.String(), warning); has != tc.warns {
			t.Errorf("%s: warning logged = %v, want %v\n%s", name, has, tc.warns, logs.String())
		}
	}
}
