package attention

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// fakeDeliverer records messages and answers from fn (delivered by default).
// fn runs without any lock held, so it may call back into the outbox.
type fakeDeliverer struct {
	mu   sync.Mutex
	msgs []Message
	fn   func(ctx context.Context, m Message) (bool, error)
}

func (f *fakeDeliverer) Deliver(ctx context.Context, m Message) (bool, error) {
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, m)
	}
	return true, nil
}

func (f *fakeDeliverer) messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.msgs...)
}

func (f *fakeDeliverer) setFn(fn func(ctx context.Context, m Message) (bool, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

func failing(context.Context, Message) (bool, error) { return false, errors.New("send failed") }

// obEnv is an env with an outbox over a recording deliverer.
type obEnv struct {
	*env
	ob  *Outbox
	del *fakeDeliverer
}

// digestCfg is liveCfg with a 60 s grouping window and the digest on.
func digestCfg() config.AttentionConfig {
	cfg := liveCfg()
	cfg.GroupWindowSeconds = 60
	cfg.Digest = true
	return cfg
}

func newObEnv(t *testing.T) *obEnv { return newObEnvCfg(t, digestCfg()) }

func newObEnvCfg(t *testing.T, cfg config.AttentionConfig) *obEnv {
	t.Helper()
	e := newEnvMode(t, config.ProactiveModeLive, cfg)
	del := &fakeDeliverer{}
	window := time.Duration(cfg.GroupWindowSeconds) * time.Second
	return &obEnv{env: e, ob: NewOutbox(e.g, del, window, e.clock.Now), del: del}
}

func (e *obEnv) submit(c proactive.Candidate) Decision {
	e.t.Helper()
	d, err := e.ob.Submit(context.Background(), Item{Candidate: c})
	if err != nil {
		e.t.Fatalf("Submit(%s): %v", c.DedupKey, err)
	}
	return d
}

func (e *obEnv) tick() {
	e.t.Helper()
	if err := e.ob.Tick(context.Background()); err != nil {
		e.t.Fatalf("Tick: %v", err)
	}
}

// at sets the clock and ticks.
func (e *obEnv) tickAt(t time.Time) {
	e.t.Helper()
	e.clock.Set(t)
	e.tick()
}

// row is the part of an attention_decisions row the outbox tests look at.
type row struct {
	delivered      int
	deliveredAtSet bool
	intervention   string
}

func (e *env) rowOf(id string) row {
	e.t.Helper()
	var (
		r  row
		at sql.NullInt64
		iv sql.NullString
	)
	if err := e.db.QueryRow(`SELECT delivered, delivered_at_ms, intervention_id FROM attention_decisions WHERE id = ?`, id).
		Scan(&r.delivered, &at, &iv); err != nil {
		e.t.Fatalf("row %s: %v", id, err)
	}
	r.deliveredAtSet = at.Valid
	r.intervention = iv.String
	return r
}
