package perception

import (
	"strings"
	"time"
)

// The closed vocabularies of a news item. The parser (a tool-less model call) must
// answer inside them: anything else is discarded, so these are the only words from the
// model that ever reach a trigger as anything but the one-line Summary.
const (
	NewsUp      = "up"
	NewsDown    = "down"
	NewsNeutral = "neutral"

	NewsLow    = "low"
	NewsMedium = "medium"
	NewsHigh   = "high"
)

// NewsEventTypes are the kinds of event a parser may name, and NewsDirections and
// NewsMagnitudes the other two enumerations.
var (
	NewsEventTypes = []string{
		"hack", "listing", "delisting", "regulation", "earnings", "macro", "partnership", "other",
	}
	NewsDirections = []string{NewsUp, NewsDown, NewsNeutral}
	NewsMagnitudes = []string{NewsLow, NewsMedium, NewsHigh}
)

// NewsSource is one article behind a news item: the registrable domain it came from
// and its link, both computed by code from the URL (never from the article).
type NewsSource struct {
	Domain string
	URL    string
}

// NewsItem is the payload of a events.KindPerceptionNewsItem event: what a parser read
// in articles about one asset, after code validated every field, and how many
// independent sites said it.
//
// It is TrustUntrusted end to end. Every field but Summary is a closed enumeration, a
// number or a string code computed (the asset symbol is the one asked about, the
// domains and links come from the URLs); Summary is the parser's one line about an
// article that anyone could have written, cleaned to 200 characters, and is shown only
// quoted ("Reported by N sources"). The payload is private to the pipeline: the bus
// sees the event's envelope alone.
type NewsItem struct {
	Asset     string
	Direction string // up | down | neutral
	Magnitude string // low | medium | high
	EventType string // one of NewsEventTypes
	// PublishedAt is when the article says it was published: RFC3339, within 48 hours
	// and not in the future.
	PublishedAt time.Time
	Summary     string
	// Sources are the distinct registrable domains that reported the same (asset,
	// direction, event_type) within 24 hours, one entry each, sorted by domain.
	Sources []NewsSource
	// Corroboration is len(Sources): one domain counts once however many articles.
	Corroboration int
	// Watched says the asset is the symbol of one of the owner's active watches. It is
	// set by code from the owner's intents, never from the article. Whether the asset
	// is held is read by the trigger from the world model.
	Watched bool
	At      time.Time
}

// PrivatePayload marks the payload as private (see PrivatePayload).
func (NewsItem) PrivatePayload() {}

// ValidNewsEnum reports whether v is one of the allowed values.
func ValidNewsEnum(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// CleanNewsLine reduces s to one line of at most limit characters: control, format,
// bidirectional and unprintable characters are dropped, white space collapses, and a
// cut ends with an ellipsis.
func CleanNewsLine(s string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case IsBidiControl(r), r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0, r == 0xad, r >= 0x200b && r <= 0x200d,
			r == 0x2028, r == 0x2029, r == 0x2060, r == 0xfeff, r >= 0xe000 && r <= 0xf8ff, r == 0xfffd:
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if rs := []rune(out); len(rs) > limit {
		out = strings.TrimRight(string(rs[:limit-1]), " ") + "…"
	}
	return out
}
