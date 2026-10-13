package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// Trigger modes, ordered off < shadow < live. A spec's mode and the global mode are
// both one of these; the effective mode is the lower of the two.
const (
	ModeOff    = "off"
	ModeShadow = "shadow"
	ModeLive   = "live"
)

// Bounds on everything that outside data or a caller can make grow.
const (
	// MaxParamsBytes bounds a spec's parameters.
	MaxParamsBytes = 4 << 10
	// MaxStateBytes bounds the state one trigger keeps.
	MaxStateBytes = 16 << 10
	// MaxOriginRunes bounds a spec's origin.
	MaxOriginRunes = 128
	// MaxSpecs bounds how many specs the registry holds (and loads).
	MaxSpecs = 500
	// MaxCandidatesPerEvaluation bounds what one trigger may return for one event.
	MaxCandidatesPerEvaluation = 32
	// MaxDemandsPerTrigger bounds what one trigger may ask perception to fetch.
	MaxDemandsPerTrigger = 32
	// MaxEvidenceBytes bounds a candidate's evidence once encoded as JSON.
	MaxEvidenceBytes = 8 << 10
	// MaxTitleBytes, MaxSubjectBytes, MaxDedupKeyBytes and MaxForcedSkills bound the
	// other free-form parts of a candidate.
	MaxTitleBytes    = 500
	MaxSubjectBytes  = 128
	MaxDedupKeyBytes = 200
	MaxForcedSkills  = 8
	// MaxConsecutiveErrors is how many errors in a row switch a spec off.
	MaxConsecutiveErrors = 10

	maxIDLen       = 64
	maxKindLen     = 48
	maxIntentIDLen = 64
	maxSymbolLen   = 128
)

// Errors returned by the registry's mutators.
var (
	// ErrInvalidSpec: the spec is malformed, or its parameters were refused.
	ErrInvalidSpec = errors.New("triggers: invalid spec")
	// ErrUnknownKind: no factory is registered for the spec's kind.
	ErrUnknownKind = errors.New("triggers: unknown trigger kind")
	// ErrNotFound: no spec has that ID.
	ErrNotFound = errors.New("triggers: no such trigger")
	// ErrExists: a spec with that ID already exists.
	ErrExists = errors.New("triggers: trigger already exists")
	// ErrTooMany: the registry holds MaxSpecs specs already.
	ErrTooMany = errors.New("triggers: too many triggers")
)

// Trigger is one typed condition worth noticing.
//
// Evaluate is called for EVERY event of the kinds the trigger lists, whether or not
// the model changed: d.Empty() says nothing changed, and the View holds the current
// facts (the last price, a position's mark and liquidation prices), which can move
// without a Change. A trigger reads what it acts on from the View, not only from the
// Diff.
//
// Evaluate is PURE: it reads the view and the diff and returns candidates. It does
// no network I/O and no database work, reads no clock but v.Now(), draws no random
// number and calls no tool; the same view, diff and state give the same candidates.
// The import boundary of this package keeps it from reaching the agent, the tools,
// a provider or the notify service.
//
// A trigger that acts on a price, a position or a portfolio total MUST say how old
// that fact may be and test it with v.Fresh: a restored fact keeps its old
// timestamp, so without the test a trigger fires on last week's data after a
// restart. Portfolio baselines can be re-based during the day (see
// worldmodel.PortfolioView.DayStartRebased); the model refuses to produce a change
// across a re-base, and a trigger must not compute one from two views.
//
// A trigger sets Candidate.Trust to the lowest trust among the inputs it used
// (proactive.MinTrust; MinInputTrust is a helper). The registry caps it at the
// trust of the event, fills ID, TriggerID and IntentID and the Shadow flag, and
// refuses a malformed candidate (see the rules in validate.go).
type Trigger interface {
	// ID is stable: "<kind>:<spec id>".
	ID() string
	// Kinds are the event kinds the trigger is evaluated for. The registry routes a
	// diff only to triggers that list the kind of its event.
	Kinds() []events.Kind
	// Demands is what perception must fetch for the trigger. It is read once, when
	// the trigger is built, so it must not change.
	Demands() []perception.Demand
	// Evaluate returns the candidates for this event. st is a private copy of the
	// trigger's state: set st.Raw and st.Dirty to change it. The change takes effect
	// only after attention has recorded its decision for every candidate this call
	// returned.
	Evaluate(ctx context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error)
}

// State is the part of a trigger the registry keeps for it between events and
// across restarts: which side of a line it is on, which tier it last fired. The
// registry never reads Raw; it only stores it, capped at MaxStateBytes and valid
// JSON. Dirty means Raw was changed and has to be saved.
type State struct {
	Raw   json.RawMessage
	Dirty bool
	// TurnOff asks the registry to switch the spec off (mode "off") once attention
	// has recorded its decision for every candidate this evaluation returned: a
	// one-shot trigger that has done its job. Like a change of Raw it is held back
	// until then, so a refused or crashed delivery leaves the spec on and the alert
	// fires again. It takes effect only for a spec whose effective mode is live: a
	// shadow rehearsal never uses a one-shot up.
	TurnOff bool
}

// Spec is the stored definition of one trigger.
type Spec struct {
	// ID is a ULID; Add generates it when it is empty.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Params are the kind's parameters, at most MaxParamsBytes of JSON that the
	// kind's factory accepts.
	Params json.RawMessage `json:"params,omitempty"`
	// IntentID is set when the trigger was compiled from an intent.
	IntentID string `json:"intent_id,omitempty"`
	// Mode is "off", "shadow" or "live". The effective mode is the lower of this and
	// the global triggers mode.
	Mode string `json:"mode"`
	// Origin says who made the spec: "tool:set_price_alert",
	// "migration:cron:<jobid>", "builtin", "intent".
	Origin    string    `json:"origin"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Factory builds the trigger for a spec of its kind. It must validate the spec's
// parameters and return an error for any it does not accept. It is pure: it does not
// touch the network or a database.
type Factory func(spec Spec) (Trigger, error)

// ListFilter narrows List. The zero value lists everything.
type ListFilter struct {
	Kind     string // exact
	Mode     string // exact
	IntentID string // exact
	Origin   string // prefix
}

// MinInputTrust is the lowest trust among the event of d, every change and every
// signal it carries, and extra. A trigger that cannot say which of the diff's
// inputs it used can pass this as the candidate's trust: it errs on the side of
// less trust.
func MinInputTrust(d worldmodel.Diff, extra ...proactive.Trust) proactive.Trust {
	m := d.Event.TrustLevel()
	for _, c := range d.Changes {
		m = proactive.MinTrust(m, c.Trust)
	}
	for _, s := range d.Signals {
		m = proactive.MinTrust(m, s.Trust)
	}
	return proactive.MinTrust(append([]proactive.Trust{m}, extra...)...)
}
