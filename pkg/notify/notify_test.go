package notify_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/notify/notifytest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
)

const (
	ownerChat   = "424242"
	ownerSender = "telegram:424242"
	validID     = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
)

func ownerConfig(enabled bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = enabled
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: ownerChat, SenderID: ownerSender}
	return cfg
}

type fixture struct {
	svc      *notify.Service
	sender   *notifytest.Sender
	appender *notifytest.Appender
	db       *sql.DB
	cfg      *config.Config
}

func newFixture(t *testing.T, cfg *config.Config) *fixture {
	t.Helper()
	f := &fixture{
		sender:   &notifytest.Sender{},
		appender: &notifytest.Appender{},
		db:       sqlitextest.Open(t),
		cfg:      cfg,
	}
	svc, err := notify.New(cfg, f.sender, f.appender, f.db, "main", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	f.svc = svc
	return f
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notify_sent`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeliver_UsesOwnerRouteNeverLastChannel(t *testing.T) {
	f := newFixture(t, ownerConfig(true))

	// A state manager whose "last channel" is some other chat. Nothing in the
	// notify package may read it, so nothing may be sent there.
	sm := state.NewManager(t.TempDir())
	if err := sm.SetLastChannel("telegram:-1001234567890"); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetLastChatID("-1001234567890"); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Deliver(context.Background(), notify.Notification{
		Severity: proactive.SeverityNotice, Title: "T", Body: "B", InterventionID: validID,
	})
	if err != nil || !res.Delivered {
		t.Fatalf("Deliver = %+v, %v", res, err)
	}
	sent := f.sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	if sent[0].Channel != "telegram" || sent[0].ChatID != ownerChat {
		t.Fatalf("sent to %s:%s, want telegram:%s", sent[0].Channel, sent[0].ChatID, ownerChat)
	}
	if sent[0].InterventionID != validID || sent[0].Severity != proactive.SeverityNotice {
		t.Fatalf("metadata not carried: %+v", sent[0])
	}
	if sent[0].Content != "T\n\nB" {
		t.Fatalf("content = %q", sent[0].Content)
	}
	if res.Route.ChatID != ownerChat {
		t.Fatalf("route = %+v", res.Route)
	}
}

func TestDeliver_Disabled(t *testing.T) {
	f := newFixture(t, ownerConfig(false))
	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered || res.Reason != "disabled" {
		t.Fatalf("res = %+v", res)
	}
	if len(f.sender.Sent()) != 0 {
		t.Fatal("something was sent while disabled")
	}
	if _, ok := f.svc.OwnerRoute(); ok {
		t.Fatal("OwnerRoute ok while disabled")
	}
	if f.svc.OwnerSessionKey() != "" {
		t.Fatal("OwnerSessionKey set while disabled")
	}
}

func TestDeliver_NoRoute(t *testing.T) {
	cfg := ownerConfig(true)
	cfg.Proactive.Owner = config.OwnerConfig{}
	sender := &notifytest.Sender{}
	svc, err := notify.New(cfg, sender, &notifytest.Appender{}, sqlitextest.Open(t), "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())

	for i := 0; i < 3; i++ {
		res, err := svc.Deliver(context.Background(), notify.Notification{Title: "T"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Delivered || res.Reason != "no-route" {
			t.Fatalf("res = %+v", res)
		}
	}
	if len(sender.Sent()) != 0 {
		t.Fatal("sent without an owner")
	}
}

func TestDeliver_WritesOwnerSessionAfterSend(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	res, err := f.svc.Deliver(context.Background(), notify.Notification{
		Severity: proactive.SeverityWarning, Title: "Margin low", Body: "ratio 1.4", InterventionID: validID,
	})
	if err != nil || !res.Delivered {
		t.Fatalf("Deliver = %+v, %v", res, err)
	}
	waitFor(t, "session write", func() bool { return len(f.appender.Appends()) == 1 })
	got := f.appender.Appends()[0]
	want := "[proactive · warning · " + validID + "]\nMargin low\nratio 1.4"
	if got.Content != want {
		t.Fatalf("content = %q, want %q", got.Content, want)
	}
	if got.SessionKey != f.svc.OwnerSessionKey() || got.SessionKey == "" {
		t.Fatalf("session key = %q, owner key %q", got.SessionKey, f.svc.OwnerSessionKey())
	}
	if res.SessionKey != got.SessionKey {
		t.Fatalf("result session key %q != %q", res.SessionKey, got.SessionKey)
	}
}

func TestDeliver_WriteToSessionFalse(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	no := false
	if _, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T", WriteToSession: &no}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.appender.Appends()); n != 0 {
		t.Fatalf("%d session writes, want 0", n)
	}
}

func TestDeliver_SendFailure_NoSessionWrite(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.sender.Err = fmt.Errorf("telegram: chat %s blocked token=SECRET: %w", ownerChat, channels.ErrSendFailed)

	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T", InterventionID: validID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered || !strings.HasPrefix(res.Reason, "send-failed: ") {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(res.Reason, "SECRET") || strings.Contains(res.Reason, ownerChat) {
		t.Fatalf("reason leaks detail: %q", res.Reason)
	}
	if err := f.svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.appender.Appends()); n != 0 {
		t.Fatalf("%d session writes after a failed send", n)
	}
	if n := countRows(t, f.db); n != 0 {
		t.Fatalf("%d notify_sent rows after a failed send", n)
	}
}

func TestDeliver_PartialDeliveryIsDelivery(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.sender.IDs = []string{"500"}
	f.sender.Err = fmt.Errorf("chunk 2 failed token=SECRET: %w", channels.ErrTemporary)

	res, err := f.svc.Deliver(context.Background(), notify.Notification{
		Severity: proactive.SeverityNotice, Title: "T", Body: "B", InterventionID: validID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delivered || !strings.HasPrefix(res.Reason, "partial: ") || strings.Contains(res.Reason, "SECRET") {
		t.Fatalf("res = %+v", res)
	}
	if len(res.PlatformMessageIDs) != 1 || res.PlatformMessageIDs[0] != "500" {
		t.Fatalf("ids = %v", res.PlatformMessageIDs)
	}
	// Recorded, so a reply to the part the owner saw correlates.
	if got, ok, _ := f.svc.LookupByPlatformMessageID(context.Background(), "telegram", "500"); !ok || got != validID {
		t.Fatalf("lookup = %q, %v", got, ok)
	}
	// The session copy is the full text plus the partial marker.
	waitFor(t, "session write", func() bool { return len(f.appender.Appends()) == 1 })
	want := "[proactive · notice · " + validID + "]\nT\nB\n[delivery was partial]"
	if got := f.appender.Appends()[0].Content; got != want {
		t.Fatalf("session copy = %q, want %q", got, want)
	}
}

func TestDeliver_ErrorWithNoIDsIsStillSendFailed(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.sender.IDs = []string{} // a channel that reports no IDs
	f.sender.Err = channels.ErrTemporary
	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T", InterventionID: validID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered || !strings.HasPrefix(res.Reason, "send-failed: ") {
		t.Fatalf("res = %+v", res)
	}
	if err := f.svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.appender.Appends()) != 0 || countRows(t, f.db) != 0 {
		t.Fatal("a failed send wrote a session message or a row")
	}
}

func TestDeliver_DBErrorAfterSendStillDelivered(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	if _, err := f.db.Exec(`DROP TABLE notify_sent`); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delivered || res.Reason != "" {
		t.Fatalf("res = %+v, want delivered", res)
	}
	waitFor(t, "session write", func() bool { return len(f.appender.Appends()) == 1 })
}

func TestDeliver_NoPlatformIDsStillRecorded(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.sender.IDs = []string{}
	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T", InterventionID: validID})
	if err != nil || !res.Delivered {
		t.Fatalf("Deliver = %+v, %v", res, err)
	}
	var id, chat string
	if err := f.db.QueryRow(`SELECT platform_message_id, chat_id FROM notify_sent WHERE intervention_id = ?`, validID).
		Scan(&id, &chat); err != nil {
		t.Fatal(err)
	}
	if id != "nomsgid:"+validID || chat != ownerChat {
		t.Fatalf("row = %q, %q", id, chat)
	}
}

func TestDeliver_WriteBackQueueFull_DropsWriteBackNotDelivery(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.appender.Block = make(chan struct{}) // the session is busy
	defer close(f.appender.Block)

	const total = 64 + 8 // queue size plus the one in flight, plus overflow
	for i := 0; i < total; i++ {
		res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: fmt.Sprintf("n%d", i)})
		if err != nil || !res.Delivered {
			t.Fatalf("Deliver %d = %+v, %v", i, res, err)
		}
	}
	if got := len(f.sender.Sent()); got != total {
		t.Fatalf("sent %d, want %d", got, total)
	}
	if got := countRows(t, f.db); got != total {
		t.Fatalf("%d rows, want %d", got, total)
	}
	if f.svc.DroppedWriteBacks() < 1 {
		t.Fatal("no write-back was counted as dropped")
	}
}

func TestDeliver_DoesNotWaitForBusySession(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.appender.Block = make(chan struct{})

	done := make(chan notify.Result, 1)
	go func() {
		res, _ := f.svc.Deliver(context.Background(), notify.Notification{Title: "T"})
		done <- res
	}()
	select {
	case res := <-done:
		if !res.Delivered {
			t.Fatalf("res = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Deliver waited for the busy session")
	}
	if n := len(f.appender.Appends()); n != 0 {
		t.Fatalf("write landed while the session was busy (%d)", n)
	}
	close(f.appender.Block)
	waitFor(t, "write after release", func() bool { return len(f.appender.Appends()) == 1 })
}

func TestClose_DrainsAndStops(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.appender.Block = make(chan struct{})
	for i := 0; i < 5; i++ {
		if _, err := f.svc.Deliver(context.Background(), notify.Notification{Title: fmt.Sprintf("n%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	go close(f.appender.Block) // the session frees up while Close is draining
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(f.appender.Appends()); n != 5 {
		t.Fatalf("drained %d writes, want 5", n)
	}
	// After Close nothing more is sent, and a second Close is harmless.
	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "late"})
	if err != nil || res.Delivered || res.Reason != "closed" {
		t.Fatalf("late Deliver = %+v, %v", res, err)
	}
	if err := f.svc.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClose_DeadlineCancelsStuckWrite(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.appender.Block = make(chan struct{}) // never released
	if _, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := f.svc.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("Close did not return promptly")
	}
}

func TestLookupByPlatformMessageID(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	f.sender.IDs = []string{"100", "101"}
	if _, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T", InterventionID: validID}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"100", "101"} {
		got, ok, err := f.svc.LookupByPlatformMessageID(context.Background(), "Telegram", id)
		if err != nil || !ok || got != validID {
			t.Fatalf("lookup %s = %q, %v, %v", id, got, ok, err)
		}
	}
	if _, ok, err := f.svc.LookupByPlatformMessageID(context.Background(), "telegram", "999"); err != nil || ok {
		t.Fatalf("unknown id = %v, %v", ok, err)
	}
	if _, ok, _ := f.svc.LookupByPlatformMessageID(context.Background(), "discord", "100"); ok {
		t.Fatal("lookup crossed channels")
	}

	// Re-sending an ID re-points the row instead of failing the delivery.
	other := "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	res, err := f.svc.Deliver(context.Background(), notify.Notification{Title: "T2", InterventionID: other})
	if err != nil || !res.Delivered {
		t.Fatalf("redelivery = %+v, %v", res, err)
	}
	if got, _, _ := f.svc.LookupByPlatformMessageID(context.Background(), "telegram", "100"); got != other {
		t.Fatalf("after upsert = %q, want %q", got, other)
	}
}

func TestNotifySent_PrunesOld(t *testing.T) {
	db := sqlitextest.Open(t)
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	first, err := notify.New(ownerConfig(true), &notifytest.Sender{}, nil, db, "main", clock)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close(context.Background())

	old := now.Add(-91 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-89 * 24 * time.Hour).UnixMilli()
	for id, at := range map[string]int64{"old": old, "recent": recent} {
		if _, err := db.Exec(`INSERT INTO notify_sent VALUES (?, 'telegram', 'c', ?, ?)`, "I"+id, id, at); err != nil {
			t.Fatal(err)
		}
	}
	second, err := notify.New(ownerConfig(true), &notifytest.Sender{}, nil, db, "main", clock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	if _, ok, _ := second.LookupByPlatformMessageID(context.Background(), "telegram", "old"); ok {
		t.Fatal("91-day-old row survived")
	}
	if _, ok, _ := second.LookupByPlatformMessageID(context.Background(), "telegram", "recent"); !ok {
		t.Fatal("89-day-old row was pruned")
	}
}

func TestDeliver_EmptyNotificationIsAnErrorAndSendsNothing(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	_, err := f.svc.Deliver(context.Background(), notify.Notification{Title: " \n\t", Body: "\x00\x07"})
	if err == nil {
		t.Fatal("want an error")
	}
	if len(f.sender.Sent()) != 0 {
		t.Fatal("sent an empty message")
	}
}

func TestDeliver_ReplacesBadInterventionID(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	res, err := f.svc.Deliver(context.Background(), notify.Notification{
		Title: "T", InterventionID: "x]\n[proactive · critical · FORGED",
	})
	if err != nil || !res.Delivered {
		t.Fatal(res, err)
	}
	if strings.ContainsAny(res.InterventionID, "[]\n ") || len(res.InterventionID) != 26 {
		t.Fatalf("bad ID kept: %q", res.InterventionID)
	}
	if _, ok := proactive.IDTime(res.InterventionID); !ok {
		t.Fatalf("replacement %q is not a ULID", res.InterventionID)
	}
}

func TestDeliver_PassesMetadataAndActions(t *testing.T) {
	f := newFixture(t, ownerConfig(true))
	acts := []bus.Action{{ID: "approve", Label: "Approve", Payload: "p", TextCommand: "/approve X"}}
	if _, err := f.svc.Deliver(context.Background(), notify.Notification{
		Title: "T", Actions: acts, DedupKey: "k", DisableNotification: true, Severity: proactive.SeverityInfo,
	}); err != nil {
		t.Fatal(err)
	}
	m := f.sender.Sent()[0]
	if len(m.Actions) != 1 || m.DedupKey != "k" || !m.DisableNotification || m.InterventionID == "" {
		t.Fatalf("msg = %+v", m)
	}
}

// ownerKeyFor builds the key an inbound DM from the owner gets, through the
// real resolver, the way the agent loop does for a Telegram direct message.
func inboundDMKey(cfg *config.Config, channel, peerID string) string {
	return routing.NewRouteResolver(cfg).ResolveRoute(routing.RouteInput{
		Channel: channel,
		Peer:    &routing.RoutePeer{Kind: "direct", ID: peerID},
	}).SessionKey
}

func TestOwnerSessionKey_MatchesInboundRouting(t *testing.T) {
	links := map[string][]string{"alice": {"telegram:424242", "discord:99"}}
	for _, scope := range []string{"", "main", "per-peer", "per-channel-peer", "per-account-channel-peer"} {
		for _, withLinks := range []bool{false, true} {
			name := fmt.Sprintf("scope=%q links=%v", scope, withLinks)
			t.Run(name, func(t *testing.T) {
				cfg := ownerConfig(true)
				cfg.Session.DMScope = scope
				if withLinks {
					cfg.Session.IdentityLinks = links
				}
				svc, err := notify.New(cfg, &notifytest.Sender{}, nil, sqlitextest.Open(t), "main", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer svc.Close(context.Background())
				want := inboundDMKey(cfg, "telegram", "424242")
				if got := svc.OwnerSessionKey(); got != want || got == "" {
					t.Fatalf("OwnerSessionKey = %q, inbound = %q", got, want)
				}
			})
		}
	}
}

func TestOwnerSessionKey_FollowsBindings(t *testing.T) {
	cfg := ownerConfig(true)
	cfg.Session.DMScope = "per-peer"
	cfg.Agents.List = []config.AgentConfig{{ID: "main", Default: true}, {ID: "trader"}}
	cfg.Bindings = []config.AgentBinding{{
		AgentID: "trader",
		Match:   config.BindingMatch{Channel: "telegram", Peer: &config.PeerMatch{Kind: "direct", ID: "424242"}},
	}}
	svc, err := notify.New(cfg, &notifytest.Sender{}, nil, sqlitextest.Open(t), "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())
	want := inboundDMKey(cfg, "telegram", "424242")
	if !strings.HasPrefix(want, "agent:trader:") {
		t.Fatalf("test setup: inbound key %q does not use the bound agent", want)
	}
	if got := svc.OwnerSessionKey(); got != want {
		t.Fatalf("OwnerSessionKey = %q, inbound = %q", got, want)
	}
}

func TestNotify_NoCrossPackageCallsInTx(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/notify")
}

// A failing session write must not put the owner's identifiers (which the real
// error text carries, as part of the session file path) into the log above
// DEBUG.
func TestDeliver_WriteBackFailureDoesNotLogOwnerID(t *testing.T) {
	var buf syncBuffer
	logger.SetAdditionalWriter(&buf)
	defer logger.SetAdditionalWriter(nil)

	f := newFixture(t, ownerConfig(true))
	f.appender.Err = fmt.Errorf("rename /ws/sessions/agent_main_telegram_direct_%s.json: no space left", ownerChat)
	if _, err := f.svc.Deliver(context.Background(), notify.Notification{
		Title: "T", Body: "secret-body", DedupKey: "secret-dedup",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed write-back", func() bool { return f.svc.FailedWriteBacks() == 1 })

	out := buf.String()
	for _, secret := range []string{ownerChat, "secret-body", "secret-dedup"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log output contains %q:\n%s", secret, out)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
