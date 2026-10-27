// Package notify is the single exit door for anything the assistant says on its
// own initiative.
//
// It guarantees four things:
//
//  1. Proactive output goes to the configured owner's direct chat. A caller
//     cannot name a destination, and there is no fallback to the "last active"
//     chat or to any group: with no owner, nothing is sent.
//  2. The message is written into the owner's conversation session, so that a
//     reply such as "yes, do it" has a referent. The write is asynchronous and
//     never delays delivery.
//  3. The platform message IDs are recorded, for reply correlation.
//  4. The delivery is recorded, for audit.
//
// The package imports no agent code. The session write goes through the small
// SessionAppender interface, which *agent.AgentLoop satisfies.
package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
)

const (
	// Reasons reported in Result.Reason.
	ReasonDisabled   = "disabled"
	ReasonNoRoute    = "no-route"
	ReasonClosed     = "closed"
	reasonSendFailed = "send-failed"
	reasonPartial    = "partial"

	// partialMarker ends the session copy of a message the owner received only
	// in part.
	partialMarker = "[delivery was partial]"

	// writeBackQueueSize bounds the pending session writes of one service.
	writeBackQueueSize = 64
	// dbTimeout bounds the bookkeeping that follows a successful send.
	dbTimeout = 5 * time.Second
	// noRouteWarnEvery limits the "no owner route" warning.
	noRouteWarnEvery = time.Hour
	// pruneTimeout bounds the retention sweep at construction.
	pruneTimeout = 10 * time.Second
)

// Notification is one thing to say to the owner. It deliberately has no
// channel or chat field: the destination is the owner and nothing else.
type Notification struct {
	// Candidate is the situation that produced this notification, kept for
	// audit logging only.
	Candidate *proactive.Candidate
	Severity  proactive.Severity
	// Title is the template headline (never model text).
	Title string
	// Body is the template evidence block plus optional prose.
	Body    string
	Actions []bus.Action
	// DedupKey is the audit identity used to collapse repeats. Never logged
	// above DEBUG.
	DedupKey string
	// InterventionID is the attention decision ID. A fresh one is generated
	// when it is empty or not made of ULID characters.
	InterventionID string
	// SessionBody, when set, replaces Body in the copy written into the owner's
	// session; the message the owner is sent is unchanged. The model reads the
	// session on the owner's next turn, so a body that quotes text from outside
	// (an external trigger's message) is kept out of it by giving the session a
	// body without that text. nil means the session copy carries Body.
	SessionBody *string
	// WriteToSession is true by default (nil).
	WriteToSession      *bool
	DisableNotification bool
}

// Result is the outcome of one Deliver call.
type Result struct {
	Delivered bool
	// Reason is "" on success. Otherwise "disabled", "no-route", "closed",
	// "send-failed: <class>" (Delivered is false) or "partial: <class>"
	// (Delivered is true: the owner got part of the message; see Deliver).
	Reason             string
	Route              proactive.OwnerRoute
	SessionKey         string
	PlatformMessageIDs []string
	InterventionID     string
}

// Sender delivers a message and reports what happened. *channels.Manager
// satisfies it.
type Sender interface {
	SendAndReport(ctx context.Context, msg bus.OutboundMessage) channels.DeliveryReceipt
}

// SessionAppender adds an assistant message to a session. *agent.AgentLoop
// satisfies it. It must take the session's lock and honour ctx while waiting.
type SessionAppender interface {
	AppendAssistantMessage(ctx context.Context, sessionKey, content string) error
}

type writeBack struct {
	interventionID string
	sessionKey     string
	content        string
}

// Service delivers notifications to the owner. Build one with New; it owns a
// goroutine that must be stopped with Close. A nil *Service is a valid, disabled
// service: Deliver reports "disabled" and the rest are no-ops, so a caller that
// holds the gateway's service need not nil-check it.
type Service struct {
	cfg      *config.Config
	sender   Sender
	appender SessionAppender
	store    *store
	clock    func() time.Time

	// ownerKey is the owner's session key when proactive output can be
	// delivered, "" otherwise. cfg is not changed after construction, so it
	// is computed once.
	ownerKey string

	queue    chan writeBack
	mu       sync.Mutex // guards closed and sends on queue
	closed   bool
	runCtx   context.Context
	runStop  context.CancelFunc
	workerWG sync.WaitGroup

	warnMu       sync.Mutex
	lastNoRoute  time.Time
	droppedWrite atomic.Int64
	failedWrite  atomic.Int64
}

// New builds a Service for cfg. s sends the message; a writes it into the
// owner's session (nil disables the write-back); db holds notify_sent and must
// be a handle from sqlitex (New migrates its table and prunes rows older than
// 90 days). defaultAgentID is the agent that handles the owner's direct
// messages when no binding claims them. clock may be nil.
//
// A Service reads cfg once: build a new one after a config reload.
func New(
	cfg *config.Config,
	s Sender,
	a SessionAppender,
	db *sql.DB,
	defaultAgentID string,
	clock func() time.Time,
) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("notify: nil config")
	}
	if s == nil {
		return nil, errors.New("notify: nil sender")
	}
	if db == nil {
		return nil, errors.New("notify: nil database")
	}
	if clock == nil {
		//simclock:wall the default when no clock is injected
		clock = time.Now
	}

	ctx, cancel := context.WithTimeout(context.Background(), pruneTimeout)
	defer cancel()
	st, err := newStore(ctx, db)
	if err != nil {
		return nil, err
	}
	if n, err := st.prune(ctx, clock().Add(-retention)); err != nil {
		logger.WarnCF("notify", "Pruning old delivery records failed", map[string]any{"error": err.Error()})
	} else if n > 0 {
		logger.InfoCF("notify", "Pruned old delivery records", map[string]any{"rows": n})
	}

	svc := &Service{
		cfg:      cfg,
		sender:   s,
		appender: a,
		store:    st,
		clock:    clock,
		queue:    make(chan writeBack, writeBackQueueSize),
	}
	if route, ok := svc.OwnerRoute(); ok {
		svc.ownerKey = ownerSessionKey(cfg, route, defaultAgentID)
	}
	svc.runCtx, svc.runStop = context.WithCancel(context.Background())
	svc.workerWG.Add(1)
	go svc.writeBackWorker()
	return svc, nil
}

// OwnerRoute returns where proactive output goes: the configured owner, and
// only when proactive delivery is enabled and the owner is valid.
func (s *Service) OwnerRoute() (proactive.OwnerRoute, bool) {
	if s == nil {
		return proactive.OwnerRoute{}, false
	}
	eff := s.cfg.Proactive.Effective()
	if !eff.CanDeliver() {
		return proactive.OwnerRoute{}, false
	}
	return eff.Owner, true
}

// CanDeliver reports whether Deliver can currently reach the owner.
func (s *Service) CanDeliver() bool {
	_, ok := s.OwnerRoute()
	return ok
}

// OwnerSessionKey returns the key of the owner's direct-message session, the
// one an inbound message from the owner would use. It is "" when there is no
// deliverable owner.
func (s *Service) OwnerSessionKey() string {
	if s == nil {
		return ""
	}
	return s.ownerKey
}

// ownerSessionKey derives the session key an inbound direct message from the
// owner gets. It runs the real route resolver with the same inputs the agent
// loop gives it for a direct message (channel, a direct peer whose ID is the
// sender's platform ID, no account), so bindings, the DM scope and identity
// links all apply exactly as they do to the owner's own messages.
func ownerSessionKey(cfg *config.Config, route proactive.OwnerRoute, defaultAgentID string) string {
	resolved := routing.NewRouteResolver(cfg).ResolveRoute(routing.RouteInput{
		Channel: route.Channel,
		Peer:    &routing.RoutePeer{Kind: "direct", ID: route.PeerID},
	})
	// No binding claims the owner: the caller's default agent decides.
	if want := routing.NormalizeAgentID(defaultAgentID); defaultAgentID != "" &&
		resolved.MatchedBy == "default" && want != resolved.AgentID {
		return strings.ToLower(routing.BuildAgentPeerSessionKey(routing.SessionKeyParams{
			AgentID:       want,
			Channel:       resolved.Channel,
			AccountID:     resolved.AccountID,
			Peer:          &routing.RoutePeer{Kind: "direct", ID: route.PeerID},
			DMScope:       dmScope(cfg),
			IdentityLinks: cfg.Session.IdentityLinks,
		}))
	}
	return resolved.SessionKey
}

func dmScope(cfg *config.Config) routing.DMScope {
	if cfg.Session.DMScope == "" {
		return routing.DMScopeMain
	}
	return routing.DMScope(cfg.Session.DMScope)
}

// Deliver sends n to the owner's direct chat, then (asynchronously) writes it
// into the owner's session and records the platform message IDs.
//
// A message that cannot or must not be sent is reported in Result, not as an
// error: Delivered is false with Reason "disabled" (proactive is off),
// "no-route" (no valid owner), "closed" (the service was shut down) or
// "send-failed: <class>". A multi-part message whose later part fails after an
// earlier one went out is Delivered with Reason "partial: <class>": the IDs
// that were sent are recorded and the session copy ends with a partial marker.
// That needs the channel to report platform message IDs; one that reports none
// yields send-failed for any error. The error return is for a malformed Notification
// (nothing to say), in which case nothing is sent.
func (s *Service) Deliver(ctx context.Context, n Notification) (Result, error) {
	if s == nil {
		return Result{Reason: ReasonDisabled}, nil
	}
	eff := s.cfg.Proactive.Effective()
	switch {
	case !eff.Enabled:
		return Result{Reason: ReasonDisabled}, nil
	case !eff.CanDeliver():
		s.warnNoRoute()
		return Result{Reason: ReasonNoRoute}, nil
	}
	route := eff.Owner

	id := n.InterventionID
	if !validInterventionID(id) {
		id = proactive.NewID()
	}
	res := Result{Route: route, SessionKey: s.ownerKey, InterventionID: id}

	text, ok := render(n.Severity, id, n.Title, n.Body)
	if !ok {
		return res, errors.New("notify: notification has neither title nor body")
	}

	if n.SessionBody != nil {
		text.Session = sessionCopy(n.Severity, id, cleanTitle(n.Title), cleanBody(*n.SessionBody))
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		res.Reason = ReasonClosed
		return res, nil
	}

	receipt := s.sender.SendAndReport(ctx, bus.OutboundMessage{
		Channel:             route.Channel,
		ChatID:              route.ChatID,
		Content:             text.Outbound,
		Severity:            n.Severity,
		DedupKey:            n.DedupKey,
		InterventionID:      id,
		Actions:             n.Actions,
		DisableNotification: n.DisableNotification,
	})
	// A receipt that carries an error AND at least one platform message ID is a
	// partial delivery: a later chunk failed after the owner already received
	// the first. It counts as delivered, so the caller does not send it again
	// and a reply to the part the owner saw can be correlated. A channel that
	// reports no IDs cannot tell a partial failure from a total one, so there
	// an error is always send-failed.
	partial := receipt.Err != nil && len(receipt.PlatformMessageIDs) > 0
	if receipt.Err != nil && !partial {
		class := classifySendError(receipt.Err)
		res.Reason = reasonSendFailed + ": " + class
		logger.WarnCF("notify", "Proactive delivery failed", map[string]any{
			"intervention_id": id,
			"channel":         route.Channel,
			"class":           class,
		})
		logger.DebugCF("notify", "Proactive delivery failure detail", map[string]any{
			"intervention_id": id,
			"error":           receipt.Err.Error(),
		})
		return res, nil
	}

	res.Delivered = true
	res.PlatformMessageIDs = append([]string(nil), receipt.PlatformMessageIDs...)
	sessionText := text.Session
	if partial {
		class := classifySendError(receipt.Err)
		res.Reason = reasonPartial + ": " + class
		sessionText += "\n" + partialMarker
		logger.WarnCF("notify", "Proactive delivery was partial", map[string]any{
			"intervention_id": id,
			"channel":         route.Channel,
			"class":           class,
			"delivered_ids":   len(res.PlatformMessageIDs),
		})
		logger.DebugCF("notify", "Proactive delivery partial failure detail", map[string]any{
			"intervention_id": id,
			"error":           receipt.Err.Error(),
		})
	}
	s.logDelivered(n, route, id)

	// The user already has the message: nothing below may undo that.
	s.recordSent(ctx, route, id, res.PlatformMessageIDs)
	if (n.WriteToSession == nil || *n.WriteToSession) && s.appender != nil && s.ownerKey != "" {
		s.enqueueWriteBack(writeBack{interventionID: id, sessionKey: s.ownerKey, content: sessionText})
	}
	return res, nil
}

// LookupByPlatformMessageID maps the platform message ID a reply points at back
// to the intervention that sent it.
func (s *Service) LookupByPlatformMessageID(
	ctx context.Context,
	channel, platformMsgID string,
) (interventionID string, ok bool, err error) {
	return s.store.lookup(ctx, strings.ToLower(strings.TrimSpace(channel)), platformMsgID)
}

// DroppedWriteBacks counts session writes dropped because the queue was full.
func (s *Service) DroppedWriteBacks() int64 { return s.droppedWrite.Load() }

// FailedWriteBacks counts session writes the appender rejected.
func (s *Service) FailedWriteBacks() int64 { return s.failedWrite.Load() }

func (s *Service) logDelivered(n Notification, route proactive.OwnerRoute, id string) {
	logger.InfoCF("notify", "Proactive message delivered", map[string]any{
		"intervention_id": id,
		"channel":         route.Channel,
		"severity":        n.Severity.String(),
	})
	fields := map[string]any{"intervention_id": id, "dedup_key": n.DedupKey}
	if n.Candidate != nil {
		fields["candidate_id"] = n.Candidate.ID
		fields["trigger_id"] = n.Candidate.TriggerID
	}
	logger.DebugCF("notify", "Proactive message detail", fields)
}

// recordSent writes the audit rows. A failure is logged and swallowed.
func (s *Service) recordSent(ctx context.Context, route proactive.OwnerRoute, id string, ids []string) {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	if err := s.store.record(dbCtx, id, route.Channel, route.ChatID, ids, s.clock()); err != nil {
		logger.ErrorCF("notify", "Recording the delivery failed; the message was delivered", map[string]any{
			"intervention_id": id,
			"error":           err.Error(),
		})
	}
}

// warnNoRoute logs that proactive output has nowhere to go, at most once an
// hour so a busy trigger cannot flood the log. It reports whether it logged.
func (s *Service) warnNoRoute() bool {
	now := s.clock()
	s.warnMu.Lock()
	due := s.lastNoRoute.IsZero() || now.Sub(s.lastNoRoute) >= noRouteWarnEvery
	if due {
		s.lastNoRoute = now
	}
	s.warnMu.Unlock()
	if due {
		logger.WarnCF("notify", "Proactive output dropped: proactive is enabled but no valid owner is configured", nil)
	}
	return due
}

// enqueueWriteBack queues a session write without ever blocking the caller.
func (s *Service) enqueueWriteBack(w writeBack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- w:
	default:
		n := s.droppedWrite.Add(1)
		logger.WarnCF("notify", "Session write-back queue full; message delivered but not added to the session",
			map[string]any{"dropped_total": n})
	}
}

// writeBackWorker applies queued session writes one at a time. Each waits for
// the session's lock inside the appender, so it never splits a running turn.
func (s *Service) writeBackWorker() {
	defer s.workerWG.Done()
	for w := range s.queue {
		if s.runCtx.Err() != nil {
			s.failedWrite.Add(1)
			continue
		}
		if err := s.appender.AppendAssistantMessage(s.runCtx, w.sessionKey, w.content); err != nil {
			s.failedWrite.Add(1)
			// The error can carry the session file path, which embeds the
			// owner's peer ID: only a class goes above DEBUG.
			logger.WarnCF("notify", "Session write-back failed", map[string]any{
				"intervention_id": w.interventionID,
				"class":           classifyWriteError(err),
			})
			logger.DebugCF("notify", "Session write-back failure detail", map[string]any{
				"intervention_id": w.interventionID,
				"error":           err.Error(),
			})
		}
	}
}

// Close stops accepting work and lets the write-back worker finish what is
// queued, for as long as ctx allows. When ctx ends first, pending writes are
// abandoned and an in-flight one is cancelled. It returns ctx.Err() in that
// case and is safe to call more than once.
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.workerWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.runStop()
		return nil
	case <-ctx.Done():
	}
	s.runStop()
	select {
	case <-done:
	//simclock:wall a shutdown wait, bounded in wall time
	case <-time.After(2 * time.Second):
		logger.WarnCF("notify", "Write-back worker did not stop after cancellation", nil)
	}
	return fmt.Errorf("notify: close: %w", ctx.Err())
}

// classifyWriteError reduces a session write error to a short class.
func classifyWriteError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}

// classifySendError reduces a delivery error to a short, secret-free class.
func classifySendError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, channels.ErrRateLimit):
		return "rate-limited"
	case errors.Is(err, channels.ErrNotRunning):
		return "channel-not-running"
	case errors.Is(err, channels.ErrTemporary):
		return "temporary"
	case errors.Is(err, channels.ErrSendFailed):
		return "send-error"
	}
	msg := err.Error()
	if strings.Contains(msg, "not found") || strings.Contains(msg, "no active worker") {
		return "channel-unavailable"
	}
	return "error"
}
