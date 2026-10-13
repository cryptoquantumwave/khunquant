package intent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// An intent is what the owner wants the assistant to keep doing ("watch SOL
// below 120", "remind me Friday", "keep crypto under 30%"), stored as typed data
// instead of chat history. The ledger (ledger.go) is the only writer.
//
// The security-relevant rule is who can make one ACTIVE: only the owner, in a
// conversation, saying so. Everything else is a suggestion (StatusProposed): it
// is kept, listed to the owner who asks, counted in the digest, and does nothing.
// The decision is made where the turn is known (pkg/tools); the ledger enforces a
// floor under it, so that no caller can store an active intent that is not
// explicit and user-authored (see Store.CreateIntent).

// IntentType says what an intent is for.
type IntentType string

const (
	TypeGoal          IntentType = "goal"
	TypeStandingOrder IntentType = "standing_order"
	TypeWatch         IntentType = "watch"
	TypeReminder      IntentType = "reminder"
	TypePreference    IntentType = "preference"
)

// IntentTypes lists every type, in the order tools offer them.
func IntentTypes() []IntentType {
	return []IntentType{TypeGoal, TypeStandingOrder, TypeWatch, TypeReminder, TypePreference}
}

// Valid reports whether t is a known type.
func (t IntentType) Valid() bool {
	for _, k := range IntentTypes() {
		if t == k {
			return true
		}
	}
	return false
}

// Status is where an intent is in its life.
type Status string

const (
	StatusProposed  Status = "proposed"
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
)

// Statuses lists every status.
func Statuses() []Status {
	return []Status{StatusProposed, StatusActive, StatusPaused, StatusDone, StatusCancelled, StatusExpired}
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, k := range Statuses() {
		if s == k {
			return true
		}
	}
	return false
}

// Open reports whether an intent in this status can still change: it is proposed,
// active or paused. Done, cancelled and expired are terminal.
func (s Status) Open() bool {
	return s == StatusProposed || s == StatusActive || s == StatusPaused
}

// Source says how an intent came to exist.
type Source string

const (
	// SourceExplicit: the owner asked for it, in a conversation.
	SourceExplicit Source = "explicit"
	// SourceInferred: a model, or a person who is not the owner, suggested it.
	SourceInferred Source = "inferred"
	// SourceReflection: the end-of-day reflection suggested it (a later change).
	SourceReflection Source = "reflection"
)

// Valid reports whether s is a known source.
func (s Source) Valid() bool {
	return s == SourceExplicit || s == SourceInferred || s == SourceReflection
}

// TriggerSpec names the trigger that watches for an intent and its parameters.
// The kind is checked here by format only; it is validated against the trigger
// registry when intents are compiled into triggers (a later change).
type TriggerSpec struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Expectation is what a goal expects the world to look like (a later change
// evaluates it). It is stored and bounded here; no tool sets it yet.
type Expectation struct {
	Expr        string             `json:"expr"`
	Description string             `json:"description,omitempty"`
	Severity    proactive.Severity `json:"severity"`
}

// Provenance records who and what created an intent, or changed its status. It is
// filled by the tool from the turn's context, never from tool arguments.
type Provenance struct {
	SessionKey string             `json:"session_key,omitempty"`
	MessageID  string             `json:"message_id,omitempty"`
	SenderID   string             `json:"sender_id,omitempty"`
	Trust      proactive.Trust    `json:"trust"`
	TurnKind   proactive.TurnKind `json:"turn_kind"`
}

// Intent is one stored intent. Rows are never deleted: an intent that ends is
// marked (InvalidAt, SupersededBy) and stays as history.
type Intent struct {
	ID    string
	Type  IntentType
	Title string // one line, at most MaxTitleRunes; shown in digests
	// Params is type-specific JSON (a goal's target, a reminder's text), at most
	// MaxParamsBytes.
	Params json.RawMessage
	// Trigger is compiled into a trigger by a later change; nothing reads it yet.
	Trigger  *TriggerSpec
	Expected *Expectation
	// MandateID is always empty in this change: no mandate exists to point at.
	MandateID  string
	Status     Status
	Source     Source
	Provenance Provenance

	// FireBudget is how many times the intent may fire; 0 is unlimited. When
	// FiresUsed reaches it the intent is done.
	FireBudget int
	FiresUsed  int
	// Cooldown is the least time between two fires (enforced when intents fire).
	Cooldown time.Duration
	// Lifetime is how long the intent stays in force once ACTIVE (0 = until the
	// owner ends it). ExpiresAt is derived from it by the ledger: while proposed it
	// is the proposal's own expiry (at most ProposalTTL), and activation restarts
	// the clock.
	Lifetime  time.Duration
	ExpiresAt *time.Time
	ValidFrom time.Time
	// InvalidAt is set when the intent ends (done, cancelled, expired, superseded).
	InvalidAt    *time.Time
	SupersededBy string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IntentFilter selects intents for ListIntents and CountIntents. Statuses are
// matched against the EFFECTIVE status (an intent past its ExpiresAt reads as
// expired whether or not the sweeper has run). The zero filter selects all.
type IntentFilter struct {
	Statuses []Status
	Types    []IntentType
	// Limit caps the rows returned; values outside 1..MaxListLimit mean MaxListLimit.
	Limit int
	// NewestFirst orders the rows by creation, newest first (the default is oldest
	// first). With a Limit it keeps the newest rows rather than the oldest.
	NewestFirst bool
}

// IntentEvent is one row of an intent's history: its creation and every status
// change, with who made it.
type IntentEvent struct {
	ID         int64
	IntentID   string
	At         time.Time
	FromStatus Status // empty for a creation
	ToStatus   Status
	By         string
	Reason     string
}

// Bounds. Every one is enforced by the ledger, whoever calls it.
const (
	// MaxTitleRunes bounds Title.
	MaxTitleRunes = 120
	// MaxParamsBytes bounds Params and Trigger.Params, as compact JSON.
	MaxParamsBytes = 4 << 10
	// MaxFireBudget bounds FireBudget.
	MaxFireBudget = 1000
	// MaxCooldown bounds Cooldown.
	MaxCooldown = 30 * 24 * time.Hour
	// MinLifetime and MaxLifetime bound a non-zero Lifetime.
	MinLifetime = time.Minute
	MaxLifetime = 365 * 24 * time.Hour
	// ProposalTTL is how long a proposed intent waits for the owner before it
	// expires.
	ProposalTTL = 14 * 24 * time.Hour
	// MaxOpenIntents bounds the intents that are proposed, active or paused.
	MaxOpenIntents = 100
	// MaxProposedIntents bounds the proposed ones among them.
	MaxProposedIntents = 20
	// MaxListLimit is the most rows one list returns.
	MaxListLimit = 500

	maxExprBytes        = 500
	maxDescriptionRunes = 200
	maxProvenanceRunes  = 128
	maxReasonRunes      = 200
	maxJSONDepth        = 8
)

var (
	// ErrNotFound: there is no intent with that id.
	ErrNotFound = errors.New("intent: no such intent")
	// ErrBadTransition: the move is not in the transition table.
	ErrBadTransition = errors.New("intent: transition not allowed")
	// ErrTooManyOpen: the ledger already holds MaxOpenIntents open intents.
	ErrTooManyOpen = errors.New("intent: too many open intents")
	// ErrTooManyProposed: the ledger already holds MaxProposedIntents proposals.
	ErrTooManyProposed = errors.New("intent: too many suggested intents")
	// ErrNotActive: an intent that is not active cannot fire.
	ErrNotActive = errors.New("intent: intent is not active")
	// ErrBudgetSpent: the intent has used all of its fires.
	ErrBudgetSpent = errors.New("intent: fire budget spent")
	// ErrInferredMandate: an inferred intent can never carry a mandate.
	ErrInferredMandate = errors.New("intent: an inferred intent cannot carry a mandate")
	// ErrMandatesUnavailable: no mandate exists yet to attach.
	ErrMandatesUnavailable = errors.New("intent: mandates are not available yet")
	// ErrNotOwnerAuthored: an intent cannot become or start active unless it was
	// made, or confirmed, by an owner's user turn.
	ErrNotOwnerAuthored = errors.New("intent: only an explicit, user-authored intent can be active")
)

// transitions is the table of allowed status changes. Beyond the spec's table
// (proposed to active or cancelled; active to paused, done, cancelled or expired;
// paused to active or cancelled) it lets proposed and paused intents expire:
// a proposal expires after ProposalTTL, and an intent past its ExpiresAt reads as
// expired whatever it was.
var transitions = map[Status][]Status{
	StatusProposed: {StatusActive, StatusCancelled, StatusExpired},
	StatusActive:   {StatusPaused, StatusDone, StatusCancelled, StatusExpired},
	StatusPaused:   {StatusActive, StatusCancelled, StatusExpired},
}

// CanTransition reports whether the table allows from to go to to.
func CanTransition(from, to Status) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

var (
	kindPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	turnKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// normalizeText returns s as one printable line of at most maxRunes characters.
// Whitespace of every kind (newlines and tabs included) becomes a single space.
// A control character, a bidirectional control character or an invisible
// formatting character is refused rather than dropped: a title that holds one is
// hiding something, and the owner should see the refusal, not a silently altered
// text. The joiners U+200C and U+200D are kept (emoji and some scripts need them).
// Text too long is refused, not cut: a cut title can say something else.
func normalizeText(field, s string, maxRunes int, required bool) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("intent: %s is not valid text", field)
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	for _, r := range s {
		if unicode.IsControl(r) || proactive.IsBidiControl(r) ||
			(unicode.Is(unicode.Cf, r) && r != 0x200C && r != 0x200D) {
			return "", fmt.Errorf("intent: %s holds a control or invisible formatting character (U+%04X)", field, r)
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" && required {
		return "", fmt.Errorf("intent: %s is required", field)
	}
	if n := utf8.RuneCountInString(s); n > maxRunes {
		return "", fmt.Errorf("intent: %s is %d characters; the limit is %d", field, n, maxRunes)
	}
	return s, nil
}

// normalizeJSONObject validates raw as a JSON object of at most max bytes (once
// compacted) that nests at most maxJSONDepth deep and holds only finite numbers,
// and returns it compacted. Empty input and JSON null mean "none" (nil).
func normalizeJSONObject(field string, raw json.RawMessage, max int) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > max*4 { // refuse before parsing something huge
		return nil, fmt.Errorf("intent: %s is %d bytes; the limit is %d", field, len(raw), max)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("intent: %s is not valid JSON", field)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("intent: %s is not valid JSON: %w", field, err)
	}
	if buf.Len() > max {
		return nil, fmt.Errorf("intent: %s is %d bytes; the limit is %d", field, buf.Len(), max)
	}
	// Decoding into float64 refuses a number out of range (1e999), which is how
	// "not finite" reaches JSON; NaN and Infinity are not valid JSON at all.
	var v any
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		return nil, fmt.Errorf("intent: %s holds a number that is not finite or not representable: %w", field, err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("intent: %s must be a JSON object", field)
	}
	if depth(obj) > maxJSONDepth {
		return nil, fmt.Errorf("intent: %s nests deeper than %d levels", field, maxJSONDepth)
	}
	return json.RawMessage(buf.Bytes()), nil
}

func depth(v any) int {
	switch x := v.(type) {
	case map[string]any:
		d := 0
		for _, e := range x {
			if n := depth(e); n > d {
				d = n
			}
		}
		return d + 1
	case []any:
		d := 0
		for _, e := range x {
			if n := depth(e); n > d {
				d = n
			}
		}
		return d + 1
	}
	return 0
}

// cleanProvenance returns p with its free-text fields reduced to one short
// printable line and its enumerations checked.
func cleanProvenance(p Provenance) (Provenance, error) {
	clean := func(s string) string { return cleanAuthor(s) }
	p.SessionKey, p.MessageID, p.SenderID = clean(p.SessionKey), clean(p.MessageID), clean(p.SenderID)
	if p.Trust < proactive.TrustUntrusted || p.Trust > proactive.TrustUserAuthored {
		return Provenance{}, fmt.Errorf("intent: unknown trust level %d", int(p.Trust))
	}
	if !turnKindPattern.MatchString(string(p.TurnKind)) {
		return Provenance{}, fmt.Errorf("intent: turn kind %q is not a plain lower-case token", p.TurnKind)
	}
	return p, nil
}

// ownerAuthored reports whether p is the provenance of a user turn whose text is
// the owner's: the floor under every activation.
func ownerAuthored(p Provenance) bool {
	return p.TurnKind == proactive.TurnUser && p.Trust == proactive.TrustUserAuthored && p.SenderID != ""
}

// normalizeNew validates and normalises in for creation at time now, and returns
// the intent to store (without ID, timestamps or expiry, which the ledger sets).
func normalizeNew(in Intent) (Intent, error) {
	out := in
	if !in.Type.Valid() {
		return Intent{}, fmt.Errorf("intent: unknown type %q", in.Type)
	}
	title, err := normalizeText("title", in.Title, MaxTitleRunes, true)
	if err != nil {
		return Intent{}, err
	}
	out.Title = title
	if out.Params, err = normalizeJSONObject("params", in.Params, MaxParamsBytes); err != nil {
		return Intent{}, err
	}

	out.Trigger = nil
	if in.Trigger != nil {
		if !kindPattern.MatchString(in.Trigger.Kind) {
			return Intent{}, fmt.Errorf("intent: trigger kind %q must be a lower-case token such as price_cross", in.Trigger.Kind)
		}
		tp, err := normalizeJSONObject("trigger params", in.Trigger.Params, MaxParamsBytes)
		if err != nil {
			return Intent{}, err
		}
		out.Trigger = &TriggerSpec{Kind: in.Trigger.Kind, Params: tp}
	}

	out.Expected = nil
	if e := in.Expected; e != nil {
		expr, err := normalizeText("expectation", e.Expr, maxExprBytes, true)
		if err != nil {
			return Intent{}, err
		}
		desc, err := normalizeText("expectation description", e.Description, maxDescriptionRunes, false)
		if err != nil {
			return Intent{}, err
		}
		if e.Severity < proactive.SeverityInfo || e.Severity > proactive.SeverityCritical {
			return Intent{}, fmt.Errorf("intent: unknown severity %d", int(e.Severity))
		}
		out.Expected = &Expectation{Expr: expr, Description: desc, Severity: e.Severity}
	}

	if in.MandateID != "" {
		if in.Source != SourceExplicit {
			return Intent{}, ErrInferredMandate
		}
		// MR-310 relaxes this for an explicit intent; until a mandate exists there
		// is nothing to point at.
		return Intent{}, ErrMandatesUnavailable
	}

	switch in.Status {
	case "":
		out.Status = StatusProposed
	case StatusProposed, StatusActive:
	default:
		return Intent{}, fmt.Errorf("intent: an intent is created proposed or active, not %q", in.Status)
	}
	switch in.Source {
	case "":
		out.Source = SourceInferred
	case SourceExplicit, SourceInferred, SourceReflection:
	default:
		return Intent{}, fmt.Errorf("intent: unknown source %q", in.Source)
	}
	if out.Provenance, err = cleanProvenance(in.Provenance); err != nil {
		return Intent{}, err
	}
	if out.Status == StatusActive && (out.Source != SourceExplicit || !ownerAuthored(out.Provenance)) {
		return Intent{}, ErrNotOwnerAuthored
	}

	if in.FireBudget < 0 || in.FireBudget > MaxFireBudget {
		return Intent{}, fmt.Errorf("intent: fire budget %d is outside 0..%d", in.FireBudget, MaxFireBudget)
	}
	if in.FiresUsed != 0 {
		return Intent{}, errors.New("intent: a new intent has used no fires")
	}
	if in.Cooldown < 0 || in.Cooldown > MaxCooldown {
		return Intent{}, fmt.Errorf("intent: cooldown %s is outside 0..%s", in.Cooldown, MaxCooldown)
	}
	if in.Lifetime != 0 && (in.Lifetime < MinLifetime || in.Lifetime > MaxLifetime) {
		return Intent{}, fmt.Errorf("intent: lifetime %s is outside %s..%s", in.Lifetime, MinLifetime, MaxLifetime)
	}
	if in.ExpiresAt != nil || in.InvalidAt != nil || in.SupersededBy != "" || in.FiresUsed != 0 {
		return Intent{}, errors.New("intent: set Lifetime, not ExpiresAt; a new intent is not yet ended or superseded")
	}
	return out, nil
}

// cleanReason keeps a reason to one short printable line.
func cleanReason(s string) string {
	s = cleanAuthor(s)
	if r := []rune(s); len(r) > maxReasonRunes {
		s = string(r[:maxReasonRunes])
	}
	return s
}
