package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/notify/notifytest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

const ownerID = "123456789"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// notifyDeliverer is the gateway's attention-to-notify adapter, reduced to what the
// tests need: a message delivered by the outbox is sent to the owner by a real
// notify.Service, which records notify_sent.
type notifyDeliverer struct{ svc *notify.Service }

func (d notifyDeliverer) Deliver(ctx context.Context, m attention.Message) (bool, error) {
	res, err := d.svc.Deliver(ctx, notify.Notification{
		Severity: m.Severity, Title: m.Title, Body: m.Body, DedupKey: m.DedupKey, InterventionID: m.InterventionID,
	})
	return err == nil && res.Delivered, err
}

// fixture is the real stack under feedback: attention (governor and outbox) delivering
// through a real notify service on one database, with a fake clock and a recorder.
type fixture struct {
	t      *testing.T
	db     *sql.DB
	clock  *fakeClock
	ob     *attention.Outbox
	notify *notify.Service
	sender *notifytest.Sender
	bus    *events.EventBus
	fb     *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := sqlitextest.Open(t)
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}

	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: ownerID, SenderID: "telegram:" + ownerID}
	cfg.Proactive.Learning.Enabled = true

	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	acfg := config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: 50, DedupWindowHours: 24}
	gov, err := attention.New(db, profiles, acfg, config.ProactiveModeLive, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	sender := &notifytest.Sender{}
	ns, err := notify.New(cfg, sender, nil, db, "", clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close(context.Background()) })

	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	fb, err := New(ctx, db, Options{Events: bus, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	// The recorder's table was created "now" in real time; the tests run on a fake clock.
	fb.since = time.Time{}
	return &fixture{t: t, db: db, clock: clock, ob: attention.NewOutbox(gov, notifyDeliverer{ns}, 0, clock.Now),
		notify: ns, sender: sender, bus: bus, fb: fb}
}

// deliver sends an alert about subject to the owner through the outbox and returns
// the intervention it went out as and the platform message id it carries.
func (f *fixture) deliver(subject, key string, mut ...func(*proactive.Candidate)) (interventionID, platformID string) {
	f.t.Helper()
	c := proactive.Candidate{
		TriggerID: "trig:" + key, DedupKey: key, Severity: proactive.SeverityWarning, Subject: subject, Title: "title " + key,
	}
	for _, m := range mut {
		m(&c)
	}
	ctx := context.Background()
	d, err := f.ob.Submit(ctx, attention.Item{Candidate: c})
	if err != nil {
		f.t.Fatal(err)
	}
	if d.Disposition != proactive.DispNotify {
		f.t.Fatalf("decision = %s/%s, want notify", d.Disposition, d.Reason)
	}
	if err := f.ob.Tick(ctx); err != nil {
		f.t.Fatal(err)
	}
	sent := f.sender.Sent()
	if len(sent) == 0 {
		f.t.Fatal("nothing was delivered")
	}
	last := sent[len(sent)-1]
	if last.InterventionID == "" {
		f.t.Fatal("the delivered message carries no intervention id")
	}
	id, ok, err := f.notify.LookupByPlatformMessageID(ctx, "telegram", fmt.Sprintf("m%d", len(sent)))
	if err != nil || !ok || id != last.InterventionID {
		f.t.Fatalf("notify_sent lookup = %q %v %v, want %q", id, ok, err, last.InterventionID)
	}
	return last.InterventionID, fmt.Sprintf("m%d", len(sent))
}

// kinds returns the kinds recorded for an intervention, in order.
func (f *fixture) kinds(id string) []string {
	f.t.Helper()
	rows, err := f.fb.Responses(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Kind)
	}
	return out
}

func (f *fixture) count() int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM feedback_responses`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
