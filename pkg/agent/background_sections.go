package agent

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// A background section is a block of dynamic context that only background turns
// carry: sub-turns and proactive turns (PromptLight and PromptProactive). A user
// turn never gets it, so the full prompt of a user turn is byte-identical whether
// or not one is attached. The first one is the world model's "what changed since
// you last looked" (pkg/worldmodel, wired by the gateway); the agent knows it only
// through FinancialContributor, so pkg/agent does not import it.
//
// With none attached (the default, and perception off) nothing is added, in the
// prompt or in the budget estimate.

// backgroundSectionTimeout bounds one render. A section reads a local database; a
// slow one is dropped for this request, not waited for. It is a variable only so
// that a test need not wait a second.
var backgroundSectionTimeout = time.Second

const (
	// backgroundSectionChars is what the context-usage estimate adds for an attached
	// section: the section's own cap (1,500 characters) and its separator. The
	// estimate does not render it, because rendering reads the database.
	backgroundSectionChars = 1600
)

// turnCompleter is optionally implemented by a background section that wants to
// know when a proactive turn has finished, to move the window it renders. The
// section is only told about a turn that completed; a failed turn leaves it where
// it was.
type turnCompleter interface {
	TurnCompleted(ctx context.Context)
}

type sectionHolder struct{ s FinancialContributor }

// backgroundSections is the one attached section. It belongs to an AgentLoop and
// is shared by the context builders of every agent in that loop's registry, so a
// registry swapped in by a config reload keeps it.
type backgroundSections struct {
	cur atomic.Pointer[sectionHolder]
}

func (b *backgroundSections) attached() bool {
	return b != nil && b.cur.Load() != nil
}

// render returns the section's text, or "" when none is attached, it has nothing
// to say, or it fails (a failure is logged at debug and costs the request nothing).
func (b *backgroundSections) render(ctx context.Context) (out string) {
	if b == nil {
		return ""
	}
	h := b.cur.Load()
	if h == nil || h.s == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			logger.WarnCF("agent", "background section panicked; left out of the prompt",
				map[string]any{"section": h.s.Name(), "panic": r})
			out = ""
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, backgroundSectionTimeout)
	defer cancel()
	section, err := h.s.Section(ctx)
	if err != nil {
		logger.DebugCF("agent", "background section error",
			map[string]any{"section": h.s.Name(), "error": err.Error()})
		return ""
	}
	return section
}

// turnCompleted tells the section a proactive turn finished.
func (b *backgroundSections) turnCompleted(ctx context.Context) {
	if b == nil {
		return
	}
	h := b.cur.Load()
	if h == nil {
		return
	}
	tc, ok := h.s.(turnCompleter)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.WarnCF("agent", "background section panicked on turn completion", map[string]any{"panic": r})
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundSectionTimeout)
	defer cancel()
	tc.TurnCompleted(ctx)
}

// SetBackgroundSection attaches s as the section background turns carry, replacing
// any earlier one; nil detaches. It is safe to call while turns are running and it
// survives a config reload (the agent loop, not the registry, holds it). The
// returned function detaches s only if it is still the attached one, so a stopping
// generation of services cannot remove the next generation's.
//
// The section's Section must be quick (it is cut off after a second), safe to call
// concurrently and read-only. If it also has a TurnCompleted(context.Context)
// method, it is called when a proactive turn has finished successfully.
func (al *AgentLoop) SetBackgroundSection(s FinancialContributor) (detach func()) {
	if al == nil || al.background == nil {
		return func() {}
	}
	if s == nil {
		al.background.cur.Store(nil)
		return func() {}
	}
	h := &sectionHolder{s: s}
	al.background.cur.Store(h)
	return func() { al.background.cur.CompareAndSwap(h, nil) }
}

// SetClock makes the prompt of every agent read now (see ContextBuilder.SetClock) and
// keeps it for the registries a reload installs. A nil now restores the wall clock.
func (al *AgentLoop) SetClock(now func() time.Time) {
	if al == nil {
		return
	}
	if now == nil {
		al.clock.Store(nil)
	} else {
		al.clock.Store(&now)
	}
	al.mu.RLock()
	reg := al.registry
	al.mu.RUnlock()
	if reg == nil {
		return
	}
	for _, id := range reg.ListAgentIDs() {
		if a, ok := reg.GetAgent(id); ok && a != nil && a.ContextBuilder != nil {
			a.ContextBuilder.SetClock(now)
		}
	}
}

// attachBackgroundSections gives the loop's holder to every agent's context
// builder in registry. It runs wherever a registry is installed.
func (al *AgentLoop) attachBackgroundSections(registry *AgentRegistry) {
	if al == nil || al.background == nil || registry == nil {
		return
	}
	clk := al.clock.Load()
	for _, id := range registry.ListAgentIDs() {
		if a, ok := registry.GetAgent(id); ok && a != nil && a.ContextBuilder != nil {
			a.ContextBuilder.WithBackgroundSections(al.background)
			if clk != nil { // a reload builds new instances: they take the loop's clock too
				a.ContextBuilder.SetClock(*clk)
			}
		}
	}
}
