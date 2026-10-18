package approval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	logComponent = "approval"
	// SweepEvery is how often the sweeper expires and heals.
	SweepEvery = 30 * time.Second
	// pruneEvery is how often finished rows past retention are pruned.
	pruneEvery = time.Hour
	pruneBatch = 100
	// storeTimeout bounds one database operation made on behalf of a caller.
	storeTimeout = 15 * time.Second
	// maxCodeAttempts bounds the retries when a random code is taken.
	maxCodeAttempts = 12
)

// Options are the dependencies of a Service. Cfg, DB, Backend and Notifier are required.
type Options struct {
	DB       *sql.DB
	Cfg      *config.Config
	Backend  OrderBackend
	Notifier Notifier
	// Editor edits delivered cards on a decision and on expiry; nil leaves them.
	Editor CardEditor
	// Killer is the kill switch; nil means never killed.
	Killer Killer
	// Mandates creates the mandate row of an approved mandate proposal; nil means
	// mandate proposals cannot be made or approved.
	Mandates MandateSink
	// Memories appends the fact of an approved memory proposal to MEMORY.md, and Digest
	// queues the announcement of a suggestion for the next digest; memory suggestions
	// can be made only while both are set.
	Memories MemorySink
	Digest   MemoryDigest
	// Clock is the time source for expiry, staleness and waits; nil is the wall clock.
	Clock journal.Clock
	// Events receives approval.created, approval.decided and approval.expired.
	Events *events.EventBus
	// Locale words and times a card; nil is English in UTC.
	Locale func(ctx context.Context) Locale
	// VerifyPolls and VerifyPollInterval bound the wait for a market order to fill
	// (defaults 5 and 1s). The interval runs on Clock.
	VerifyPolls        int
	VerifyPollInterval time.Duration
}

// Service owns the proposals table and the approval.order workflow. Build one
// with New; run its sweeper with Run.
type Service struct {
	o  Options
	st *store

	// mu serialises Propose in this process, so the friendly bound messages and
	// the insert agree; the insert itself enforces the bounds in one statement.
	mu sync.Mutex

	wfMu sync.RWMutex
	wf   Workflows

	pruneMu   sync.Mutex
	lastPrune time.Time
}

// New builds a Service and migrates its table.
func New(ctx context.Context, o Options) (*Service, error) {
	switch {
	case o.Cfg == nil:
		return nil, errors.New("approval: nil config")
	case o.Backend == nil:
		return nil, errors.New("approval: nil order backend")
	case o.Notifier == nil:
		return nil, errors.New("approval: nil notifier")
	}
	st, err := newStore(ctx, o.DB)
	if err != nil {
		return nil, err
	}
	if o.Clock == nil {
		o.Clock = journal.RealClock{}
	}
	if o.VerifyPolls <= 0 {
		o.VerifyPolls = 5
	}
	if o.VerifyPollInterval == 0 {
		o.VerifyPollInterval = time.Second
	}
	return &Service{o: o, st: st}, nil
}

// SetWorkflows attaches the journal engine. The engine is built from the
// service's Definitions, so it cannot exist first; nothing executes until this is called.
func (s *Service) SetWorkflows(w Workflows) {
	s.wfMu.Lock()
	s.wf = w
	s.wfMu.Unlock()
}

func (s *Service) workflows() Workflows {
	s.wfMu.RLock()
	defer s.wfMu.RUnlock()
	return s.wf
}

func (s *Service) now() time.Time { return s.o.Clock.Now() }

func (s *Service) locale(ctx context.Context) Locale {
	if s.o.Locale != nil {
		return s.o.Locale(ctx)
	}
	return Locale{Lang: "en", Loc: time.UTC}
}

func (s *Service) expiry() time.Duration {
	if m := s.o.Cfg.Proactive.Approvals.ExpiryMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return DefaultExpiry
}

func (s *Service) killed() bool { return s.o.Killer != nil && s.o.Killer.Killed() }

func (s *Service) owner() (senderID string, ok bool) {
	route, ok := s.o.Cfg.Proactive.OwnerRoute()
	if !ok {
		return "", false
	}
	return route.SenderID, true
}

func (s *Service) emit(kind events.Kind, p Proposal, sev events.Severity) {
	if s.o.Events == nil {
		return
	}
	s.o.Events.PublishNonBlocking(events.Event{
		Kind: kind, Source: events.Source{Component: logComponent}, Severity: sev,
		Payload: map[string]any{"id": p.ID, "code": p.Code, "kind": string(p.Kind), "status": string(p.Status), "origin": string(p.Origin)},
	})
}

// ProposeRequest is a proposal to create. Origin, Trust, DeliverNow and the ids
// are facts about the turn, read from its context by the caller, never from a
// model argument.
type ProposeRequest struct {
	Order     OrderRequest
	Rationale string
	Origin    Origin
	// Trust is the minimum trust of the candidates behind a background turn, after
	// anything the turn read from outside has lowered it.
	Trust proactive.Trust
	// DeliverNow says attention decided to deliver the candidate's message now
	// (so quiet hours were respected upstream).
	DeliverNow                  bool
	CandidateID, InterventionID string
	// MandateID and Expiry are set by CODE only (never from a tool argument): the
	// dca.execute workflow asking about a run outside its mandate. OriginMandate
	// requires a MandateID; a positive Expiry replaces the default life of the card.
	MandateID string
	Expiry    time.Duration
}

// Propose creates a pending proposal and delivers its card to the owner.
//
// Gate A (who may propose) is decided here from the request's facts; gate B (the
// order gates, dry) is the backend's Prepare. Nothing is stored unless both pass.
func (s *Service) Propose(ctx context.Context, r ProposeRequest) (Proposal, error) {
	rationale, err := CleanRationale(r.Rationale)
	if err != nil {
		return Proposal{}, err
	}
	switch r.Origin {
	case OriginOwner:
	case OriginProactive:
		if r.Trust < minProactiveTrust {
			return Proposal{}, ErrUntrustedOrigin
		}
		if !r.DeliverNow || r.CandidateID == "" {
			return Proposal{}, ErrNotDeliverNow
		}
	case OriginMandate:
		// Built by code, from a mandate, for exactly the plan's own amount; only the
		// owner's approval places it, like any other order proposal.
		if r.MandateID == "" {
			return Proposal{}, fmt.Errorf("approval: origin %q needs a mandate", r.Origin)
		}
	default:
		return Proposal{}, fmt.Errorf("approval: unknown origin %q", r.Origin)
	}
	if _, ok := s.owner(); !ok {
		return Proposal{}, fmt.Errorf("%w: no owner is configured to approve it", ErrUnavailable)
	}

	ctx, cancel := context.WithTimeout(ctx, storeTimeout+30*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	class := classOf(KindOrder, r.Origin)
	if codes, err := s.st.pendingCodes(ctx, now, class); err != nil {
		return Proposal{}, err
	} else if max, _, _ := class.bounds(); len(codes) >= max {
		return Proposal{}, &PendingError{Codes: codes}
	}

	prep, err := s.o.Backend.Prepare(ctx, r.Order)
	if err != nil {
		return Proposal{}, err
	}
	if r.Origin == OriginProactive && !prep.Reducing {
		return Proposal{}, ErrNotRiskReducing
	}
	if s.killed() && !prep.Reducing {
		return Proposal{}, ErrKilled
	}
	params, err := canonicalOrder(prep.Params)
	if err != nil {
		return Proposal{}, err
	}

	loc := s.locale(ctx)
	p := Proposal{
		ID: proactive.NewID(), Nonce: newNonce(), Kind: KindOrder, Params: params, Hash: hashOf(KindOrder, params),
		Rationale: rationale, Origin: r.Origin, Mode: prep.Mode, Reducing: prep.Reducing,
		CandidateID: r.CandidateID, InterventionID: r.InterventionID,
		Lang: langOf(loc.Lang), Status: StatusPending, CreatedAt: now, ExpiresAt: now.Add(s.expiry()),
	}
	if r.Expiry > 0 {
		p.ExpiresAt = now.Add(r.Expiry)
	}
	var card Card
	for attempt := 0; ; attempt++ {
		p.Code = newCode()
		if card, err = RenderOrderCard(p, prep.Params, loc, ""); err != nil {
			return Proposal{}, err
		}
		p.Summary = card.Title
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
			return Proposal{}, ErrHourlyLimit
		}
		codes, _ := s.st.pendingCodes(ctx, now, class)
		return Proposal{}, &PendingError{Codes: codes}
	case err != nil:
		return Proposal{}, err
	}

	if err := s.deliverCard(ctx, &p, card); err != nil {
		return Proposal{}, err
	}
	s.emit(events.KindApprovalCreated, p, events.SeverityInfo)
	return p, nil
}

// deliverCard sends the card to the owner through notify.Deliver, directly: this
// is the second delivery path beside the attention outbox (see the package doc).
// A card that did not reach the owner is cancelled, so it does not hold a pending
// slot nobody can see.
func (s *Service) deliverCard(ctx context.Context, p *Proposal, card Card) error {
	session := card.SessionBody
	res, err := s.o.Notifier.Deliver(ctx, notify.Notification{
		Severity:       proactive.SeverityWarning,
		Title:          card.Title,
		Body:           card.Body,
		Actions:        card.Actions,
		DedupKey:       "approval:" + p.ID,
		InterventionID: p.InterventionID,
		SessionBody:    &session,
	})
	if err != nil || !res.Delivered {
		reason := res.Reason
		if err != nil {
			reason = err.Error()
		}
		_, _ = s.st.cancel(context.WithoutCancel(ctx), p.ID, "card not delivered", s.now())
		p.Status = StatusCancelled
		return fmt.Errorf("%w: the card could not be delivered to the owner (%s)", ErrUnavailable, reason)
	}
	p.CardChannel, p.CardChatID, p.CardMessageIDs = res.Route.Channel, res.Route.ChatID, res.PlatformMessageIDs
	if err := s.st.setCard(context.WithoutCancel(ctx), p.ID, p.CardChannel, p.CardChatID, p.CardMessageIDs); err != nil {
		logger.WarnCF(logComponent, "Could not record the card's message ids; the card will not be edited",
			map[string]any{"code": p.Code, "error": err.Error()})
	}
	return nil
}

// Get returns a proposal by id.
func (s *Service) Get(ctx context.Context, id string) (Proposal, error) { return s.st.get(ctx, id) }

// ByCode returns the pending proposal that holds code.
func (s *Service) ByCode(ctx context.Context, code string) (Proposal, error) {
	return s.st.pendingByCode(ctx, NormalizeCode(code))
}

// Decide records the owner's decision on the pending proposal that holds code.
//
// The sender must be the configured owner (the same comparison tools.checkOwnerTurn
// makes), the proposal pending and unexpired, and nonce, when given, the one stored
// (compared in constant time); button callbacks carry the nonce, the text command
// does not and relies on the owner check and the code's uniqueness. The transition
// is one conditional UPDATE, so a double tap, two channels, or a race with the
// expiry sweeper decides at most once; the loser is told the current status.
// Everything else the caller could probe answers ErrNotFound and nothing more.
//
// While the kill switch is engaged an approval of a proposal that does not only reduce
// risk is refused with ErrKilled and the proposal stays pending; a rejection, and
// the approval of an order that reduces risk, are not affected.
//
// Approving starts the approval.order workflow; the proposal is then executing.
func (s *Service) Decide(ctx context.Context, code, nonce string, approve bool, senderID string) (Proposal, error) {
	ownerID, ok := s.owner()
	canonical := identity.CanonicalSender(senderID, "")
	if !ok || !identity.SameSender(canonical, ownerID) {
		logger.WarnCF(logComponent, "SECURITY: approval decision from a sender who is not the owner", nil)
		return Proposal{}, ErrNotOwner
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	code = NormalizeCode(code)
	p, err := s.st.pendingByCode(ctx, code)
	if err != nil {
		return Proposal{}, err
	}
	now := s.now()
	if !now.Before(p.ExpiresAt) {
		s.expireOne(ctx, p)
		return Proposal{}, ErrNotFound
	}
	if nonce != "" && !nonceEqual(p.Nonce, nonce) {
		logger.WarnCF(logComponent, "SECURITY: approval decision with the wrong nonce", map[string]any{"code": code})
		return Proposal{}, ErrNotFound
	}
	if err := verifyProposal(p); err != nil {
		logger.ErrorCF(logComponent, "SECURITY: refusing to decide a proposal that does not match its hash", map[string]any{"code": code})
		_, _ = s.st.cancel(ctx, p.ID, "integrity check failed", now)
		return Proposal{}, ErrNotFound
	}

	// While the kill switch is engaged only an order that reduces risk may be
	// approved. The proposal is left pending (it expires on its own, and an owner
	// who resumes can still approve it); a rejection is never blocked.
	if approve && s.killed() && !p.Reducing && p.Kind != KindMemory { // a memory moves no money
		logger.InfoCF(logComponent, "Approval refused: the kill switch is engaged", map[string]any{"code": code})
		return Proposal{}, ErrKilled
	}

	if approve && (p.Kind == KindMandate && s.o.Mandates == nil || p.Kind == KindMemory && s.o.Memories == nil) {
		return Proposal{}, ErrUnavailable
	}
	to := StatusRejected
	if approve {
		to = StatusApproved
	}
	changed, err := s.st.decide(ctx, p.ID, to, canonical, now)
	if err != nil {
		return Proposal{}, err
	}
	if !changed {
		cur, gerr := s.st.get(ctx, p.ID)
		if gerr != nil {
			return Proposal{}, ErrNotFound
		}
		return Proposal{}, &StatusError{Code: cur.Code, Status: cur.Status}
	}
	p, err = s.st.get(ctx, p.ID)
	if err != nil {
		return Proposal{}, err
	}
	s.emit(events.KindApprovalDecided, p, events.SeverityInfo)
	s.editCard(ctx, p, OutcomeText(p.Lang, p.Status))
	if approve {
		if err := s.startExecution(ctx, p); err != nil {
			logger.ErrorCF(logComponent, "Approved, but the order workflow did not start; the sweeper will start it",
				map[string]any{"code": p.Code, "error": err.Error()})
		}
		if cur, gerr := s.st.get(ctx, p.ID); gerr == nil {
			p = cur
		}
	}
	return p, nil
}

// PendingByCard returns the pending, unexpired proposal whose card is the
// message messageID in chatID on channel: the answer to "does this reply point at a
// card that can still be decided?". A topic's composite chat id ("chat/thread")
// matches the chat the card was sent to. Anything else answers ErrNotFound.
func (s *Service) PendingByCard(ctx context.Context, channel, chatID, messageID string) (Proposal, error) {
	if messageID == "" || chatID == "" {
		return Proposal{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	ps, err := s.st.list(ctx, `status = 'pending' AND expires_at_ms > ?`, ms(s.now()))
	if err != nil {
		return Proposal{}, err
	}
	for _, p := range ps {
		if p.CardChatID == "" || !strings.EqualFold(p.CardChannel, channel) {
			continue
		}
		if chatID != p.CardChatID && !strings.HasPrefix(chatID, p.CardChatID+"/") {
			continue
		}
		for _, id := range p.CardMessageIDs {
			if id == messageID {
				return p, nil
			}
		}
	}
	return Proposal{}, ErrNotFound
}

// expireOne moves one pending proposal past its expiry to expired.
func (s *Service) expireOne(ctx context.Context, p Proposal) {
	changed, err := s.st.expire(ctx, p.ID, s.now())
	if err != nil || !changed {
		return
	}
	p.Status = StatusExpired
	s.emit(events.KindApprovalExpired, p, events.SeverityInfo)
	s.editCard(ctx, p, OutcomeText(p.Lang, StatusExpired))
}

// ExpireDue moves every pending proposal past ExpiresAt to expired, editing its
// card, and returns how many it moved. It fails closed: a proposal the sweeper
// has not reached yet still cannot be approved (Decide checks the time itself).
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	due, err := s.st.list(ctx, `status = 'pending' AND expires_at_ms <= ? LIMIT 50`, ms(s.now()))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range due {
		before := p.Status
		s.expireOne(ctx, p)
		if cur, err := s.st.get(ctx, p.ID); err == nil && cur.Status == StatusExpired && before == StatusPending {
			n++
		}
	}
	return n, nil
}

// editCard edits the delivered card to show outcome, when the channel can.
func (s *Service) editCard(ctx context.Context, p Proposal, outcome string) {
	if s.o.Editor == nil || len(p.CardMessageIDs) == 0 {
		return
	}
	loc := s.locale(ctx)
	loc.Lang = p.Lang
	var card Card
	if p.Kind == KindMemory {
		return // announced in a digest, which is one message of many: there is no card to edit
	}
	if p.Kind == KindMandate {
		mp, err := VerifiedMandate(p)
		if err != nil {
			return
		}
		card = RenderMandateCard(p, mp, loc, outcome)
	} else {
		op, err := decodeVerified(p)
		if err != nil {
			return
		}
		if card, err = RenderOrderCard(p, op, loc, outcome); err != nil {
			return
		}
	}
	// A card is edited in its first message only: a long card is one message, and
	// the editor drops the buttons by replacing the message's content.
	if err := s.o.Editor.EditCard(context.WithoutCancel(ctx), p.CardChannel, p.CardChatID, p.CardMessageIDs[0], card.Text()); err != nil {
		logger.DebugCF(logComponent, "Card not edited", map[string]any{"code": p.Code, "error": err.Error()})
	}
}

// WorkflowKind is the journal kind of an approved order's execution.
const WorkflowKind = "approval.order"

// WorkflowID derives the journal id from a proposal id, so starting the workflow
// of one proposal twice is a no-op.
func WorkflowID(proposalID string) string { return "ap_" + proposalID }

type workflowInput struct {
	ProposalID string `json:"proposal_id"`
}

// startExecution starts the approval.order workflow of an approved proposal and
// marks it executing. It is idempotent and is called from exactly two places:
// Decide (right after the owner approved) and Recover (for an approved proposal
// whose start was lost to a crash between the two). The workflow's first step
// re-reads the row and refuses anything that is not approved by the owner.
func (s *Service) startExecution(ctx context.Context, p Proposal) error {
	if p.Kind == KindMandate {
		return s.executeMandate(ctx, p) // creates the mandate row; no workflow, no order
	}
	if p.Kind == KindMemory {
		return s.executeMemory(ctx, p) // appends the fact; no workflow, no order
	}
	wf := s.workflows()
	if wf == nil {
		return errors.New("approval: no workflow engine attached")
	}
	id := WorkflowID(p.ID)
	if _, err := wf.StartWithID(ctx, WorkflowKind, id, workflowInput{ProposalID: p.ID}); err != nil {
		return err
	}
	_, err := s.st.markExecuting(ctx, p.ID, id)
	return err
}

// Recover heals what a crash can leave between the two components: every approved
// proposal without a workflow is started, and every executing one whose workflow
// has ended gets its final status. Safe to call at any time, from any number of
// goroutines; the gateway calls it at start and the sweeper on every pass.
func (s *Service) Recover(ctx context.Context) error {
	var errs []error
	approved, err := s.st.list(ctx, `status = 'approved' LIMIT 50`)
	if err != nil {
		return err
	}
	for _, p := range approved {
		if err := s.startExecution(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	wf := s.workflows()
	if wf == nil {
		return errors.Join(errs...)
	}
	defer s.renotify(ctx)
	executing, err := s.st.list(ctx, `status = 'executing' LIMIT 50`)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, p := range executing {
		if p.Kind == KindMandate {
			errs = append(errs, s.executeMandate(ctx, p))
			continue
		}
		if p.Kind == KindMemory {
			errs = append(errs, s.executeMemory(ctx, p))
			continue
		}
		w, err := wf.Get(ctx, p.WorkflowID)
		switch {
		case errors.Is(err, journal.ErrNotFound):
			errs = append(errs, s.startExecution(ctx, Proposal{ID: p.ID}))
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		case !w.Status.Terminal():
			continue
		}
		if w.Status == journal.StatusSucceeded {
			s.settleExecuted(ctx, p, "")
			continue
		}
		s.settleFailed(ctx, p, s.describeFailure(ctx, wf, p, w))
	}
	return errors.Join(errs...)
}

// Run runs the sweeper until ctx ends: it heals at start (a crash between an
// approval and its workflow is healed on the next start), then every SweepEvery
// expires proposals past their time, heals again and prunes. The goroutine is the
// caller's; it returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.o.Clock.After(SweepEvery):
		}
		s.sweep(ctx)
	}
}

func (s *Service) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, storeTimeout*2)
	defer cancel()
	if _, err := s.ExpireDue(sctx); err != nil {
		logger.WarnCF(logComponent, "Expiring proposals failed", map[string]any{"error": err.Error()})
	}
	if err := s.Recover(sctx); err != nil {
		logger.WarnCF(logComponent, "Healing approved orders failed", map[string]any{"error": err.Error()})
	}
	s.pruneMu.Lock()
	due := s.now().Sub(s.lastPrune) >= pruneEvery
	if due {
		s.lastPrune = s.now()
	}
	s.pruneMu.Unlock()
	if due {
		cutoff := s.now().Add(-RetentionDays * 24 * time.Hour)
		for i := 0; i < 10; i++ {
			n, err := s.st.prune(sctx, cutoff, pruneBatch)
			if err != nil || n < pruneBatch {
				break
			}
		}
	}
}

// Params returns the stored params of a proposal, verified against its hash.
func (s *Service) Params(p Proposal) (OrderParams, error) { return decodeVerified(p) }
