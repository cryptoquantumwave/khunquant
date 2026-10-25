package intents

import (
	"context"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// MisfireGrace is how late a reminder's timer may be seen and still fire (the clock
// source's own default).
const MisfireGrace = perception.DefaultMisfireGrace

// missedReason is the reason a reminder whose moment passed while nothing ran is
// ended with; list_intents shows it.
func missedReason(due time.Time) string {
	if due.IsZero() {
		return "missed: it was due while the assistant was not running"
	}
	return "missed: was due " + due.UTC().Format(time.RFC3339) + " while the assistant was not running"
}

// ownerActedSince reports whether the owner made the intent active (a resume after a
// pause, or a confirmation) after t. It is how a spec the registry stopped after
// repeated errors is told apart from one the owner has since taken in hand: the
// registry stamps the spec when it stops it, the ledger stamps the owner's act, and
// the later of the two wins. A pause and a resume that fall between two passes are
// seen all the same.
func (c *Compiler) ownerActedSince(ctx context.Context, it intent.Intent, t time.Time) bool {
	evs, err := c.store.Events(ctx, it.ID)
	if err != nil {
		return false // cannot tell: leave the spec off
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].ToStatus == intent.StatusActive {
			return evs[i].At.After(t)
		}
	}
	return false
}

// repairLostFire ends a one-shot intent whose firing happened but whose count never
// reached the ledger (the process died between the trigger committing its state and
// the compiler's write). Without it the intent would stay "active" for ever: its
// trigger is spent and nothing will fire it again. It reports whether it ended the
// intent. It acts only while the registry is live (a rehearsal in shadow is not a
// firing) and only on evidence that is committed:
//
//   - a one-shot watch or standing order (FireBudget 1) or a one-shot reminder whose
//     trigger's committed state says it fired: the attention decision was recorded, so
//     the owner was told;
//   - a one-shot reminder whose timer is gone and whose moment is more than the
//     misfire grace behind: it was missed, and is ended with the reason.
//
// It cannot double count. The count is the intent's own FiresUsed: a one-shot is
// counted at most once whoever writes it (RecordFire is one conditional UPDATE, and
// the queued firing that arrives later finds the intent done or its budget spent and
// is benign), and a reminder that already has its count only needs ending.
func (c *Compiler) repairLostFire(ctx context.Context, it intent.Intent, plan Plan, cur triggers.Spec, live bool, rep *Report) bool {
	// A spec still in shadow only rehearsed: what its state says is not a firing.
	if !live || it.Status != intent.StatusActive || cur.Mode == triggers.ModeShadow {
		return false
	}
	reminder := it.Type == intent.TypeReminder
	oneShot := (reminder && OneShotReminder(it)) || (!reminder && Compiles(it) && it.FireBudget == 1)
	if !oneShot {
		return false
	}

	if c.specs.OneShotFired(ctx, it.ID) {
		var err error
		switch {
		case it.FiresUsed == 0:
			err = c.handleFire(ctx, triggers.Firing{SpecID: it.ID, Kind: plan.Kind, IntentID: it.ID, DedupKey: "repair"})
		case reminder:
			err = c.store.Transition(ctx, it.ID, intent.StatusDone, systemProvenance, "reminder delivered")
			if errors.Is(err, intent.ErrBadTransition) {
				err = nil
			}
		default:
			return false
		}
		if err != nil {
			c.fail(rep, it.ID, "repair a lost firing", err)
			return false
		}
		c.st.repairedFires.Add(1)
		c.Request()
		return true
	}

	if reminder && it.FiresUsed == 0 && !plan.Due.IsZero() && c.now().Sub(plan.Due) > MisfireGrace {
		t := c.timersNow()
		if t == nil {
			return false
		}
		if _, ok := t.Get(it.ID); ok {
			return false // the clock still holds it and will say it was missed
		}
		err := c.store.Transition(ctx, it.ID, intent.StatusDone, systemProvenance, missedReason(plan.Due))
		if err != nil && !errors.Is(err, intent.ErrBadTransition) {
			c.fail(rep, it.ID, "end a missed reminder", err)
			return false
		}
		c.st.repairedFires.Add(1)
		c.st.missed.Add(1)
		c.Request()
		return true
	}
	return false
}
