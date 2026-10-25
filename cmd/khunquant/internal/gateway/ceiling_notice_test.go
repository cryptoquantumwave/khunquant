package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// recordingPublisher is a message bus that remembers what it was asked to send
// and can be told to fail.
type recordingPublisher struct {
	mu   sync.Mutex
	sent []bus.OutboundMessage
	err  error
}

func (p *recordingPublisher) PublishOutbound(_ context.Context, msg bus.OutboundMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, msg)
	return nil
}

func (p *recordingPublisher) messages() []bus.OutboundMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bus.OutboundMessage(nil), p.sent...)
}

func everyHour() cron.CronSchedule {
	ms := int64(3_600_000)
	return cron.CronSchedule{Kind: "every", EveryMS: &ms}
}

// addJob stores a job and applies edit to it, the way an operator's changes
// reach a stored job.
func addJob(t *testing.T, svc *cron.CronService, name, channel, to string, deliver bool, edit func(*cron.CronJob)) string {
	t.Helper()
	job, err := svc.AddJob(name, everyHour(), "do the thing", deliver, channel, to)
	if err != nil {
		t.Fatalf("AddJob %q: %v", name, err)
	}
	if edit != nil {
		edit(job)
		if err := svc.UpdateJob(job); err != nil {
			t.Fatalf("UpdateJob %q: %v", name, err)
		}
	}
	return job.ID
}

// asBeforeUpgrade rewrites the store the way a jobs.json from before this
// version looks - no job carries the notified flag, since AddJob now creates
// jobs already marked - and returns a service over it.
func asBeforeUpgrade(t *testing.T, path string) *cron.CronService {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var store cron.CronStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	for i := range store.Jobs {
		store.Jobs[i].State.NotifiedCeilingV1 = false
	}
	out, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return cron.NewCronService(path, nil)
}

// A job created now is born under the ceiling and is never told.
func TestCeilingNotice_JobAddedAfterUpgradeIsNeverTold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	svc := cron.NewCronService(path, nil)
	id := addJob(t, svc, "new reminder", "telegram", "chat-1", false, nil)
	if !svc.GetJob(id).State.NotifiedCeilingV1 {
		t.Fatal("AddJob did not create the job already notified")
	}
	pub := &recordingPublisher{}
	sendCeilingNotices(pub, svc)
	sendCeilingNotices(pub, cron.NewCronService(path, nil))
	if n := len(pub.messages()); n != 0 {
		t.Fatalf("a job created under the ceiling was told: %+v", pub.messages())
	}
}

// TestCeilingNotice_SentOncePerGenericJob: a generic scheduled agent job is told
// once; jobs that do not run through the model, or that code owns, are not; the
// record survives a restart; and a failed publish is retried.
func TestCeilingNotice_SentOncePerGenericJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	svc := cron.NewCronService(path, nil)

	generic := addJob(t, svc, "morning brief", "telegram", "chat-1", false, nil)
	directive := addJob(t, svc, "weekly review", "telegram", "chat-2", true, func(j *cron.CronJob) {
		j.Payload.Type = "directive" // deliver + directive still goes through the model
	})
	// None of these should be told anything.
	addJob(t, svc, "plain reminder", "telegram", "chat-3", true, nil)
	addJob(t, svc, "nightly backup", "telegram", "chat-4", false, func(j *cron.CronJob) {
		j.Payload.Command = "backup.sh"
	})
	for _, prefixed := range []string{"price_alert:BTC", "indicator_alert:rsi", "dca:7:weekly", "dn:3:monitor"} {
		addJob(t, svc, prefixed, "telegram", "chat-5", false, nil)
	}
	addJob(t, svc, "no target", "", "", false, nil)
	disabled := addJob(t, svc, "paused", "telegram", "chat-6", false, func(j *cron.CronJob) {
		j.Enabled = false
	})

	svc = asBeforeUpgrade(t, path) // all of the above pre-date the ceiling

	pub := &recordingPublisher{}
	sendCeilingNotices(pub, svc)

	want := []bus.OutboundMessage{
		{Channel: "telegram", ChatID: "chat-1", Content: "Scheduled task 'morning brief' now runs with " +
			"read-and-notify tools only: it can no longer trade, change plans or schedules, run commands " +
			"or use MCP tools. For recurring buys, create a DCA plan (ask me \"create a DCA plan\")."},
		{Channel: "telegram", ChatID: "chat-2", Content: ceilingNoticeMessage("weekly review")},
	}
	if got := pub.messages(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %+v\nwant %+v", got, want)
	}

	for _, id := range []string{generic, directive} {
		if !svc.GetJob(id).State.NotifiedCeilingV1 {
			t.Errorf("job %s was told but not marked", id)
		}
	}
	if svc.GetJob(disabled).State.NotifiedCeilingV1 {
		t.Error("a disabled job was marked")
	}

	// A job added after the upgrade is never told, however often we run.
	addJob(t, svc, "added later", "telegram", "chat-9", false, nil)

	// A second pass in the same process sends nothing.
	sendCeilingNotices(pub, svc)
	if n := len(pub.messages()); n != len(want) {
		t.Fatalf("second pass sent %d message(s) in total, want %d", n, len(want))
	}

	// The record is in the file: a fresh service on the same store still knows,
	// and an update made from a stale copy cannot clear it.
	svc2 := cron.NewCronService(path, nil)
	if !svc2.GetJob(generic).State.NotifiedCeilingV1 {
		t.Fatal("the notified flag did not survive a restart")
	}
	stale := svc2.GetJob(generic)
	stale.State.NotifiedCeilingV1 = false
	stale.Payload.Message = "edited"
	if err := svc2.UpdateJob(stale); err != nil {
		t.Fatal(err)
	}
	if !svc2.GetJob(generic).State.NotifiedCeilingV1 {
		t.Fatal("UpdateJob dropped the notified flag")
	}
	sendCeilingNotices(pub, svc2)
	if n := len(pub.messages()); n != len(want) {
		t.Fatalf("after restart %d message(s) in total, want %d", n, len(want))
	}

	// Enabling the paused job later does not tell it until the next start - and
	// then it is told, once.
	if svc2.EnableJob(disabled, true) == nil {
		t.Fatal("enable failed")
	}
	sendCeilingNotices(pub, svc2)
	if n := len(pub.messages()); n != len(want)+1 {
		t.Fatalf("the re-enabled job was not told: %d message(s)", n)
	}
}

// A publish that fails leaves the job unmarked, so the next start tries again.
func TestCeilingNotice_PublishFailureIsRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	svc := cron.NewCronService(path, nil)
	id := addJob(t, svc, "morning brief", "telegram", "chat-1", false, nil)
	svc = asBeforeUpgrade(t, path)

	failing := &recordingPublisher{err: errors.New("bus is full")}
	sendCeilingNotices(failing, svc)
	if svc.GetJob(id).State.NotifiedCeilingV1 {
		t.Fatal("a failed publish was recorded as sent")
	}
	if cron.NewCronService(path, nil).GetJob(id).State.NotifiedCeilingV1 {
		t.Fatal("a failed publish reached the store")
	}

	ok := &recordingPublisher{}
	sendCeilingNotices(ok, cron.NewCronService(path, nil)) // the next start
	if n := len(ok.messages()); n != 1 {
		t.Fatalf("retry sent %d message(s), want 1", n)
	}
	sendCeilingNotices(ok, cron.NewCronService(path, nil))
	if n := len(ok.messages()); n != 1 {
		t.Fatalf("a third start sent another message (%d in total)", n)
	}
}

// The notice goes out through the bus the gateway really uses, wired from
// setupCronTool: the unit tests above prove the rule, this proves the wiring.
func TestSetupCronTool_SendsCeilingNotice(t *testing.T) {
	workspace := t.TempDir()
	storePath := filepath.Join(workspace, "cron", "jobs.json")
	seed := cron.NewCronService(storePath, nil)
	addJob(t, seed, "morning brief", "telegram", "chat-1", false, nil)
	asBeforeUpgrade(t, storePath)

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspace
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	al := agent.NewAgentLoop(cfg, msgBus, noopProvider{})
	defer al.Close()

	setupCronTool(al, msgBus, workspace, true, 0, cfg)

	select {
	case msg := <-msgBus.OutboundChan():
		if msg.Channel != "telegram" || msg.ChatID != "chat-1" || msg.Content != ceilingNoticeMessage("morning brief") {
			t.Fatalf("published %+v", msg)
		}
	default:
		t.Fatal("setupCronTool published no notice")
	}
}

type noopProvider struct{}

func (noopProvider) Chat(context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: "ok"}, nil
}
func (noopProvider) GetDefaultModel() string { return "noop" }

// TestClassOf_AllGatewayRegisteredToolsClassified registers the tools the
// gateway adds (cron, alerts, DCA, delta-neutral) on a real loop with every tool
// switched on, and fails on any the class table does not know. The agent
// package's matching test covers the per-agent tools.
func TestClassOf_AllGatewayRegisteredToolsClassified(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspace
	toolsVal := reflect.ValueOf(&cfg.Tools).Elem()
	toolCfg := reflect.TypeOf(config.ToolConfig{})
	for i := 0; i < toolsVal.NumField(); i++ {
		if f := toolsVal.Field(i); f.Type() == toolCfg {
			f.FieldByName("Enabled").SetBool(true)
		}
	}
	cfg.Tools.Cron.Enabled = true

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	al := agent.NewAgentLoop(cfg, msgBus, noopProvider{})
	defer al.Close()
	before := map[string]bool{}
	for _, n := range al.GetRegistry().GetDefaultAgent().Tools.List() {
		before[n] = true
	}

	setupCronTool(al, msgBus, workspace, true, 0, cfg)

	var added []string
	for _, n := range al.GetRegistry().GetDefaultAgent().Tools.List() {
		if !before[n] {
			added = append(added, n)
		}
	}
	if len(added) < 22 {
		t.Fatalf("setupCronTool registered only %d tool(s): %v", len(added), added)
	}
	for _, n := range added {
		if tools.ClassOf(n) == tools.ClassUnknown {
			t.Errorf("gateway-registered tool %q is unclassified: add it to pkg/tools/classification.go", n)
		}
	}
	for _, must := range []string{tools.NameCron, tools.NameExecuteDCAOrder, tools.NameUnwindDeltaNeutralPosition, tools.NameSetPriceAlert} {
		found := false
		for _, n := range added {
			found = found || n == must
		}
		if !found {
			t.Errorf("fixture is missing %s", must)
		}
	}
}
