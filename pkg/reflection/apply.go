package reflection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// ProvenanceSuffix is appended to the origin of a builtin spec whose parameters an
// approved reflection changed. The gateway re-asserts builtin specs from the config on
// every start and reload; a spec that carries the suffix is left as it was tuned.
const ProvenanceSuffix = "+reflection"

type (
	// Specs is the trigger registry.
	Specs interface {
		List(ctx context.Context, f triggers.ListFilter) ([]triggers.Spec, error)
		Update(ctx context.Context, s triggers.Spec) error
	}
	// Intents is the intent ledger (*intent.Store).
	Intents interface {
		ListIntents(ctx context.Context, f intent.IntentFilter) ([]intent.Intent, error)
		GetIntent(ctx context.Context, id string) (intent.Intent, error)
		Transition(ctx context.Context, id string, to intent.Status, by intent.Provenance, reason string) error
	}
)

// Muter is attention's mute list (*attention.Outbox).
type Muter interface {
	Mute(ctx context.Context, keyPrefix string, until time.Time, reason string) error
}

// Applier applies an approved reflection proposal through the stores that own the thing
// changed (approval.ReflectionSink). It is the only code in the program that changes a
// store for the reflection, and it is reached only from approval.Service after the
// owner approved exactly that stored change: it re-proves the proposal's hash and
// decision, compares the current value with the one the owner saw, and writes through
// the same paths the owner's own tools use. It never activates or creates an intent,
// never creates a watch and never touches an order, a mandate or the profile.
type Applier struct {
	Specs   Specs
	Intents Intents
	Mute    Muter
	Now     func() time.Time
}

var _ approval.ReflectionSink = (*Applier)(nil)

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", approval.ErrReflectionRefused, fmt.Sprintf(format, args...))
}

// ApplyApprovedReflection implements approval.ReflectionSink. It is idempotent: a change
// already in place is a success, and a change whose starting value is no longer the
// current one is refused instead of overwriting what moved.
func (a *Applier) ApplyApprovedReflection(ctx context.Context, p approval.Proposal) error {
	if (p.Status != approval.StatusExecuting && p.Status != approval.StatusApproved) || p.DecidedBy == "" {
		return errors.New("reflection: the proposal is not approved by the owner")
	}
	switch p.Kind {
	case approval.KindTriggerParam:
		v, err := approval.VerifiedTriggerParam(p)
		if err != nil {
			return err
		}
		return a.setTriggerParam(ctx, p, v)
	case approval.KindIntentCleanup:
		v, err := approval.VerifiedIntentCleanup(p)
		if err != nil {
			return err
		}
		return a.cleanUpIntent(ctx, p, v)
	case approval.KindMute:
		v, err := approval.VerifiedMute(p)
		if err != nil {
			return err
		}
		return a.muteKind(ctx, p, v)
	}
	return fmt.Errorf("reflection: %s is not a reflection proposal", p.Kind)
}

func (a *Applier) setTriggerParam(ctx context.Context, p approval.Proposal, v approval.TriggerParamParams) error {
	specs, err := a.Specs.List(ctx, triggers.ListFilter{Kind: v.Kind})
	if err != nil {
		return err
	}
	for _, s := range specs {
		if s.ID != v.SpecID {
			continue
		}
		cur, ok := triggers.SpecBounds(s)[v.Param]
		switch {
		case triggers.OwnerAlertSpec(s.Kind, s.Origin):
			return refused("trigger %s is an alert the owner set", s.ID)
		case !ok:
			return refused("%s is no longer a tunable parameter of %s", v.Param, s.Kind)
		case cur.Value == v.To:
			return nil // already applied
		case cur.Value != v.From:
			return refused("%s is now %g, not the %g it was proposed from", v.Param, cur.Value, v.From)
		case s.IntentID != "":
			return refused("the trigger now belongs to an intent")
		}
		params, err := triggers.WithParam(s.Kind, s.Params, v.Param, v.To)
		if err != nil {
			return refused("%v", err)
		}
		s.Params = params
		if strings.HasPrefix(s.Origin, "builtin:") && !strings.HasSuffix(s.Origin, ProvenanceSuffix) {
			s.Origin += ProvenanceSuffix
		}
		if err := a.Specs.Update(ctx, s); err != nil {
			if errors.Is(err, triggers.ErrInvalidSpec) || errors.Is(err, triggers.ErrNotFound) {
				return refused("%v", err)
			}
			return err
		}
		logger.InfoCF("reflection", "AUDIT: an approved reflection changed a trigger parameter",
			map[string]any{"source": string(approval.OriginReflection), "proposal": p.Code, "trigger": s.ID, "kind": s.Kind, "param": v.Param, "from": v.From, "to": v.To})
		return nil
	}
	return refused("trigger %s no longer exists", v.SpecID)
}

func (a *Applier) cleanUpIntent(ctx context.Context, p approval.Proposal, v approval.IntentCleanupParams) error {
	cur, err := a.Intents.GetIntent(ctx, v.IntentID)
	if err != nil {
		return refused("intent %s: %v", v.IntentID, err)
	}
	to := intent.StatusPaused
	if v.Action == approval.CleanupCancel {
		to = intent.StatusCancelled
	}
	if cur.Status == to {
		return nil // already applied
	}
	// Only a move the table allows to a state that ends or holds the intent; never to
	// active, and nothing is created.
	if !intent.CanTransition(cur.Status, to) {
		return refused("a %s intent cannot become %s", cur.Status, to)
	}
	by := intent.Provenance{TurnKind: proactive.TurnReflection, Trust: proactive.TrustSystem}
	if err := a.Intents.Transition(ctx, v.IntentID, to, by, "reflection "+p.Code); err != nil {
		if errors.Is(err, intent.ErrBadTransition) {
			return refused("%v", err)
		}
		return err
	}
	logger.InfoCF("reflection", "AUDIT: an approved reflection changed an intent",
		map[string]any{"source": string(approval.OriginReflection), "proposal": p.Code, "intent": v.IntentID, "to": string(to)})
	return nil
}

func (a *Applier) muteKind(ctx context.Context, p approval.Proposal, v approval.MuteParams) error {
	//simclock:wall the default when no clock is injected
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if !strings.HasSuffix(v.Prefix, ":") || strings.Count(v.Prefix, ":") != 1 {
		return refused("a mute covers a whole trigger kind, not %q", v.Prefix) // never one alert
	}
	until := now().Add(time.Duration(v.Days) * 24 * time.Hour)
	if err := a.Mute.Mute(ctx, v.Prefix, until, "reflection "+p.Code); err != nil {
		return err
	}
	logger.InfoCF("reflection", "AUDIT: an approved reflection muted a trigger kind",
		map[string]any{"source": string(approval.OriginReflection), "proposal": p.Code, "prefix": v.Prefix, "days": v.Days})
	return nil
}
