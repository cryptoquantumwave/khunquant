// Package feedback records what the owner did with each proactive message: found it
// useful or not, snoozed or muted it, replied to it, acted on it, approved or rejected
// the proposal it carried, or did nothing at all.
//
// It RECORDS, and keeps the acceptance posteriors the rows add up to (posterior.go).
// Nothing here changes a disposition, a budget, a trigger or a delivery; attention reads
// a posterior and, only with proactive.learning.adapt on, lets it move a Notice to the
// digest.
//
// # What is stored
//
// feedback_responses(id, intervention_id, kind, value, at_ms, source). The intervention
// is the attention decision's InterventionID. value is a short code (a snooze length, a
// reply's class, "unknown" for none), never text the owner typed: the text of a reply is
// classified in memory and dropped. source names the path and the channel
// ("telegram:button", "web:reply", "order_fill", "approval"); "web" is the authenticated
// web launcher chat, the second owner route. Rows older than 180 days are deleted.
//
// # Who may
//
// The package knows no owner. Its callers apply the owner rule: the control path records
// a tap, a reaction, a typed command or a reply only from the owner (the rule it uses
// for its own commands, web route included), and the approval service only lets the owner
// decide a card. A response from anybody else is never passed to Record.
//
// # Unknown is not negative
//
// A bot gets no read receipts on most channels. An intervention that has no other row
// 24 hours after delivery gets kind "none" with value "unknown": weak evidence, not a
// verdict.
//
// A nil *Service is valid and does nothing, so a caller holding the gateway's service
// need not check for learning being off. With learning off the service is not built and
// the table does not exist.
package feedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Kind is what a response was.
type Kind string

// The kinds. KindReaction is reserved: a 👍 or 👎 is recorded as useful or not_useful with
// the reaction as its source, so that everything that counts a verdict counts it once.
const (
	KindUseful    Kind = "useful"
	KindNotUseful Kind = "not_useful"
	KindSnooze    Kind = "snooze"
	KindMute      Kind = "mute"
	KindReply     Kind = "reply"
	KindActedOn   Kind = "acted_on"
	KindReaction  Kind = "reaction"
	KindApproved  Kind = "approved"
	KindRejected  Kind = "rejected"
	KindExpired   Kind = "expired"
	KindNone      Kind = "none"
)

var validKinds = map[Kind]bool{
	KindUseful: true, KindNotUseful: true, KindSnooze: true, KindMute: true, KindReply: true, KindActedOn: true,
	KindReaction: true, KindApproved: true, KindRejected: true, KindExpired: true, KindNone: true,
}

const (
	maxValueRunes  = 64
	maxSourceRunes = 64

	// dbTimeout bounds one write or read.
	dbTimeout = 5 * time.Second
	// maxAsync bounds the background recordings in flight.
	maxAsync = 8

	// UnansweredAfter is how long an intervention waits for a response before it is
	// marked none.
	UnansweredAfter = 24 * time.Hour
	// ActedOnWindow is how long after delivery an order or a change counts as acting on it.
	ActedOnWindow = 6 * time.Hour
	// scanWindow bounds how far back the housekeeper looks for unanswered interventions.
	scanWindow     = 30 * 24 * time.Hour
	housekeepEvery = 15 * time.Minute
)

// Response is one thing the owner did.
type Response struct {
	InterventionID string
	Kind           Kind
	// Value is a short code, never typed text.
	Value string
	// Source is the path and channel that saw it, such as "telegram:button".
	Source string
	// Event identifies the event the response came from (the message a button was on,
	// the inbound message, the order). It is not stored; with the intervention, kind and
	// source it makes the row's id, so the same event recorded twice is one row.
	Event string
	// At is when it happened; zero means now.
	At time.Time
}

// Publisher is the part of the event bus the service uses.
type Publisher interface {
	PublishNonBlocking(evt events.Event) events.PublishResult
}

// Options configure New.
type Options struct {
	// Events receives feedback.recorded; nil publishes nothing.
	Events Publisher
	// Clock is the time source; nil is time.Now.
	Clock func() time.Time
	// Changes, when set, is read by the housekeeper for plan and intent changes that
	// count as acting on an alert (see ObserveChange).
	Changes ChangeSource
	// Zone is the owner's time zone, which gives the hour bucket of a posterior. It must
	// fall back the way attention does (the default profile's zone when the profile
	// cannot be read, UTC when the zone cannot be loaded); nil means UTC.
	Zone func(ctx context.Context) *time.Location
}

// Change is a plan or an intent that changed.
type Change struct {
	// Source is ChangePlan or ChangeIntent.
	Source string
	// Subject is what it is about: a market symbol or "plan:12".
	Subject string
	// Event identifies the change, so that seeing it twice records once.
	Event string
	At    time.Time
}

// ChangeSource lists the changes made since a time. The gateway supplies one over the
// intent ledger.
type ChangeSource interface {
	ChangesSince(ctx context.Context, since time.Time) ([]Change, error)
}

// Service records responses. Build one with New only when learning is effectively on.
type Service struct {
	db      *sql.DB
	pub     Publisher
	clock   func() time.Time
	since   time.Time
	changes ChangeSource
	polled  time.Time // changes up to here have been looked at; guarded by mu
	post    *Store

	sem chan struct{}
	wg  sync.WaitGroup

	mu     sync.Mutex
	sub    events.Subscription
	cancel context.CancelFunc
	done   chan struct{}
}

// New migrates the table and returns the service. db is the shared handle from sqlitex.
func New(ctx context.Context, db *sql.DB, o Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("feedback: nil database")
	}
	since, err := migrate(ctx, db)
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, pub: o.Events, clock: o.Clock, since: since, changes: o.Changes, sem: make(chan struct{}, maxAsync)}
	if s.clock == nil {
		s.clock = time.Now
	}
	s.polled = s.clock()
	s.post = &Store{db: db, clock: s.clock, zone: o.Zone, pub: o.Events}
	return s, nil
}

// Record stores r and reports whether it was new (false for a repeat of the same
// event). The caller has already applied the owner rule.
func (s *Service) Record(ctx context.Context, r Response) (bool, error) {
	if s == nil {
		return false, nil
	}
	r.InterventionID = strings.TrimSpace(r.InterventionID)
	r.Source = clip(strings.TrimSpace(r.Source), maxSourceRunes)
	r.Value = clip(strings.TrimSpace(r.Value), maxValueRunes)
	if r.InterventionID == "" || r.Source == "" || !validKinds[r.Kind] {
		return false, errors.New("feedback: a response needs an intervention, a source and a known kind")
	}
	at := r.At
	if at.IsZero() {
		at = s.clock()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	id := responseID(r)
	added, err := insert(ctx, s.db, id, r, at)
	if err != nil || !added {
		return false, err
	}
	// The response teaches the acceptance posterior, once. A failure here is logged: the
	// response itself is recorded, and a later Update for the intervention applies it.
	if err := s.post.Update(ctx, r.InterventionID); err != nil {
		logger.WarnCF("feedback", "Could not update the acceptance posterior", map[string]any{"error": err.Error()})
	}
	if s.pub != nil {
		s.pub.PublishNonBlocking(events.Event{
			Kind:     events.KindFeedbackRecorded,
			Source:   events.Source{Component: Component},
			Severity: events.SeverityInfo,
			Payload:  map[string]any{"id": id, "intervention_id": r.InterventionID, "kind": string(r.Kind)},
		})
	}
	return true, nil
}

// Async runs f on a goroutine of its own with a bounded context, for a recording that
// must not delay what triggered it (a reply on its way to the agent). When the bound on
// recordings in flight is reached, f is dropped. It never blocks.
func (s *Service) Async(f func(ctx context.Context)) {
	if s == nil {
		return
	}
	select {
	case s.sem <- struct{}{}:
	default:
		logger.DebugCF("feedback", "A recording was dropped: too many in flight", nil)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.sem }()
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorCF("feedback", "A recording panicked", nil)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		f(ctx)
	}()
}

// Wait blocks until the recordings started by Async are done.
func (s *Service) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

func responseID(r Response) string {
	h := sha256.Sum256([]byte(r.InterventionID + "\x00" + string(r.Kind) + "\x00" + r.Source + "\x00" + r.Event))
	return hex.EncodeToString(h[:16])
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
