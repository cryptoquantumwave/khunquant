package triggers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// KindExternal is the kind of the builtin trigger that turns a request from
// outside the process ("khunquant trigger", POST /api/proactive/trigger) into a
// candidate.
const KindExternal = "external"

// ExternalSpecID is the ID of the one builtin spec of the external kind.
const ExternalSpecID = "builtin"

// externalDedupWindow is how long the same message under the same name is
// absorbed: a script that reports the same failure every few minutes is told once.
const externalDedupWindow = 10 * time.Minute

// ExternalFactory builds the external trigger. It takes no parameters and refuses
// any, so a spec cannot carry configuration into it.
func ExternalFactory(spec Spec) (Trigger, error) {
	if p := strings.TrimSpace(string(spec.Params)); p != "" && p != "{}" && p != "null" {
		return nil, errNoExternalParams
	}
	return externalTrigger{id: spec.Kind + ":" + spec.ID}, nil
}

var errNoExternalParams = errors.New("the external trigger takes no parameters")

// externalTrigger makes one candidate from each request. It is a door for text
// from outside, so it is deliberately plain:
//
//   - the title is the validated name and nothing else; the message is evidence;
//   - the dedup key carries a hash of the message, never the message;
//   - the candidate is untrusted, is never protective and never critical, and is
//     composed from the template only: an external message cannot wake the model
//     by itself;
//   - it never returns an error for what a request contains, because the registry
//     switches a spec off after ten errors in a row and a request must not be able
//     to do that. What it cannot use it ignores.
type externalTrigger struct{ id string }

func (t externalTrigger) ID() string { return t.id }

func (externalTrigger) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionExternalTrigger}
}

func (externalTrigger) Demands() []perception.Demand { return nil }

func (externalTrigger) Evaluate(_ context.Context, _ worldmodel.View, d worldmodel.Diff, _ *State) ([]proactive.Candidate, error) {
	var req perception.ExternalTrigger
	switch p := d.Payload.(type) {
	case perception.ExternalTrigger:
		req = p
	case *perception.ExternalTrigger:
		if p == nil {
			return nil, nil
		}
		req = *p
	default:
		return nil, nil // not a request this trigger knows
	}
	if !perception.ValidExternalName(req.Name) {
		return nil, nil
	}

	sev := ExternalSeverity(req.Severity)
	msg := cleanExternalMessage(req.Message)
	sum := sha256.Sum256([]byte(msg))
	return []proactive.Candidate{{
		DedupKey:    "external:" + req.Name + ":" + hex.EncodeToString(sum[:4]),
		Severity:    sev,
		Protective:  false,
		Salience:    externalSalience(sev),
		Subject:     req.Name,
		Title:       req.Name,
		Quote:       externalQuote(msg),
		Evidence:    externalEvidence(msg, req.Data),
		Trust:       proactive.TrustUntrusted,
		Compose:     proactive.ComposeTemplate,
		DedupWindow: externalDedupWindow,
		At:          req.At,
	}}, nil
}

// ExternalSeverity is the severity an external request gets: the one it asked for
// ("info", "notice", "warning"), capped at Warning ("critical" counts as "warning"),
// and Notice when it names none or something unknown. An external request is never
// Critical.
func ExternalSeverity(requested string) proactive.Severity {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "info":
		return proactive.SeverityInfo
	case "warning", "critical":
		return proactive.SeverityWarning
	}
	return proactive.SeverityNotice
}

// externalSalience orders the severities the trigger can produce.
func externalSalience(s proactive.Severity) float64 {
	switch s {
	case proactive.SeverityInfo:
		return 0.3
	case proactive.SeverityWarning:
		return 0.7
	}
	return 0.5
}

// cleanExternalMessage bounds the message to the characters the door allows, and
// drops the bidirectional controls the door refuses (a payload that did not come
// through the door is cleaned here all the same).
func cleanExternalMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if perception.IsBidiControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) > perception.MaxExternalMessageRunes {
		s = string([]rune(s)[:perception.MaxExternalMessageRunes])
	}
	return s
}

const (
	// externalQuoteLineRunes caps one line of the quote; the quote as a whole stays
	// within proactive.MaxQuoteRunes and proactive.MaxQuoteLines.
	externalQuoteLineRunes = 200
	// externalQuoteMarker is the last line of a quote that was cut.
	externalQuoteMarker = "…"
)

// externalQuote is the message as the Quote of the candidate: one cleaned line per
// paragraph, each cut at externalQuoteLineRunes, at most proactive.MaxQuoteLines lines
// and proactive.MaxQuoteRunes characters including a last "…" line that says the
// message goes on. Attention shows the lines to the owner quoted ("> "), which says
// the words are somebody else's, and keeps them out of the owner's session. "" for
// no message.
func externalQuote(msg string) string {
	var lines []string
	used := 0
	// Room for the marker and its line break is always kept.
	budget := proactive.MaxQuoteRunes - utf8.RuneCountInString(externalQuoteMarker) - 1
	cut := false
	for _, para := range strings.Split(strings.ReplaceAll(msg, "\r", "\n"), "\n") {
		line := oneExternalLine(para, externalQuoteLineRunes)
		if line == "" {
			continue
		}
		cost := utf8.RuneCountInString(line) + 1
		if len(lines) == proactive.MaxQuoteLines-1 || used+cost > budget {
			cut = true
			break
		}
		lines = append(lines, line)
		used += cost
	}
	if cut {
		lines = append(lines, externalQuoteMarker)
	}
	return strings.Join(lines, "\n")
}

// oneExternalLine reduces s to one line of at most limit characters: white space
// collapses, control, format and bidirectional characters and anything not
// printable are dropped, and a cut ends with an ellipsis.
func oneExternalLine(s string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), perception.IsBidiControl(r), !unicode.IsPrint(r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > limit {
		out = strings.TrimRight(string([]rune(out)[:limit-1]), " ") + "…"
	}
	return out
}

// externalEvidence is the message and the data, fitted into the evidence bound: a
// request that fits the door's limits can still overflow the encoded bound (every
// '<' encodes as six bytes), and an overflow would be an error against the spec.
// The data goes first, then the end of the message.
func externalEvidence(msg string, data json.RawMessage) map[string]any {
	ev := map[string]any{}
	if len(data) > 0 && len(data) <= perception.MaxExternalDataBytes && json.Valid(data) {
		ev["data"] = json.RawMessage(append([]byte(nil), data...))
	}
	if msg != "" {
		ev["message"] = msg
	}
	if fits(ev) {
		return ev
	}
	delete(ev, "data")
	for msg != "" && !fits(ev) {
		r := []rune(msg)
		msg = string(r[:len(r)*3/4])
		if msg == "" {
			delete(ev, "message")
			break
		}
		ev["message"] = msg
	}
	return ev
}

func fits(ev map[string]any) bool {
	b, err := json.Marshal(ev)
	return err == nil && len(b) <= MaxEvidenceBytes
}
