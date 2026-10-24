package policy

import (
	"context"
	"time"
)

// The owner's autonomy dial. It is tighten-only: for the autonomous columns (a
// trusted autonomous turn, and everything else that is not the owner's own root
// turn) the owner may raise gate C's restriction per tier, never lower it. The
// owner's own typed order is never dialled: they are not made to approve it.

// DialColumn names the three columns of gate C for the owner-facing view.
type DialColumn string

// The columns.
const (
	// ColumnOwner is the owner's own root conversation turn: never dialled.
	ColumnOwner DialColumn = "owner"
	// ColumnTrusted is an autonomous turn whose inputs were all exchange data or better.
	ColumnTrusted DialColumn = "trusted_autonomous"
	// ColumnOther is everything else: tainted, untrusted, another person, unknown.
	ColumnOther DialColumn = "other"
)

func (c DialColumn) internal() column {
	switch c {
	case ColumnOwner:
		return colUser
	case ColumnTrusted:
		return colTrusted
	}
	return colTainted
}

// DefaultMode is gate C's mode today (before the owner's dial) for a tier, column and
// whether a mandate covers the action. It is the function gate C itself uses.
func DefaultMode(t Tier, c DialColumn, covered bool) Mode { return baseMode(t, c.internal(), covered) }

// DialMode reads an owner's setting (default, ask, notify, off) as the mode it raises
// gate C to. "default" is ModeAct: the floor, which max-rank leaves alone. ok is false
// for any other word, including "act": the dial has no setting that does more.
func DialMode(setting string) (m Mode, ok bool) {
	switch setting {
	case "", "default":
		return ModeAct, true
	case "ask":
		return ModeAsk, true
	case "notify":
		return ModeNotifyOnly, true
	case "off":
		return ModeDeny, true
	}
	return ModeDeny, false
}

// Tighten is the stricter of two modes. Gate C applies the owner's dial with it, so
// the dial can only raise the restriction.
func Tighten(today, owner Mode) Mode {
	if owner.Rank() > today.Rank() {
		return owner
	}
	return today
}

// ownerDialTimeout bounds the read of the owner's dial.
const ownerDialTimeout = 2 * time.Second

// SetOwnerDial gives the engine the owner's dial: the setting ("default", "ask",
// "notify", "off") for a tier T1..T3. It is asked on every decision that reaches gate
// C on an autonomous turn, so a change takes effect on the next decision. A read error
// is treated as "off" (a dial that cannot be read must not become no dial). Call it
// before the engine is shared; nil (the default) means no dial.
func (e *Engine) SetOwnerDial(f func(ctx context.Context, t Tier) (setting string, err error)) {
	e.ownerDial = f
}

// ownerChoice is the owner's dial resolved for one request.
type ownerChoice struct {
	mode  Mode
	label string // for the reasons
	set   bool
}

// ownerChoice reads the owner's setting for the request's tier. It does I/O, so it
// runs before the engine takes its lock. T0 and T4 are not dialled and the owner's own
// root turn is never dialled.
func (e *Engine) ownerChoice(ctx context.Context, r ActionRequest, t Tier) ownerChoice {
	if e.ownerDial == nil || columnOf(r) == colUser || t < T1Protective || t > T3Discretionary {
		return ownerChoice{}
	}
	ctx, cancel := context.WithTimeout(ctx, ownerDialTimeout)
	defer cancel()
	setting, err := e.ownerDial(ctx, t)
	if err != nil {
		return ownerChoice{mode: ModeDeny, label: "unreadable, treated as off", set: true}
	}
	m, ok := DialMode(setting)
	if !ok {
		return ownerChoice{mode: ModeDeny, label: "unknown, treated as off", set: true}
	}
	if m == ModeAct {
		return ownerChoice{}
	}
	return ownerChoice{mode: m, label: setting, set: true}
}
