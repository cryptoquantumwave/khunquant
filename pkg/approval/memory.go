package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// KindMemory is a fact somebody other than the owner's own conversation wants kept in
// MEMORY.md. Approving it appends the fact; rejecting it, or letting it expire, writes
// nothing. The fact is text a model wrote, so it is stored only here until the owner
// has read it in the digest: it is in no prompt and in no file before that.
const KindMemory Kind = "memory"

// OriginSuggestion marks a proposal made by the remember tool outside the owner's own
// conversation turn (a background turn, a sub-turn, or somebody who is not the owner).
const OriginSuggestion Origin = "suggestion"

// Memory bounds, counted apart from orders and mandates.
const (
	// MaxFactRunes bounds a fact.
	MaxFactRunes = 500
	// MaxPendingMemory is how many memory suggestions of the agent itself (and of the owner's
	// own non-root turns) may wait for a decision. It counts nobody else's.
	MaxPendingMemory = 5
	// MaxMemoryPerDay is how many of those may be created in a rolling day.
	MaxMemoryPerDay = 10
	// MaxPendingPerSuggester is how many suggestions one person who is not the owner may
	// have waiting. Two is enough for an honest "remember this" and leaves a stranger
	// able to put little in front of the owner.
	MaxPendingPerSuggester = 2
	// MaxMemoryPerSuggesterPerDay is how many one such person may create in a rolling day.
	// It is above the pending bound so that a decided suggestion frees a slot, and low
	// enough that a person cannot turn the digest into their own channel.
	MaxMemoryPerSuggesterPerDay = 4
	// MaxPendingMemoryPersons is how many suggestions all such people together may have
	// waiting, so that many senders cannot flood the digest. It never touches
	// MaxPendingMemory.
	MaxPendingMemoryPersons = 5
	// MaxMemoryPersonsPerDay is how many all such people together may create in a rolling day.
	MaxMemoryPersonsPerDay = 10
	// MaxSuggesterNameRunes bounds the name of a suggester shown in the digest.
	MaxSuggesterNameRunes = 32
	// MemoryExpiry is how long the owner has to decide.
	MemoryExpiry = 7 * 24 * time.Hour
	// MaxDigestMemoryCards is how many suggestions of one digest get buttons
	// (two buttons each, and a message carries at most bus.MaxActions).
	MaxDigestMemoryCards = bus.MaxActions / 2
)

// Memory categories.
const (
	CategoryPreference = "preference"
	CategoryFact       = "fact"
	CategoryPlanNote   = "plan_note"
)

// ValidCategory reports whether c is a memory category.
func ValidCategory(c string) bool {
	return c == CategoryPreference || c == CategoryFact || c == CategoryPlanNote
}

// ErrMemoryUnavailable: memory suggestions cannot be queued (no owner to confirm them,
// or the digest that would carry them is not running).
var ErrMemoryUnavailable = errors.New("approval: memory suggestions are not available")

// ErrMemoryDaily: too many memory suggestions in the last day.
var ErrMemoryDaily = errors.New("approval: too many memory suggestions today")

// ErrMemorySuggesterFull: the bound of a person who is not the owner is reached. It says
// nothing about how many suggestions wait, whose they are or what they say.
var ErrMemorySuggesterFull = errors.New("approval: you already have memory suggestions waiting for the owner's decision; " +
	"try again after they have been decided or have expired")

// MemoryParams is the canonical, hashed description of a suggested memory.
type MemoryParams struct {
	Fact     string `json:"fact"`
	Category string `json:"category"`
	// Source says which kind of turn suggested it (heartbeat, cron, proactive, sub_turn,
	// non_owner); it is a label for the owner and is never an instruction.
	Source string `json:"source"`
	// By says who suggested it when that was not the agent itself: SuggestedByOwner or
	// SuggestedByPerson. Empty means the agent (and is how every suggestion made before
	// this field existed reads).
	By string `json:"by,omitempty"`
	// Who is the cleaned name of the person (By is SuggestedByPerson), channel included:
	// "alice (telegram)". It is remote-authored text, shown to the owner on a line of its own.
	Who string `json:"who,omitempty"`
}

// Who suggested a memory.
const (
	SuggestedByOwner  = "owner"
	SuggestedByPerson = "person"
)

// Suggester is the author of a suggestion. The zero value is the agent itself.
type Suggester struct {
	// Kind is "" (the agent: a heartbeat, a job, a sub-turn of one), SuggestedByOwner (the
	// owner's own turn that is not their root conversation) or SuggestedByPerson.
	Kind string
	// Channel and ID name a person stably (the bound is kept per Channel and ID). Name is
	// what the channel calls them, and may be empty.
	Channel, ID, Name string
}

// suggesterPrefix starts the key stored in the proposal's intent_id column (which a
// memory proposal otherwise leaves empty) for a suggestion made by a person.
const suggesterPrefix = "suggester:"

func isSuggesterKey(k string) bool { return strings.HasPrefix(k, suggesterPrefix) }

// suggesterKey is the stable key the per-suggester bound is counted by: a digest of the
// channel and sender id, so it has a fixed size and cannot carry text.
func suggesterKey(channel, id string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(channel)) + "\x00" + strings.ToLower(strings.TrimSpace(id))))
	return suggesterPrefix + hex.EncodeToString(sum[:16])
}

// cleanName reduces remote-authored text to a short single-line label: invalid UTF-8,
// control, line-break and invisible formatting characters (bidi included) are dropped,
// white space collapses, the brackets that would let it imitate the "(channel)" part and
// the quote marker are removed, and it is cut at max runes.
func cleanName(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
		case r == '(' || r == ')' || r == '>' || r == '<':
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if rs := []rune(out); len(rs) > max {
		out = strings.TrimSpace(string(rs[:max]))
	}
	return out
}

// maxWhoRunes bounds a stored label: a name, a space and a bracketed channel.
const maxWhoRunes = MaxSuggesterNameRunes + 3 + 16

// SuggesterLabel is how a person is named in the digest: "name (channel)", or
// "id (channel)" when the channel gave no name (or one that cleans to nothing).
func SuggesterLabel(s Suggester) string {
	ch := cleanName(s.Channel, 16)
	name := cleanName(s.Name, MaxSuggesterNameRunes)
	if name == "" {
		name = cleanName(s.ID, MaxSuggesterNameRunes)
	}
	if name == "" {
		name = "unknown"
	}
	if ch == "" {
		return name
	}
	return name + " (" + ch + ")"
}

// SourceText is what the digest says about who suggested the memory: "the assistant",
// "you" (the owner) or the person's label.
func (m MemoryParams) SourceText() string {
	switch m.By {
	case SuggestedByOwner:
		return "you"
	case SuggestedByPerson:
		if m.Who != "" {
			return m.Who
		}
		return "unknown"
	}
	return "the assistant"
}

// CleanFact reduces a fact to one clean line: white space (line breaks included)
// collapses to single spaces, control and invisible formatting characters (bidi
// overrides among them) are dropped, and the two comment delimiters are broken so that
// the fact can never close the stamp's comment or open one of its own. It refuses an
// empty fact and one over MaxFactRunes characters rather than truncating it, so that
// what the owner reads is the whole of what would be saved.
func CleanFact(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.ReplaceAll(strings.ReplaceAll(out, "<!--", "< !--"), "-->", "-- >")
	switch n := len([]rune(out)); {
	case n == 0:
		return "", errors.New("the fact is empty")
	case n > MaxFactRunes:
		return "", fmt.Errorf("the fact is %d characters; at most %d are allowed", n, MaxFactRunes)
	}
	return out, nil
}

func validateMemoryParams(m MemoryParams) error {
	if clean, err := CleanFact(m.Fact); err != nil || clean != m.Fact {
		return errors.New("the fact is not a clean single line")
	}
	switch {
	case !ValidCategory(m.Category):
		return errors.New("category must be preference, fact or plan_note")
	case strings.TrimSpace(m.Source) == "" || len(m.Source) > 32:
		return errors.New("a memory suggestion names the kind of turn that made it")
	case m.By != "" && m.By != SuggestedByOwner && m.By != SuggestedByPerson:
		return errors.New("a memory suggestion is by the agent, the owner or a person")
	case m.By == SuggestedByPerson && (m.Who == "" || len([]rune(m.Who)) > maxWhoRunes || strings.ContainsAny(m.Who, "\n\r<>")):
		return errors.New("a suggestion by a person names them")
	case m.By != SuggestedByPerson && m.Who != "":
		return errors.New("only a suggestion by a person carries a name")
	}
	return nil
}

func canonicalMemory(m MemoryParams) (json.RawMessage, error) {
	if err := validateMemoryParams(m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("approval: encode memory: %w", err)
	}
	return b, nil
}

// VerifiedMemory decodes a stored memory proposal's params and proves they are exactly
// what was hashed (the proof VerifiedMandate gives a mandate).
func VerifiedMemory(p Proposal) (MemoryParams, error) {
	if p.Kind != KindMemory {
		return MemoryParams{}, fmt.Errorf("approval: proposal %s is not a memory suggestion", p.Code)
	}
	want := hashOf(p.Kind, p.Params)
	if subtle.ConstantTimeCompare(want[:], p.Hash[:]) != 1 {
		return MemoryParams{}, errHashMismatch
	}
	dec := json.NewDecoder(bytes.NewReader(p.Params))
	dec.DisallowUnknownFields()
	var m MemoryParams
	if err := dec.Decode(&m); err != nil || dec.More() {
		return MemoryParams{}, errHashMismatch
	}
	again, err := json.Marshal(m)
	if err != nil || !bytes.Equal(again, p.Params) {
		return MemoryParams{}, errHashMismatch
	}
	if err := validateMemoryParams(m); err != nil {
		return MemoryParams{}, fmt.Errorf("%w: %v", errHashMismatch, err)
	}
	return m, nil
}

// MemorySink appends the fact of an approved memory proposal to MEMORY.md. It must be
// idempotent by proposal id (Recover calls it again after a crash) and must itself refuse
// a proposal that is not approved by the owner, or whose hash does not verify.
type MemorySink interface {
	AppendApprovedMemory(ctx context.Context, p Proposal) error
}

// MemoryDigest queues the announcement of a suggestion for the next digest. It returns an
// error unless the item was queued for the digest: delivered at once, dropped (muted),
// or only logged (shadow mode) all mean the owner would never see the suggestion.
type MemoryDigest interface {
	QueueForDigest(ctx context.Context, c proactive.Candidate) error
}

// MemoryRequest is a memory to suggest. Source is a fact about the turn, read from its
// context by the tool; the fact and category are the model's, cleaned here.
type MemoryRequest struct {
	Fact, Category, Source string
	// Suggester is who made it; the zero value is the agent itself.
	Suggester Suggester
	// Expiry replaces MemoryExpiry when positive (the nightly reflection's is shorter).
	Expiry time.Duration
}

// ProposeMemory stores a pending memory proposal and queues its announcement for the next
// digest. It delivers nothing now and writes nothing: the owner reads the fact in the
// digest and answers with Save or Discard (or /approve CODE, /reject CODE).
func (s *Service) ProposeMemory(ctx context.Context, r MemoryRequest) (Proposal, error) {
	if _, ok := s.owner(); !ok {
		return Proposal{}, fmt.Errorf("%w: no owner is configured to confirm it", ErrMemoryUnavailable)
	}
	if s.o.Memories == nil || s.o.Digest == nil {
		return Proposal{}, fmt.Errorf("%w: the daily digest that would carry it is not running", ErrMemoryUnavailable)
	}
	fact, err := CleanFact(r.Fact)
	if err != nil {
		return Proposal{}, err
	}
	mp := MemoryParams{Fact: fact, Category: r.Category, Source: r.Source}
	key := ""
	switch r.Suggester.Kind {
	case "":
	case SuggestedByOwner:
		mp.By = SuggestedByOwner
	case SuggestedByPerson:
		mp.By, mp.Who = SuggestedByPerson, SuggesterLabel(r.Suggester)
		key = suggesterKey(r.Suggester.Channel, r.Suggester.ID)
	default:
		return Proposal{}, errors.New("approval: unknown suggester")
	}
	params, err := canonicalMemory(mp)
	if err != nil {
		return Proposal{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout+30*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	// The agent's and the owner's suggestions are checked here (and named in the refusal);
	// a person's are held to their own bounds by the insert, whose refusal names nothing.
	if key == "" {
		if codes, err := s.st.pendingCodes(ctx, now, classMemory); err != nil {
			return Proposal{}, err
		} else if len(codes) >= MaxPendingMemory {
			return Proposal{}, &PendingError{Codes: codes}
		}
	}
	p := Proposal{
		IntentID: key, ID: proactive.NewID(), Nonce: newNonce(), Kind: KindMemory, Params: params, Hash: hashOf(KindMemory, params),
		Origin: OriginSuggestion, Mode: ModeLive, Lang: langOf(s.locale(ctx).Lang), Status: StatusPending,
		CreatedAt: now, ExpiresAt: now.Add(MemoryExpiry),
	}
	if r.Expiry > 0 {
		p.ExpiresAt = now.Add(r.Expiry)
	}
	for attempt := 0; ; attempt++ {
		p.Code = newCode()
		p.Summary = fmt.Sprintf("Memory suggestion %s (%s)", p.Code, r.Category) // no fact: it reaches the owner's session
		err = s.st.insert(ctx, p, now)
		if errors.Is(err, errCodeTaken) && attempt < maxCodeAttempts {
			continue
		}
		break
	}
	var bounds errBounds
	switch {
	case errors.As(err, &bounds):
		if key != "" {
			if bounds.hourly && !bounds.suggester {
				return Proposal{}, ErrMemoryDaily
			}
			return Proposal{}, ErrMemorySuggesterFull
		}
		if bounds.hourly {
			return Proposal{}, ErrMemoryDaily
		}
		codes, _ := s.st.pendingCodes(ctx, now, classMemory)
		return Proposal{}, &PendingError{Codes: codes}
	case err != nil:
		return Proposal{}, err
	}
	err = s.o.Digest.QueueForDigest(ctx, proactive.Candidate{
		TriggerID: proactive.MemoryTriggerPrefix + p.Code, DedupKey: "memory:" + p.ID,
		Severity: proactive.SeverityInfo, Salience: 0.4, Subject: "memory", Title: p.Summary,
		Quote: fact, Source: mp.SourceText(), Trust: proactive.TrustUntrusted, Compose: proactive.ComposeTemplate, At: now,
	})
	if err != nil {
		// A suggestion the owner will never see must not hold a pending slot for a week.
		_, _ = s.st.cancel(context.WithoutCancel(ctx), p.ID, "not queued for the digest", s.now())
		return Proposal{}, fmt.Errorf("%w: %v", ErrMemoryUnavailable, err)
	}
	s.emit(events.KindApprovalCreated, p, events.SeverityInfo)
	return p, nil
}

// ShownMemory is a suggestion as a digest shows it to the owner: the code and the fact.
type ShownMemory struct{ Code, Fact string }

// MemoryActions returns the Save and Discard buttons for the pending memory suggestions
// among shown (at most MaxDigestMemoryCards of them), built from the stored proposals at
// the moment a digest is sent. A button is built only where the stored, hash-verified fact
// is the fact the owner was shown, so a Save always saves what was read. The nonce lives in
// the buttons only, never in the attention log; a button's tap takes the same path as
// /approve CODE and /reject CODE.
func (s *Service) MemoryActions(ctx context.Context, shown []ShownMemory) []bus.Action {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	var out []bus.Action
	for _, sh := range shown {
		if len(out) >= 2*MaxDigestMemoryCards {
			break
		}
		p, err := s.st.pendingByCode(ctx, NormalizeCode(sh.Code))
		if err != nil || p.Kind != KindMemory || !s.now().Before(p.ExpiresAt) {
			continue
		}
		if m, err := VerifiedMemory(p); err != nil || m.Fact != sh.Fact {
			continue
		}
		save, discard := "Save", "Discard"
		if langOf(p.Lang) == "th" {
			save, discard = "บันทึก", "ทิ้ง"
		}
		out = append(out,
			bus.Action{ID: "approve", Label: save + " " + p.Code, Style: "primary", Payload: ButtonPayload(p.Code, p.Nonce), TextCommand: "/approve " + p.Code},
			bus.Action{ID: "reject", Label: discard + " " + p.Code, Style: "danger", Payload: ButtonPayload(p.Code, p.Nonce), TextCommand: "/reject " + p.Code})
	}
	return out
}

// executeMemory runs an approved memory proposal: it appends the fact (idempotently) and
// settles the proposal. A failure leaves it executing, and Recover tries again.
func (s *Service) executeMemory(ctx context.Context, p Proposal) error {
	if s.o.Memories == nil {
		return errors.New("approval: no memory sink attached")
	}
	if _, err := s.st.markExecuting(ctx, p.ID, "memory"); err != nil {
		return err
	}
	cur, err := s.st.get(ctx, p.ID)
	if err != nil {
		return err
	}
	if cur.Status != StatusExecuting {
		return nil // already settled by another pass
	}
	if err := s.o.Memories.AppendApprovedMemory(ctx, cur); err != nil {
		logger.ErrorCF(logComponent, "SECURITY: an approved memory could not be saved", map[string]any{"code": cur.Code, "error": err.Error()})
		return err
	}
	text := "Saved to memory."
	if langOf(cur.Lang) == "th" {
		text = "บันทึกลงความจำแล้ว"
	}
	if changed, err := s.st.finish(ctx, cur.ID, StatusExecuted, text, s.now()); err != nil {
		return err
	} else if changed {
		_ = s.st.markNotified(context.WithoutCancel(ctx), cur.ID) // the control reply is the message
	}
	return nil
}
