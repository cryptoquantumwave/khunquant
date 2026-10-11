// Package proactive is the shared vocabulary of KhunQuant's proactive agent:
// the small set of types that triggers, attention, notification and the agent
// loop all exchange.
//
// It imports nothing outside the standard library. That is a hard rule, not a
// preference: pkg/bus and pkg/events import this package, so any dependency
// added here risks an import cycle through the rest of the tree.
package proactive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// enumNames is the stable text form of an int-backed enumeration, indexed by
// value. The names are what gets stored and logged, so they are part of the
// on-disk format: never rename or reorder them.
type enumNames []string

// name returns the text form of v, or a "kind(n)" placeholder when v is out of
// range. The placeholder never parses back, so it cannot be mistaken for data.
func (e enumNames) name(kind string, v int) string {
	if v >= 0 && v < len(e) {
		return e[v]
	}
	return kind + "(" + strconv.Itoa(v) + ")"
}

// parse resolves a case-insensitive name. An unknown name is an error rather
// than a silent zero value: a typo in stored data must not quietly become the
// lowest severity or the least trusted source.
func (e enumNames) parse(kind, text string) (int, error) {
	t := strings.ToLower(strings.TrimSpace(text))
	for i, n := range e {
		if n == t {
			return i, nil
		}
	}
	return 0, fmt.Errorf("proactive: unknown %s %q", kind, text)
}

// marshal returns the quoted name, or an error for an out-of-range value.
func (e enumNames) marshal(kind string, v int) ([]byte, error) {
	if v < 0 || v >= len(e) {
		return nil, fmt.Errorf("proactive: cannot marshal %s", e.name(kind, v))
	}
	return []byte(strconv.Quote(e[v])), nil
}

// unmarshalJSON accepts either the quoted name or the integer form. ok is
// false for JSON null, which by convention leaves the receiver unchanged.
func (e enumNames) unmarshalJSON(kind string, data []byte) (v int, ok bool, err error) {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return 0, false, nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return 0, false, fmt.Errorf("proactive: %s: %w", kind, err)
		}
		v, err = e.parse(kind, s)
		return v, err == nil, err
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return 0, false, fmt.Errorf("proactive: %s must be a name or an integer, got %s", kind, data)
	}
	if n < 0 || n >= len(e) {
		return 0, false, fmt.Errorf("proactive: %s %d out of range", kind, n)
	}
	return n, true, nil
}

var severityNames = enumNames{"info", "notice", "warning", "critical"}

// Severity orders how urgently something deserves the owner's attention.
type Severity int

const (
	SeverityInfo     Severity = iota // digest material
	SeverityNotice                   // worth a message in active hours, within budget
	SeverityWarning                  // message in active hours, may exceed budget by 1
	SeverityCritical                 // protective: bypasses quiet hours and budget
)

// String returns the lower-case name ("info", "notice", "warning", "critical").
func (s Severity) String() string { return severityNames.name("severity", int(s)) }

// ParseSeverity parses a severity name, case-insensitively. Unknown text is an
// error, never SeverityInfo.
func ParseSeverity(text string) (Severity, error) {
	v, err := severityNames.parse("severity", text)
	return Severity(v), err
}

// MarshalText implements encoding.TextMarshaler.
func (s Severity) MarshalText() ([]byte, error) {
	b, err := severityNames.marshal("severity", int(s))
	if err != nil {
		return nil, err
	}
	return b[1 : len(b)-1], nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(text []byte) error {
	v, err := ParseSeverity(string(text))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// MarshalJSON encodes the lower-case name.
func (s Severity) MarshalJSON() ([]byte, error) { return severityNames.marshal("severity", int(s)) }

// UnmarshalJSON accepts the name or, for older stored data, the integer.
func (s *Severity) UnmarshalJSON(data []byte) error {
	v, ok, err := severityNames.unmarshalJSON("severity", data)
	if err != nil || !ok {
		return err
	}
	*s = Severity(v)
	return nil
}

var trustNames = enumNames{"untrusted", "exchange", "system", "user_authored"}

// Trust is the provenance of the data that produced a candidate or action.
type Trust int

const (
	TrustUntrusted    Trust = iota // web, news, forwarded text, external triggers
	TrustExchange                  // structured data returned by an exchange API
	TrustSystem                    // computed by KhunQuant from trusted inputs
	TrustUserAuthored              // the owner said/typed/confirmed it
)

// MinTrust returns the lowest of ts: a value derived from several inputs is
// only as trustworthy as its least trustworthy input. With no arguments it
// returns TrustUntrusted, so a caller that forgot to supply inputs fails
// closed.
func MinTrust(ts ...Trust) Trust {
	if len(ts) == 0 {
		return TrustUntrusted
	}
	m := ts[0]
	for _, t := range ts[1:] {
		if t < m {
			m = t
		}
	}
	return m
}

// String returns the lower-case name ("untrusted", "exchange", "system",
// "user_authored").
func (t Trust) String() string { return trustNames.name("trust", int(t)) }

// ParseTrust parses a trust name, case-insensitively. Unknown text is an
// error, never TrustUntrusted.
func ParseTrust(text string) (Trust, error) {
	v, err := trustNames.parse("trust", text)
	return Trust(v), err
}

// MarshalText implements encoding.TextMarshaler.
func (t Trust) MarshalText() ([]byte, error) {
	b, err := trustNames.marshal("trust", int(t))
	if err != nil {
		return nil, err
	}
	return b[1 : len(b)-1], nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *Trust) UnmarshalText(text []byte) error {
	v, err := ParseTrust(string(text))
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// MarshalJSON encodes the lower-case name.
func (t Trust) MarshalJSON() ([]byte, error) { return trustNames.marshal("trust", int(t)) }

// UnmarshalJSON accepts the name or, for older stored data, the integer.
func (t *Trust) UnmarshalJSON(data []byte) error {
	v, ok, err := trustNames.unmarshalJSON("trust", data)
	if err != nil || !ok {
		return err
	}
	*t = Trust(v)
	return nil
}

// TurnKind classifies every LLM turn; tool ceilings and budgets key on it.
type TurnKind string

const (
	TurnUser             TurnKind = "user"
	TurnProactive        TurnKind = "proactive"        // composes a message for candidates
	TurnHeartbeatLegacy  TurnKind = "heartbeat_legacy" // HEARTBEAT.md turn until MR-213
	TurnCronLegacy       TurnKind = "cron_legacy"      // non-migrated cron agent jobs
	TurnApprovalFollowUp TurnKind = "approval_followup"
	TurnReflection       TurnKind = "reflection"
	TurnSubTurn          TurnKind = "subturn"
)

var composeNames = enumNames{"template", "llm"}

// ComposeMode says how a candidate's message is produced.
type ComposeMode int

const (
	ComposeTemplate ComposeMode = iota // rendered from a template, no LLM
	ComposeLLM                         // worth an explanation: wake the LLM
)

// String returns the lower-case name ("template", "llm").
func (c ComposeMode) String() string { return composeNames.name("compose mode", int(c)) }

// ParseComposeMode parses a compose-mode name, case-insensitively. Unknown
// text is an error, never ComposeTemplate.
func ParseComposeMode(text string) (ComposeMode, error) {
	v, err := composeNames.parse("compose mode", text)
	return ComposeMode(v), err
}

// MarshalText implements encoding.TextMarshaler.
func (c ComposeMode) MarshalText() ([]byte, error) {
	b, err := composeNames.marshal("compose mode", int(c))
	if err != nil {
		return nil, err
	}
	return b[1 : len(b)-1], nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *ComposeMode) UnmarshalText(text []byte) error {
	v, err := ParseComposeMode(string(text))
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// MarshalJSON encodes the lower-case name.
func (c ComposeMode) MarshalJSON() ([]byte, error) {
	return composeNames.marshal("compose mode", int(c))
}

// UnmarshalJSON accepts the name or, for older stored data, the integer.
func (c *ComposeMode) UnmarshalJSON(data []byte) error {
	v, ok, err := composeNames.unmarshalJSON("compose mode", data)
	if err != nil || !ok {
		return err
	}
	*c = ComposeMode(v)
	return nil
}

// Candidate is "something might deserve attention", produced by triggers (or,
// in Phase 1, by a heartbeat turn's respond_proactive call).
type Candidate struct {
	ID          string // ULID
	TriggerID   string // "price_cross:<id>", "dn:12", "heartbeat"
	DedupKey    string // stable per situation, e.g. "drawdown:portfolio:tier2"
	Severity    Severity
	Protective  bool           // margin / liquidation / stop-loss
	Salience    float64        // 0..1
	Subject     string         // "BTC/USDT", "plan:12", "portfolio"
	Title       string         // template-rendered headline (no LLM)
	Evidence    map[string]any // numbers for templates; never restated by the LLM
	IntentID    string
	Trust       Trust
	Compose     ComposeMode   // ComposeTemplate (no LLM) | ComposeLLM (worth an explanation)
	DedupWindow time.Duration // 0 = attention default (MR-208)
	At          time.Time
	Shadow      bool // produced by a shadow-mode trigger
}

// Disposition is attention's decision about a candidate.
type Disposition string

const (
	DispDrop     Disposition = "drop"     // suppressed (dedup, same tier, muted)
	DispLog      Disposition = "log"      // recorded only
	DispDigest   Disposition = "digest"   // queued for the next digest
	DispNotify   Disposition = "notify"   // templated message now, no LLM
	DispEscalate Disposition = "escalate" // wake the LLM to compose/judge
)

// DecisionKind is what a proactive LLM turn asked for (via respond_proactive).
type DecisionKind string

const (
	DecideSilent  DecisionKind = "silent"
	DecideNotify  DecisionKind = "notify"
	DecideAsk     DecisionKind = "ask"
	DecidePropose DecisionKind = "propose"
)

// Decision is the outcome of a proactive LLM turn.
type Decision struct {
	Kind         DecisionKind
	Severity     Severity
	Text         string // prose around template-rendered evidence
	Question     string // for ask
	ProposalRef  string // for propose (Phase 3)
	CandidateIDs []string
}

// OwnerRoute is where proactive output goes.
type OwnerRoute struct {
	Channel  string // "telegram"
	ChatID   string // DM chat id
	SenderID string // canonical "telegram:123456" — used for owner checks
	PeerID   string // platform user id used to build the owner session key
}
