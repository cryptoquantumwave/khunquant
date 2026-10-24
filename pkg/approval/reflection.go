package approval

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The three kinds the nightly reflection (pkg/reflection) proposes besides a memory
// (KindMemory, which keeps its own sink). Approving one makes CODE apply exactly the
// stored change through a ReflectionSink; rejecting it, or letting it expire, changes
// nothing. None of them can place an order, create a mandate or make anything active.
const (
	// KindTriggerParam changes one tunable number of one trigger spec.
	KindTriggerParam Kind = "trigger_param"
	// KindIntentCleanup pauses or cancels one intent. It never activates or creates one.
	KindIntentCleanup Kind = "intent_cleanup"
	// KindMute mutes one trigger kind's alerts for at most MaxMuteDays.
	KindMute Kind = "mute"
)

// OriginReflection marks a proposal made by the nightly reflection.
const OriginReflection Origin = "reflection"

// Reflection bounds, counted apart from orders, mandates and memories.
const (
	MaxPendingReflection = 5
	MaxReflectionPerDay  = 10
	// ReflectionExpiry is how long the owner has to decide (the digest is read the next morning).
	ReflectionExpiry = 48 * time.Hour
	MaxMuteDays      = 30
	maxIntentTitle   = 60
)

// Intent cleanup actions.
const (
	CleanupPause  = "pause"
	CleanupCancel = "cancel"
)

// ErrReflectionRefused is wrapped by a sink that will never apply the change (the
// value moved since it was proposed, the intent has ended): the proposal fails
// instead of being retried.
var ErrReflectionRefused = errors.New("approval: the change can no longer be applied")

// ErrReflectionDuplicate: the same change is already waiting for the owner's decision.
var ErrReflectionDuplicate = errors.New("approval: the same change is already waiting for a decision")

// ErrReflectionUnavailable: reflection proposals cannot be queued.
var ErrReflectionUnavailable = errors.New("approval: reflection proposals are not available")

// TriggerParamParams changes Param of trigger spec SpecID from From to To. From is
// compared at apply time, so a change the owner made meanwhile is never overwritten.
type TriggerParamParams struct {
	SpecID string  `json:"spec_id"`
	Kind   string  `json:"kind"`
	Param  string  `json:"param"`
	From   float64 `json:"from"`
	To     float64 `json:"to"`
}

// IntentCleanupParams pauses or cancels an intent. Title is a snapshot for the card.
type IntentCleanupParams struct {
	IntentID string `json:"intent_id"`
	Action   string `json:"action"`
	Title    string `json:"title"`
}

// MuteParams mutes the alerts whose key starts with Prefix for Days, from approval.
type MuteParams struct {
	Prefix string `json:"prefix"`
	Days   int    `json:"days"`
}

func cleanLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// CleanLine reduces s to one clean line of at most max characters.
func CleanLine(s string, max int) string { return cleanLine(s, max) }

// CleanReflectionRationale reduces the model's prose to one clean line of at most
// MaxRationaleRunes characters. It is shown under "Why" and never parsed.
func CleanReflectionRationale(s string) string { return cleanLine(s, MaxRationaleRunes) }

func okToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-.:/@", r)) {
			return false
		}
	}
	return true
}

func (p TriggerParamParams) validate() error {
	switch {
	case !okToken(p.SpecID, 64) || !okToken(p.Kind, 48) || !okToken(p.Param, 48):
		return errors.New("the trigger, kind and parameter must be plain tokens")
	case math.IsNaN(p.From) || math.IsInf(p.From, 0) || math.IsNaN(p.To) || math.IsInf(p.To, 0):
		return errors.New("from and to must be finite numbers")
	case p.From == p.To:
		return errors.New("the change does not change anything")
	}
	return nil
}

func (p IntentCleanupParams) validate() error {
	switch {
	case !okToken(p.IntentID, 64):
		return errors.New("the intent id must be a plain token")
	case p.Action != CleanupPause && p.Action != CleanupCancel:
		return errors.New("action must be pause or cancel")
	case cleanLine(p.Title, maxIntentTitle) != p.Title:
		return errors.New("the title is not a clean short line")
	}
	return nil
}

func (p MuteParams) validate() error {
	if !okToken(p.Prefix, 64) || len(p.Prefix) < 3 || p.Days < 1 || p.Days > MaxMuteDays {
		return fmt.Errorf("a mute needs a prefix of at least 3 plain characters and 1 to %d days", MaxMuteDays)
	}
	return nil
}

func canonicalReflection(v interface{ validate() error }) (json.RawMessage, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// verifiedReflection decodes a stored proposal's params into dst and proves they are
// exactly what was hashed (the proof VerifiedMemory gives a memory).
func verifiedReflection(p Proposal, kind Kind, dst interface{ validate() error }) error {
	if p.Kind != kind {
		return fmt.Errorf("approval: proposal %s is not a %s proposal", p.Code, kind)
	}
	want := hashOf(p.Kind, p.Params)
	if subtle.ConstantTimeCompare(want[:], p.Hash[:]) != 1 {
		return errHashMismatch
	}
	dec := json.NewDecoder(bytes.NewReader(p.Params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil || dec.More() {
		return errHashMismatch
	}
	if again, err := json.Marshal(dst); err != nil || !bytes.Equal(again, p.Params) {
		return errHashMismatch
	}
	if err := dst.validate(); err != nil {
		return fmt.Errorf("%w: %v", errHashMismatch, err)
	}
	return nil
}

// VerifiedTriggerParam, VerifiedIntentCleanup and VerifiedMute return the verified params.
func VerifiedTriggerParam(p Proposal) (TriggerParamParams, error) {
	var v TriggerParamParams
	err := verifiedReflection(p, KindTriggerParam, &v)
	return v, err
}

func VerifiedIntentCleanup(p Proposal) (IntentCleanupParams, error) {
	var v IntentCleanupParams
	err := verifiedReflection(p, KindIntentCleanup, &v)
	return v, err
}

func VerifiedMute(p Proposal) (MuteParams, error) {
	var v MuteParams
	err := verifiedReflection(p, KindMute, &v)
	return v, err
}

// IsReflectionKind reports whether k is one of the three kinds above.
func IsReflectionKind(k Kind) bool {
	return k == KindTriggerParam || k == KindIntentCleanup || k == KindMute
}

func trimNum(v float64) string { return fmt.Sprintf("%g", v) }

// DescribeReflection is the one line the owner reads for a stored reflection proposal,
// built by code from its verified params. It is what the digest shows and what the
// buttons are built against.
func DescribeReflection(p Proposal) (string, error) {
	switch p.Kind {
	case KindTriggerParam:
		v, err := VerifiedTriggerParam(p)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Change %s of trigger %s (%s) from %s to %s", v.Param, v.SpecID, v.Kind, trimNum(v.From), trimNum(v.To)), nil
	case KindIntentCleanup:
		v, err := VerifiedIntentCleanup(p)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s intent %s \"%s\"", map[string]string{CleanupPause: "Pause", CleanupCancel: "Cancel"}[v.Action], v.IntentID, v.Title), nil
	case KindMute:
		v, err := VerifiedMute(p)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Mute alerts starting \"%s\" for %d days", v.Prefix, v.Days), nil
	}
	return "", fmt.Errorf("approval: proposal %s is not a reflection proposal", p.Code)
}

// ReflectionSink applies an approved reflection proposal through the stores that own
// the thing changed. It must be idempotent by proposal id (Recover calls it again after
// a crash), must itself refuse a proposal that is not approved by the owner or whose
// hash does not verify, and wraps ErrReflectionRefused when it never can apply it.
type ReflectionSink interface {
	ApplyApprovedReflection(ctx context.Context, p Proposal) error
}

// ReflectionRequest is one validated change. Exactly one of the three is set.
type ReflectionRequest struct {
	Trigger   *TriggerParamParams
	Intent    *IntentCleanupParams
	Mute      *MuteParams
	Rationale string
}

func (r ReflectionRequest) kindAndParams() (Kind, json.RawMessage, error) {
	var (
		k Kind
		v interface{ validate() error }
	)
	switch {
	case r.Trigger != nil && r.Intent == nil && r.Mute == nil:
		k, v = KindTriggerParam, r.Trigger
	case r.Intent != nil && r.Trigger == nil && r.Mute == nil:
		k, v = KindIntentCleanup, r.Intent
	case r.Mute != nil && r.Trigger == nil && r.Intent == nil:
		k, v = KindMute, r.Mute
	default:
		return "", nil, errors.New("approval: a reflection request carries exactly one change")
	}
	params, err := canonicalReflection(v)
	return k, params, err
}

// ProposeReflection stores a pending reflection proposal and queues its announcement for
// the next digest. It delivers nothing now and changes nothing: the owner reads the
// change in the digest and answers Save or Discard (or /approve CODE, /reject CODE).
func (s *Service) ProposeReflection(ctx context.Context, r ReflectionRequest) (Proposal, error) {
	if _, ok := s.owner(); !ok {
		return Proposal{}, fmt.Errorf("%w: no owner is configured to confirm it", ErrReflectionUnavailable)
	}
	if s.o.Reflections == nil || s.o.Digest == nil {
		return Proposal{}, fmt.Errorf("%w: the daily digest that would carry it is not running", ErrReflectionUnavailable)
	}
	kind, params, err := r.kindAndParams()
	if err != nil {
		return Proposal{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout+30*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if codes, err := s.st.pendingCodes(ctx, now, classReflection); err != nil {
		return Proposal{}, err
	} else if len(codes) >= MaxPendingReflection {
		return Proposal{}, &PendingError{Codes: codes}
	}
	if dup, err := s.st.pendingWithHash(ctx, hashOf(kind, params), now); err != nil {
		return Proposal{}, err
	} else if dup {
		return Proposal{}, ErrReflectionDuplicate
	}
	p := Proposal{
		ID: proactive.NewID(), Nonce: newNonce(), Kind: kind, Params: params, Hash: hashOf(kind, params),
		Rationale: CleanReflectionRationale(r.Rationale), Origin: OriginReflection, Mode: ModeLive,
		Lang: langOf(s.locale(ctx).Lang), Status: StatusPending, CreatedAt: now, ExpiresAt: now.Add(ReflectionExpiry),
	}
	var desc string
	for attempt := 0; ; attempt++ {
		p.Code = newCode()
		p.Summary = fmt.Sprintf("Suggestion %s (%s)", p.Code, kind) // no model text: it reaches the owner's session
		if desc, err = DescribeReflection(p); err != nil {
			return Proposal{}, err
		}
		err = s.st.insert(ctx, p, now)
		if errors.Is(err, errCodeTaken) && attempt < maxCodeAttempts {
			continue
		}
		break
	}
	var bounds errBounds
	switch {
	case errors.As(err, &bounds):
		if bounds.hourly {
			return Proposal{}, ErrMemoryDaily
		}
		codes, _ := s.st.pendingCodes(ctx, now, classReflection)
		return Proposal{}, &PendingError{Codes: codes}
	case err != nil:
		return Proposal{}, err
	}
	err = s.o.Digest.QueueForDigest(ctx, proactive.Candidate{
		TriggerID: proactive.ReflectionTriggerPrefix + p.Code, DedupKey: "reflection:" + p.ID,
		Severity: proactive.SeverityInfo, Salience: 0.4, Subject: "reflection", Title: p.Summary,
		Quote: desc, Evidence: map[string]any{"why": p.Rationale}, Trust: proactive.TrustUntrusted,
		Compose: proactive.ComposeTemplate, At: now,
	})
	if err != nil {
		_, _ = s.st.cancel(context.WithoutCancel(ctx), p.ID, "not queued for the digest", s.now())
		return Proposal{}, fmt.Errorf("%w: %v", ErrReflectionUnavailable, err)
	}
	s.emit(events.KindApprovalCreated, p, events.SeverityInfo)
	return p, nil
}

// ShownReflection is a suggestion as a digest shows it: the code and the described change.
type ShownReflection struct{ Code, Change string }

// ReflectionActions returns the Save and Discard buttons for the pending reflection
// proposals among shown, at most max buttons, built from the stored proposals at the
// moment the digest is sent: a button exists only where the stored, hash-verified change
// is the change the owner was shown. A suggestion left without buttons is still answered
// with /approve CODE and /reject CODE, which the digest prints.
func (s *Service) ReflectionActions(ctx context.Context, shown []ShownReflection, max int) []bus.Action {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	var out []bus.Action
	for _, sh := range shown {
		if len(out)+2 > max {
			break
		}
		p, err := s.st.pendingByCode(ctx, NormalizeCode(sh.Code))
		if err != nil || !IsReflectionKind(p.Kind) || !s.now().Before(p.ExpiresAt) {
			continue
		}
		if d, err := DescribeReflection(p); err != nil || d != sh.Change {
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

// executeReflection runs an approved reflection proposal through the sink and settles it.
// A refusal the sink will never lift fails the proposal; any other error leaves it
// executing for Recover to try again.
func (s *Service) executeReflection(ctx context.Context, p Proposal) error {
	if s.o.Reflections == nil {
		return errors.New("approval: no reflection sink attached")
	}
	if _, err := s.st.markExecuting(ctx, p.ID, "reflection"); err != nil {
		return err
	}
	cur, err := s.st.get(ctx, p.ID)
	if err != nil {
		return err
	}
	if cur.Status != StatusExecuting {
		return nil // already settled by another pass
	}
	if err := s.o.Reflections.ApplyApprovedReflection(ctx, cur); err != nil {
		if !errors.Is(err, ErrReflectionRefused) {
			logger.ErrorCF(logComponent, "SECURITY: an approved reflection change could not be applied", map[string]any{"code": cur.Code, "error": err.Error()})
			return err
		}
		logger.WarnCF(logComponent, "An approved reflection change was refused", map[string]any{"code": cur.Code, "error": err.Error()})
		if changed, ferr := s.st.finish(ctx, cur.ID, StatusFailed, "Not applied: "+cleanLine(err.Error(), 200), s.now()); ferr == nil && changed {
			_ = s.st.markNotified(context.WithoutCancel(ctx), cur.ID)
		}
		return nil
	}
	if changed, err := s.st.finish(ctx, cur.ID, StatusExecuted, "Applied.", s.now()); err != nil {
		return err
	} else if changed {
		_ = s.st.markNotified(context.WithoutCancel(ctx), cur.ID)
	}
	return nil
}
