package perception

import (
	"encoding/json"
	"time"
)

// Bounds on what a request to the external trigger may carry. They are enforced
// where the request enters (the gateway route, the CLI) and again, defensively,
// by the trigger that handles the event.
const (
	// MaxExternalNameLen is the longest name of an external trigger.
	MaxExternalNameLen = 41
	// MaxExternalMessageRunes bounds the message, in characters.
	MaxExternalMessageRunes = 2000
	// MaxExternalDataBytes bounds the data, as compact JSON.
	MaxExternalDataBytes = 4 << 10
	// MaxExternalDataDepth bounds how deeply the data may nest.
	MaxExternalDataDepth = 6
)

// ExternalTrigger is the payload of a events.KindPerceptionExternalTrigger event:
// a request that something outside the process (a script, a hook) made to be
// noticed. Every field is text from outside, validated at the door and still not
// to be trusted: the event carrying it is TrustUntrusted.
//
// The world model does not track this kind, so the payload reaches the trigger
// that handles it through worldmodel.Diff.Payload, which is never serialised and
// never published. The event bus never sees the payload either: the pipeline
// publishes an event whose payload is a PrivatePayload as its envelope alone.
type ExternalTrigger struct {
	// Name is the validated name (ValidExternalName).
	Name string
	// Message is at most MaxExternalMessageRunes characters of text.
	Message string
	// Severity is what the caller asked for: "info", "notice", "warning" or
	// "critical". The trigger caps it; "" means notice.
	Severity string
	// Data is compact JSON, an object, at most MaxExternalDataBytes; empty when none.
	Data json.RawMessage
	// At is when the request was received.
	At time.Time
}

// PrivatePayload is implemented by an event payload that must not leave the
// pipeline's own stages: text from outside that only the trigger handling its kind
// may read. The pipeline publishes such an event to the bus without its payload
// and attributes.
type PrivatePayload interface {
	PrivatePayload()
}

// PrivatePayload marks the payload as private.
func (ExternalTrigger) PrivatePayload() {}

// ValidExternalName reports whether name matches ^[a-z0-9][a-z0-9_.-]{0,40}$.
func ValidExternalName(name string) bool {
	if name == "" || len(name) > MaxExternalNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '.' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// IsBidiControl reports whether r is a bidirectional control character: the
// embeddings and overrides U+202A-U+202E, the isolates U+2066-U+2069, the marks
// U+200E and U+200F and the Arabic letter mark U+061C. They change the order text
// is displayed in without being visible, so a message that holds one can show the
// owner something other than what it says. The joiners U+200C and U+200D are not
// among them: emoji and several scripts need them.
func IsBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069,
		r == 0x200E, r == 0x200F, r == 0x061C:
		return true
	}
	return false
}
