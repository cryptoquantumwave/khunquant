package notify

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	maxTitleRunes = 200
	maxBodyRunes  = 3500
	ellipsis      = "…"

	// headerPrefix opens the line that marks a session message as proactive.
	// Text from outside must never be able to produce a line that starts with
	// it (see neutraliseHeaders).
	headerPrefix = "[proactive"
)

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// validInterventionID reports whether id may appear in a session header: 1 to
// 32 characters, all from the ULID alphabet. The restriction is what keeps an
// ID from carrying a newline, a bracket or a forged field into the header.
func validInterventionID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if strings.IndexByte(crockford, id[i]) < 0 {
			return false
		}
	}
	return true
}

// rendered is the text of one notification in its two forms.
type rendered struct {
	Outbound string // what the owner's chat receives
	Session  string // what is written into the owner's session
}

// render builds both forms of a notification. Title and Body may carry model
// text or exchange-supplied strings, so both are cleaned and capped here; the
// caller never sees the raw values again. It returns ok=false when nothing
// is left to send.
func render(severity proactive.Severity, interventionID, title, body string) (rendered, bool) {
	title = cleanTitle(title)
	body = cleanBody(body)
	if title == "" && body == "" {
		return rendered{}, false
	}

	var out strings.Builder
	switch {
	case title != "" && body != "":
		out.WriteString(title + "\n\n" + body)
	case title != "":
		out.WriteString(title)
	default:
		out.WriteString(body)
	}

	return rendered{Outbound: out.String(), Session: sessionCopy(severity, interventionID, title, body)}, true
}

// sessionCopy is the text written into the owner's session for a message: the
// proactive header line, then the (already cleaned) title and body.
func sessionCopy(severity proactive.Severity, interventionID, title, body string) string {
	var sess strings.Builder
	sess.WriteString(headerPrefix + " · " + severity.String() + " · " + interventionID + "]")
	if title != "" {
		sess.WriteString("\n" + title)
	}
	if body != "" {
		sess.WriteString("\n" + body)
	}
	return sess.String()
}

// cleanTitle returns a single line of at most maxTitleRunes runes.
func cleanTitle(s string) string {
	s = stripUnsafe(s, false)
	s = strings.Join(strings.Fields(s), " ")
	s = neutraliseHeaders(s)
	return capRunes(s, maxTitleRunes)
}

// cleanBody returns text of at most maxBodyRunes runes in which no line can be
// mistaken for a proactive header.
func cleanBody(s string) string {
	s = stripUnsafe(s, true)
	s = strings.TrimSpace(s)
	s = neutraliseHeaders(s)
	return capRunes(s, maxBodyRunes)
}

// stripUnsafe removes control characters and invisible direction/format
// characters. With keepNewlines, "\n" survives ("\r\n" and "\r" become "\n")
// and a tab becomes a space; otherwise every line break becomes a space.
func stripUnsafe(s string, keepNewlines bool) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			if keepNewlines {
				b.WriteRune('\n')
			} else {
				b.WriteRune(' ')
			}
		case r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r), isInvisibleFormat(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isInvisibleFormat matches characters that change how text is displayed
// without being visible themselves: bidirectional overrides and isolates,
// zero-width spaces, the word joiner and the byte-order mark.
func isInvisibleFormat(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, // embeddings and overrides
		r >= 0x2066 && r <= 0x2069, // isolates
		r == 0x200B, r == 0x200E, r == 0x200F, r == 0x2060, r == 0xFEFF, r == 0x061C:
		return true
	}
	return false
}

// neutraliseHeaders rewrites any line that begins (after leading space) with
// the proactive header marker so it no longer does: the opening bracket becomes
// a parenthesis. The model reads the session copy, so a forged header would
// let an exchange-supplied string pass as the assistant's own.
func neutraliseHeaders(s string) string {
	if !strings.Contains(strings.ToLower(s), headerPrefix) {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if len(trimmed) >= len(headerPrefix) && strings.EqualFold(trimmed[:len(headerPrefix)], headerPrefix) {
			lead := line[:len(line)-len(trimmed)]
			lines[i] = lead + "(" + trimmed[1:]
		}
	}
	return strings.Join(lines, "\n")
}

// capRunes shortens s to at most limit runes, ending with an ellipsis when it
// had to cut.
func capRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:limit-1]), " \n") + ellipsis
}
