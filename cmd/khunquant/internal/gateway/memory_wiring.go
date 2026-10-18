package gateway

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// memoryDigest queues the announcement of a memory suggestion in the attention outbox's
// digest lane (approval.MemoryDigest). Nothing is sent now: attention decides the
// candidate (Info severity, so it is a digest item), and the daily digest carries it,
// with the fact quoted and Save and Discard buttons for the owner.
type memoryDigest struct{ ob *attention.Outbox }

func (m memoryDigest) QueueForDigest(ctx context.Context, c proactive.Candidate) error {
	dec, err := m.ob.Submit(ctx, attention.Item{Candidate: c})
	if err != nil {
		return err
	}
	if dec.Disposition != proactive.DispDigest {
		// Muted, a repeat, shadow mode: the owner would never see it.
		return fmt.Errorf("attention did not queue it for the digest (%s: %s)", dec.Disposition, dec.Reason)
	}
	return nil
}

// memoryDigestFor returns the digest queue for this generation, or nil when the daily
// digest cannot reach the owner: attention is not live, or the digest is switched off.
// With nil, memory suggestions are unavailable (the tool says so) rather than queued
// where nobody would read them.
func memoryDigestFor(cfg *config.Config, ob *attention.Outbox) approval.MemoryDigest {
	eff := cfg.Proactive.Effective()
	if ob == nil || eff.AttentionMode != config.ProactiveModeLive || !cfg.Proactive.Attention.Digest {
		return nil
	}
	return memoryDigest{ob: ob}
}

// digestButtonSource builds the buttons of the suggestions in a digest: memories and the
// changes the nightly reflection proposes (*approval.Service).
type digestButtonSource interface {
	MemoryActions(ctx context.Context, shown []approval.ShownMemory) []bus.Action
	ReflectionActions(ctx context.Context, shown []approval.ShownReflection, max int) []bus.Action
}

// digestButtons is the approval service the attention deliverer asks for the digest's
// buttons; set while approvals run, cleared when they stop.
var digestButtons atomic.Pointer[digestButtonSource]

func setDigestButtons(b digestButtonSource) {
	if b == nil {
		digestButtons.Store(nil)
		return
	}
	digestButtons.Store(&b)
}

// digestActions returns the Save and Discard buttons for the memory suggestions and the
// reflection changes a digest message carries, or none when there are none or approvals are not running.
func digestActions(ctx context.Context, m attention.Message) []bus.Action {
	if !m.Digest {
		return nil
	}
	var shown []approval.ShownMemory
	for _, s := range attention.MemorySuggestions(m.Decisions) {
		shown = append(shown, approval.ShownMemory{Code: s.Code, Fact: s.Fact})
	}
	var changes []approval.ShownReflection
	for _, s := range attention.ReflectionSuggestions(m.Decisions) {
		changes = append(changes, approval.ShownReflection{Code: s.Code, Change: s.Change})
	}
	b := digestButtons.Load()
	if b == nil || len(shown)+len(changes) == 0 {
		return nil
	}
	// A message carries at most bus.MaxActions buttons; memories come first, and a change
	// left without buttons is answered with the /approve and /reject the digest prints.
	var out []bus.Action
	if len(shown) > 0 {
		out = (*b).MemoryActions(ctx, shown)
	}
	if room := bus.MaxActions - len(out); len(changes) > 0 && room >= 2 {
		out = append(out, (*b).ReflectionActions(ctx, changes, room)...)
	}
	return out
}
