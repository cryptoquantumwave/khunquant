package control

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control/whoami"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Input, Callback and Reply are the channel layer's control types (see
// channels.ControlInput): they live there because pkg/channels cannot import this
// package.
type (
	// Input is one inbound message or button press offered to the control path.
	Input = channels.ControlInput
	// Callback is a button press.
	Callback = channels.ControlCallback
	// Reply is what the control path says back.
	Reply = channels.ControlReply
)

// Handler handles one recognised command. It runs after the rate limit and the
// owner check for its command, with the call's context carrying what it needs
// (see Dispatcher.Override). An error becomes the reply "could not be handled".
type Handler func(ctx context.Context, in Input) (Reply, error)

// Approver is the part of *approval.Service the control path uses.
type Approver interface {
	Decide(ctx context.Context, code, nonce string, approve bool, senderID string) (approval.Proposal, error)
	PendingByCard(ctx context.Context, channel, chatID, messageID string) (approval.Proposal, error)
}

// SentLookup maps the platform message ID a reply points at back to the
// intervention that sent it (*notify.Service).
type SentLookup interface {
	LookupByPlatformMessageID(ctx context.Context, channel, platformMsgID string) (interventionID string, ok bool, err error)
}

// Attention is the part of the attention outbox the control path uses: the mutes,
// and the decision log keyed by intervention ID (*attention.Outbox).
type Attention interface {
	Mute(ctx context.Context, keyPrefix string, until time.Time, reason string) error
	// MuteExact silences exactly one dedup key and nothing that merely starts with it.
	MuteExact(ctx context.Context, dedupKey string, until time.Time, reason string) error
	Unmute(ctx context.Context, keyPrefix string) error
	InterventionKeys(ctx context.Context, interventionID string) ([]string, error)
}

// KillSwitch is the kill flag (*State).
type KillSwitch interface {
	Killed() bool
	Engage(ctx context.Context, by, reason string) (changed bool, err error)
	Clear(ctx context.Context, by string) (changed bool, err error)
}

// Deps are what the handlers act on. Every field but Cfg may be nil; a handler
// whose dependency is missing says so instead of failing.
type Deps struct {
	Cfg    *config.Config
	Events *events.EventBus
	Kill   KillSwitch
	// Stop runs the whole of /stop (cancel turns, workflows, mandates, proposals and
	// open orders); nil means /stop only engages the kill flag.
	Stop      Stopper
	Approvals Approver
	Sent      SentLookup
	Attention Attention
}

// Dispatcher is the control path: it recognises the inputs that act on the system
// and handles them without a model.
//
// Threading: TryHandle is called from the receive goroutine of every channel, at
// the same time. The handlers run synchronously in it.
type Dispatcher struct {
	deps atomic.Pointer[Deps]

	mu       sync.RWMutex
	handlers map[string]*entry

	limiter *limiter
	secLog  *onceEvery
	now     func() time.Time
}

type entry struct {
	ownerOnly bool
	h         Handler
}

// Option customises New.
type Option func(*Dispatcher)

// WithClock sets the clock of the rate limits (tests).
func WithClock(now func() time.Time) Option { return func(d *Dispatcher) { d.now = now } }

// WithRateLimit sets the rate limit: max actions per window per sender.
func WithRateLimit(max int, window time.Duration) Option {
	return func(d *Dispatcher) { d.limiter = newLimiter(max, window) }
}

// New builds a Dispatcher with the built-in handlers.
func New(deps Deps, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		handlers: map[string]*entry{},
		limiter:  newLimiter(DefaultRateLimit, DefaultRateWindow),
		secLog:   newOnceEvery(time.Minute),
		now:      time.Now,
	}
	for _, o := range opts {
		o(d)
	}
	d.SetDeps(deps)
	d.handlers[cmdApprove] = &entry{ownerOnly: true, h: d.cmdApprove}
	d.handlers[cmdReject] = &entry{ownerOnly: true, h: d.cmdReject}
	d.handlers[cmdYes] = &entry{ownerOnly: true, h: d.cmdYes}
	d.handlers[cmdSnooze] = &entry{ownerOnly: true, h: d.cmdSnooze}
	d.handlers[cmdMute] = &entry{ownerOnly: true, h: d.cmdMute}
	d.handlers[cmdUnmute] = &entry{ownerOnly: true, h: d.cmdUnmute}
	d.handlers[cmdUseful] = &entry{ownerOnly: true, h: d.cmdUseful(true)}
	d.handlers[cmdNotUseful] = &entry{ownerOnly: true, h: d.cmdUseful(false)}
	d.handlers[cmdWhoami] = &entry{ownerOnly: false, h: d.cmdWhoami}
	d.handlers[cmdStop] = &entry{ownerOnly: true, h: d.cmdStop}
	d.handlers[cmdResume] = &entry{ownerOnly: true, h: d.cmdResume}
	return d
}

// SetDeps replaces the dependencies. The gateway installs the dispatcher before
// its channels start, with what exists by then, and attaches the rest as it is
// built.
func (d *Dispatcher) SetDeps(deps Deps) { d.deps.Store(&deps) }

// Deps returns a copy of the current dependencies.
func (d *Dispatcher) Deps() Deps { return *d.deps.Load() }

// Override replaces the handler of a built-in command, keeping its rate limit and
// owner check. It is how a later change adds to /stop. It returns false for a name
// that is not a command.
func (d *Dispatcher) Override(name string, h Handler) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.handlers[name]
	if !ok || h == nil {
		return false
	}
	e.h = h
	return true
}

func (d *Dispatcher) entryFor(name string) *entry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.handlers[name]
}

var _ channels.ControlRouter = (*Dispatcher)(nil)

// Recognises reports whether in is a control command, with no I/O.
func (d *Dispatcher) Recognises(in Input) bool {
	c, ok := classify(in)
	if !ok {
		return false
	}
	if c.name == cmdYes {
		return d.standing(d.deps.Load(), in) == standOwner
	}
	return true
}

// callState is what a handler is given through its context.
type callState struct {
	cmd     command
	deps    *Deps
	outcome string
	// owner is whether the sender is the configured owner, in any chat.
	owner bool
}

type callKey struct{}

func stateOf(ctx context.Context) *callState {
	st, _ := ctx.Value(callKey{}).(*callState)
	if st == nil {
		return &callState{deps: &Deps{}}
	}
	return st
}

// setOutcome records what became of the command, for the event.
func setOutcome(ctx context.Context, outcome string) { stateOf(ctx).outcome = outcome }

// Outcomes of a command, as the control.command event reports them.
const (
	outcomeOK          = "ok"
	outcomeDenied      = "denied"     // not the owner
	outcomeNoOwner     = "no_owner"   // no owner is configured
	outcomeWrongChat   = "wrong_chat" // the owner, but not in the owner chat
	outcomeOff         = "off"        // proactive is off or the service is not wired
	outcomeRateLimited = "rate_limited"
	outcomeUsage       = "usage"
	outcomeNotFound    = "not_found"
	outcomeRefused     = "refused"
	outcomeError       = "error"
)

// handleTimeout bounds one command.
const handleTimeout = 30 * time.Second

// TryHandle handles in when it is a control command and reports whether it did.
// For anything else it returns false straight away: no I/O, no lock, only the
// text is looked at. The one exception is a reply that says "yes" in the owner's
// own chat, which costs one indexed read to find out whether it points at a
// pending approval card.
func (d *Dispatcher) TryHandle(ctx context.Context, in Input) (Reply, bool) {
	c, ok := classify(in)
	if !ok {
		return Reply{}, false
	}
	deps := d.deps.Load()

	// An approval typed as a reply to a card. It is a command only when the owner
	// sends it in their own chat and it points at a pending card; "yes" to anything
	// else is ordinary chat and goes to the agent as it always did.
	var card *approval.Proposal
	if c.name == cmdYes {
		if d.standing(deps, in) != standOwner || deps.Approvals == nil {
			return Reply{}, false
		}
		p, err := deps.Approvals.PendingByCard(ctx, in.Channel, in.ChatID, in.ReplyToMessageID)
		switch {
		case err == nil:
			card = &p
		case isNotFound(err):
			return Reply{}, false
		default:
			logger.WarnCF("control", "Could not look up the card a reply points at", map[string]any{"error": err.Error()})
			d.emit(deps, in, c, true, outcomeError)
			return Reply{Text: failedText}, true
		}
		c.args = []string{card.Code}
	}

	st := &callState{cmd: c, deps: deps, outcome: outcomeOK}
	sender := canonicalSender(in)
	st.owner = ownerRoute(deps) != "" && identity.SameSender(sender, ownerRoute(deps))

	// 10 actions a minute per sender. /stop is exempt for the owner: it must always
	// work. Anybody else saying it is limited like any other command.
	exempt := c.name == cmdStop && d.standing(deps, in) == standOwner
	if !exempt && !d.limiter.allow(limiterKey(in, sender), d.now()) {
		d.emit(deps, in, c, st.owner, outcomeRateLimited)
		return Reply{Text: rateLimitedText}, true
	}

	e := d.entryFor(c.name)
	if e == nil {
		return Reply{}, false
	}
	if e.ownerOnly {
		if refusal, outcome := d.refuseNonOwner(deps, in, c, sender); refusal != "" {
			d.emit(deps, in, c, st.owner, outcome)
			return Reply{Text: refusal}, true
		}
	}

	hctx, cancel := context.WithTimeout(context.WithValue(ctx, callKey{}, st), handleTimeout)
	defer cancel()
	reply := d.run(hctx, e.h, in, st)
	if in.Callback != nil && reply.AnswerCallback == "" {
		reply.AnswerCallback = firstLine(reply.Text, 120)
	}
	d.emit(deps, in, c, st.owner, st.outcome)
	return reply, true
}

// run calls h, turning an error or a panic into the short "could not be handled".
func (d *Dispatcher) run(ctx context.Context, h Handler, in Input, st *callState) (reply Reply) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("control", "A control handler panicked", map[string]any{
				"command": st.cmd.name, "channel": in.Channel, "panic": fmt.Sprint(rec),
			})
			st.outcome = outcomeError
			reply = Reply{Text: failedText}
		}
	}()
	reply, err := h(ctx, in)
	if err != nil {
		logger.WarnCF("control", "A control command failed", map[string]any{
			"command": st.cmd.name, "channel": in.Channel, "error": err.Error(),
		})
		st.outcome = outcomeError
		return Reply{Text: failedText}
	}
	return reply
}

// Replies of the control path.
const (
	failedText        = "That command could not be handled. Try again."
	rateLimitedText   = "Too many commands; wait a minute."
	notAuthorizedText = "Not authorized."
	noOwnerText       = "No owner is configured, so this command is unavailable. Run `khunquant owner set` " +
		"(send /whoami here to see the IDs it needs)."
	wrongChatText = "Owner commands only work in your owner chat, the direct chat configured as proactive.owner. " +
		"Send /whoami there to check."
	proactiveOffText = "Proactive features are off, so there is nothing for this command to act on."
)

// standing is who the sender is in relation to the configured owner.
type standing int

const (
	// standNoOwner: no owner is configured.
	standNoOwner standing = iota
	// standOther: somebody who is not the owner.
	standOther
	// standElsewhere: the owner, writing from a group or a chat that is not the
	// owner chat.
	standElsewhere
	// standOwner: the owner, in the owner chat.
	standOwner
)

// ownerRoute returns the configured owner's canonical sender id, or "".
func ownerRoute(deps *Deps) string {
	if deps == nil || deps.Cfg == nil {
		return ""
	}
	route, ok := deps.Cfg.Proactive.OwnerRoute()
	if !ok {
		return ""
	}
	return route.SenderID
}

// standing applies the one definition of "the owner" (identity.SameSender, which
// approval.Service.Decide and tools.checkOwnerTurn use too) and then the owner
// route's definition of the owner's chat: the same channel and chat id, and not
// a group.
func (d *Dispatcher) standing(deps *Deps, in Input) standing {
	if deps == nil || deps.Cfg == nil {
		return standNoOwner
	}
	route, ok := deps.Cfg.Proactive.OwnerRoute()
	if !ok {
		return standNoOwner
	}
	if !identity.SameSender(canonicalSender(in), route.SenderID) {
		return standOther
	}
	if whoami.IsGroupChat(in.Channel, in.ChatID, in.PeerKind) ||
		!strings.EqualFold(in.Channel, route.Channel) || strings.TrimSpace(in.ChatID) != route.ChatID {
		return standElsewhere
	}
	return standOwner
}

// refuseNonOwner returns the reply for a sender who may not give an owner-only
// command, or "" when the command may go ahead.
func (d *Dispatcher) refuseNonOwner(deps *Deps, in Input, c command, sender string) (string, string) {
	switch d.standing(deps, in) {
	case standNoOwner:
		return noOwnerText, outcomeNoOwner
	case standOther:
		// One SECURITY line per sender per minute, naming the command and the
		// channel and never the owner.
		if d.secLog.ok(sender, d.now()) {
			logger.WarnCF("control", "SECURITY: control command from a sender who is not the owner",
				map[string]any{"command": c.name, "channel": in.Channel})
		}
		return notAuthorizedText, outcomeDenied
	case standElsewhere:
		return wrongChatText, outcomeWrongChat
	}
	if !deps.Cfg.Proactive.Enabled {
		return proactiveOffText, outcomeOff
	}
	return "", ""
}

// canonicalSender is the sender as "platform:id", the way tools.checkOwnerTurn
// makes it.
func canonicalSender(in Input) string {
	s := in.SenderID
	if s == "" {
		s = in.Sender.CanonicalID
	}
	return identity.CanonicalSender(s, in.Channel)
}

func limiterKey(in Input, canonical string) string {
	if canonical != "" {
		return canonical
	}
	return strings.ToLower(in.Channel) + ":" + in.SenderID
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

// emit publishes control.command: the command's name, the channel, whether the
// sender is the owner and the outcome. Never the code, the nonce, the text, or any
// id of the sender or the chat: on Telegram a direct chat's id is the user's own
// id, so the scope carries the channel name only.
func (d *Dispatcher) emit(deps *Deps, in Input, c command, owner bool, outcome string) {
	if deps == nil || deps.Events == nil {
		return
	}
	sev := events.SeverityInfo
	if outcome == outcomeDenied || outcome == outcomeError {
		sev = events.SeverityWarn
	}
	deps.Events.PublishNonBlocking(events.Event{
		Kind:     events.KindControlCommand,
		Source:   events.Source{Component: "control"},
		Severity: sev,
		Scope:    events.Scope{Channel: in.Channel},
		Payload: map[string]any{
			"command":  c.name,
			"channel":  in.Channel,
			"owner":    owner,
			"outcome":  outcome,
			"callback": c.viaCallback,
		},
	})
}
