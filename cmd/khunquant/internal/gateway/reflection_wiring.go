package gateway

import (
	"context"
	"database/sql"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/reflection"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// reflectionStopTimeout bounds the wait for the reflection loop to stop.
const reflectionStopTimeout = 10 * time.Second

// triggerRegistry is the runtime's trigger registry, or nil: a nil runtime has none, and
// the nil *Registry must not be wrapped in an interface.
func (rt *perceptionRuntime) triggerRegistry() *triggers.Registry {
	if rt == nil {
		return nil
	}
	return rt.triggers
}

// reflectionSinkFor returns what applies an approved reflection change, or nil while the
// nightly reflection cannot run: it is off (or cognition is), or what a proposal needs
// is missing: the digest that carries it to the owner, the trigger registry, the outbox
// whose mutes it sets, the intent ledger.
func reflectionSinkFor(d assemblyDeps, db *sql.DB, ob *attention.Outbox, reg *triggers.Registry, digest bool) approval.ReflectionSink {
	if !d.Cfg.Proactive.Effective().Reflection {
		return nil
	}
	var missing string
	switch {
	case !digest:
		missing = "the daily digest that would carry its suggestions"
	case reg == nil:
		missing = "the trigger registry"
	case ob == nil:
		missing = "the attention outbox"
	}
	var store *intent.Store
	if missing == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		if store, err = intent.NewStore(ctx, db); err != nil {
			missing = "the intent ledger"
		}
	}
	if missing != "" {
		logger.WarnCF("reflection", "proactive.reflection.enabled is on but "+missing+" is not available: the nightly reflection does not run", nil)
		return nil
	}
	return &reflection.Applier{Specs: reg, Intents: store, Mute: ob}
}

// reflectionRuntime is the daily reflection loop of one generation of services.
type reflectionRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// reflectionTurn adapts the agent loop's turn to reflection.Turn.
type reflectionTurn struct{ al *agent.AgentLoop }

func (t reflectionTurn) RunReflectionTurn(ctx context.Context, input string) (reflection.TurnOutcome, error) {
	r, err := t.al.RunReflectionTurn(ctx, input)
	return reflection.TurnOutcome{Items: r.Items, Called: r.Called}, err
}

// ownerSession adapts the agent loop's session store to reflection.OwnerSession.
type ownerSession struct {
	al  *agent.AgentLoop
	key string
}

func (o ownerSession) Tail(_ context.Context, n int) ([]reflection.Message, string, error) {
	lines, fp := o.al.SessionTail(o.key, n)
	out := make([]reflection.Message, 0, len(lines))
	for _, l := range lines {
		out = append(out, reflection.Message{Role: l.Role, Text: l.Text})
	}
	return out, fp, nil
}

// startReflection starts the daily loop when the reflection can run: it is effectively
// on (so is cognition), the approval service applies its changes (reflectionSinkFor
// found everything it needs) and the services it reads exist. It returns nil otherwise.
// The loop makes one budgeted cognition job a day; it ends with stop.
func startReflection(d assemblyDeps, s *gatewayServices) *reflectionRuntime {
	if s.Approval == nil || !s.Approval.reflect || s.NotifyService == nil || s.Attention == nil {
		return nil
	}
	reg := s.Perception.triggerRegistry()
	if reg == nil {
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := intent.NewStore(ctx, db)
	if err != nil {
		return nil
	}
	svc, err := reflection.New(ctx, reflection.Options{
		DB: db,
		Sources: reflection.Sources{
			Decisions: s.Attention, Specs: reg, Intents: store, Profile: store,
			Session: ownerSession{al: d.AgentLoop, key: s.NotifyService.OwnerSessionKey()},
		},
		Turn:     reflectionTurn{al: d.AgentLoop},
		Proposer: s.Approval.svc,
		AtLocal:  d.Cfg.Proactive.Reflection.AtLocal,
		Zone:     profileZone(d),
		Clock:    d.Shared.clk(),
	})
	if err != nil {
		logger.WarnCF("reflection", "The nightly reflection is not started", map[string]any{"error": err.Error()})
		return nil
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	rt := &reflectionRuntime{cancel: runCancel, done: make(chan struct{})}
	go func() {
		defer close(rt.done)
		svc.Run(runCtx)
	}()
	return rt
}

// stop ends the loop and waits for it. It is safe on a nil runtime.
func (rt *reflectionRuntime) stop() {
	if rt == nil {
		return
	}
	rt.cancel()
	select {
	case <-rt.done:
	case <-time.After(reflectionStopTimeout):
		logger.WarnCF("reflection", "The nightly reflection did not stop in time", nil)
	}
}
