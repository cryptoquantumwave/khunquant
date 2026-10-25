package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Kind says what a proposal proposes. Only orders exist in this change; the type
// is the spec's so that the table and the hash do not change when the others come.
type Kind string

// KindOrder is a spot or futures order.
const KindOrder Kind = "order"

// Status is where a proposal stands.
type Status string

const (
	StatusPending   Status = "pending"
	StatusApproved  Status = "approved"
	StatusRejected  Status = "rejected"
	StatusExpired   Status = "expired"
	StatusExecuting Status = "executing"
	StatusExecuted  Status = "executed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Finished reports whether no further transition happens from s.
func (s Status) Finished() bool {
	switch s {
	case StatusRejected, StatusExpired, StatusExecuted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Origin says whose turn created a proposal.
type Origin string

const (
	// OriginOwner: the owner's own root conversation turn.
	OriginOwner Origin = "owner"
	// OriginProactive: a candidate-driven proactive turn. Such a proposal may only
	// reduce risk.
	OriginProactive Origin = "proactive"
)

// Market values of OrderParams.Market.
const (
	MarketSpot    = "spot"
	MarketFutures = "futures"
)

// DefaultCollarPct is the default price collar: execution of a risk-increasing
// order is abandoned when the price has moved against it by more than this many
// percent since the reference price was taken. An order that only reduces risk is
// exempt: it is never abandoned for a price move.
const DefaultCollarPct = 1.0

// OrderParams is the canonical, stored description of an order: everything the
// card shows and everything the executor places comes from these fields, and the
// hash covers them. Amount is in base units for spot and in CONTRACTS for futures
// (computed by code from a notional; never taken from the model).
type OrderParams struct {
	Provider string   `json:"provider"`
	Account  string   `json:"account"`
	Market   string   `json:"market"`
	Symbol   string   `json:"symbol"`
	Side     string   `json:"side"` // buy | sell
	Type     string   `json:"type"` // market | limit
	Amount   float64  `json:"amount"`
	Price    *float64 `json:"price,omitempty"` // limit price
	// ReduceOnly is a futures order that can only shrink a position.
	ReduceOnly bool `json:"reduce_only"`
	// PositionSide and MarginMode are futures-only, read from the account by code.
	PositionSide string `json:"position_side,omitempty"`
	MarginMode   string `json:"margin_mode,omitempty"`
	// RefPrice is the price the estimate used; NotionalUSD the order's value in USD
	// at that price, both computed by code.
	RefPrice    float64 `json:"ref_price"`
	NotionalUSD float64 `json:"notional_usd"`
	// PriceCollarPct: execution is abandoned when the price moved against the order
	// by more than this vs RefPrice.
	PriceCollarPct float64 `json:"price_collar_pct"`
}

// Proposal is one row of the proposals table.
type Proposal struct {
	ID    string
	Code  string
	Nonce string
	Kind  Kind
	// Params is the canonical JSON of the kind's params; Hash is
	// sha256(canonical JSON of {kind, params}).
	Params json.RawMessage
	Hash   [32]byte
	// Summary is the card's headline, rendered by code from Params.
	Summary string
	// Rationale is the model's optional prose: shown under "Why", never parsed or executed.
	Rationale string
	Origin    Origin
	// Mode is how the order will be placed: "live", "paper" (simulated) or
	// "real_exit" (a real exit while paper mode is on).
	Mode string
	// Reducing: when proposed, the order could only shrink exposure. Such an order
	// is never abandoned for a price move (see ApprovalOrderBackend.Recheck), and
	// its card says so.
	Reducing                              bool
	CandidateID, IntentID, InterventionID string
	Lang                                  string
	Status                                Status
	CreatedAt, ExpiresAt                  time.Time
	DecidedAt                             *time.Time
	DecidedBy                             string
	WorkflowID                            string
	CardChannel, CardChatID               string
	CardMessageIDs                        []string
	Outcome                               string
	// Notified: the owner was told how the execution ended.
	Notified   bool
	FinishedAt *time.Time
}

// Modes of an order.
const (
	ModeLive     = "live"
	ModePaper    = "paper"
	ModeRealExit = "real_exit"
)

// OrderRequest is what the propose_order tool hands the service: the model's
// choices, not yet validated. Everything numeric that ends up stored is derived
// from this by the backend.
type OrderRequest struct {
	Provider, Account string
	Market            string
	Symbol            string
	Side, Type        string
	// Amount is in base units (spot). NotionalUSD sizes a futures order (contracts
	// are computed from it); it is ignored for spot.
	Amount      float64
	NotionalUSD float64
	// Contracts is an exact futures contract count for a REDUCE-ONLY order, set by
	// code only (the delta-neutral recovery closes exactly the quantity it assessed;
	// a notional would round it). propose_order never sets it, and the backend
	// refuses it on an order that can add risk.
	Contracts  float64
	Price      *float64
	ReduceOnly bool
	// CollarPct overrides DefaultCollarPct when positive.
	CollarPct float64
}

// Prepared is the backend's answer to a request that passed the dry gates.
type Prepared struct {
	Params OrderParams
	// Reducing: the order can only shrink exposure (decided by the shared
	// classifier, and for a spot sell only when it stays within what the account
	// holds). False whenever that cannot be established.
	Reducing bool
	Mode     string
}

// Rechecked is the backend's answer at execution time, after the gates ran again.
type Rechecked struct {
	Reducing    bool    `json:"reducing"`
	Mode        string  `json:"mode"`
	NotionalUSD float64 `json:"notional_usd"`
	LastPrice   float64 `json:"last_price"`
	// PnLBefore and PositionContracts are a futures exit's baseline, for the
	// daily-loss record after the fill.
	PnLBefore         *float64 `json:"pnl_before,omitempty"`
	PositionContracts float64  `json:"position_contracts,omitempty"`
}

// Placed is what the exchange (or the simulation) answered to the order.
type Placed struct {
	OrderID       string `json:"order_id"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Status        string `json:"status,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	// Fill is what the answer to the placement already said about the fill (a
	// simulated order is complete in its answer and cannot be fetched).
	Fill Fill `json:"fill"`
}

// Fill is what a fetch of the order showed. Known is false when it could not be fetched.
type Fill struct {
	Known    bool    `json:"known"`
	Status   string  `json:"status,omitempty"`
	Filled   float64 `json:"filled"`
	AvgPrice float64 `json:"avg_price,omitempty"`
	FeeCost  float64 `json:"fee_cost,omitempty"`
	FeeCcy   string  `json:"fee_ccy,omitempty"`
	Done     bool    `json:"done"` // closed, filled, canceled or expired: nothing more will change
}

// OrderBackend is everything exchange-shaped the service needs. It is implemented
// in pkg/tools, next to the order helpers it shares with create_order; the service
// never touches a provider itself, so every gate and every placement is in one
// place that the call-graph test can name.
type OrderBackend interface {
	// Prepare validates and prices a request and runs the DRY gates (permission,
	// risk, daily loss for a risk-increasing order, leverage, paper mode), without
	// touching the rate limiter. The error names the gate that failed.
	Prepare(ctx context.Context, req OrderRequest) (Prepared, error)
	// Recheck runs the same gates again on the stored params, fetches a fresh
	// ticker, refuses a risk-increasing order whose price moved against it past its
	// collar (an order that only reduces risk is never refused for a price move) and
	// re-values the order in USD.
	Recheck(ctx context.Context, p OrderParams) (Rechecked, error)
	// Throttle spends a rate-limiter token when the placement would reach an
	// exchange. ok=false means the limiter refused; retryAfter is how long until
	// it refills.
	Throttle(p OrderParams) (ok bool, retryAfter time.Duration)
	// PlaceApprovedOrder places the order through broker.PlaceOrderIdempotent. It is
	// the ONLY place in the program that does, and the service calls it only from
	// the place step of the approval.order workflow.
	PlaceApprovedOrder(ctx context.Context, p OrderParams, key string, attempt int, firstStartedAt time.Time, rc Rechecked) (Placed, error)
	// Fetch reads the order back once.
	Fetch(ctx context.Context, p OrderParams, orderID string) (Fill, error)
	// Record reports the placed order to the order feed and the daily-loss tracker.
	Record(ctx context.Context, p OrderParams, rc Rechecked, placed Placed, fill Fill)
}

// Notifier delivers a message to the owner. *notify.Service satisfies it.
type Notifier interface {
	Deliver(ctx context.Context, n notify.Notification) (notify.Result, error)
}

// CardEditor edits a delivered card. The gateway adapts the channel manager to it
// (channels that implement channels.MessageEditor); an editor that cannot edit
// returns an error and the card is left as it was.
type CardEditor interface {
	EditCard(ctx context.Context, channel, chatID, messageID, text string) error
}

// Workflows is the part of *journal.Engine the service uses.
type Workflows interface {
	StartWithID(ctx context.Context, kind, id string, input any) (created bool, err error)
	Get(ctx context.Context, id string) (journal.Workflow, error)
	Steps(ctx context.Context, id string) ([]journal.StepRecord, error)
}

// Killer reports the kill switch (control.State is the persisted one). Nil means never killed.
type Killer interface{ Killed() bool }

// Locale is how a card is worded and timed.
type Locale struct {
	Lang string // "en" (default) or "th"
	Loc  *time.Location
}

// Errors. The ones a Decide caller can see carry no detail on purpose.
var (
	// ErrNotFound is the one answer to a decision about a code that is unknown, not
	// pending, expired, or presented with the wrong nonce.
	ErrNotFound = errors.New("not found or no longer pending")
	// ErrNotOwner: the sender is not the configured owner (or none is configured).
	ErrNotOwner = errors.New("not authorized")
	// ErrUntrustedOrigin: the turn's trust is below exchange, or it read outside text.
	ErrUntrustedOrigin = errors.New("approval: the turn's inputs are not trusted enough to propose an order")
	// ErrNotDeliverNow: a background turn whose candidate attention had not decided to deliver now.
	ErrNotDeliverNow = errors.New("approval: no proposal from a candidate that was not decided for delivery now")
	// ErrNotRiskReducing: a background turn may only propose an order that reduces risk.
	ErrNotRiskReducing = errors.New("approval: a background turn may only propose an order that reduces risk")
	// ErrKilled: the kill switch is engaged; only an exit may be proposed.
	ErrKilled = errors.New("approval: trading is stopped (kill switch); only an order that reduces risk can be proposed")
	// ErrHourlyLimit: too many proposals created in the last hour.
	ErrHourlyLimit = errors.New("approval: too many proposals in the last hour")
	// ErrUnavailable: approvals cannot run (no owner route, delivery disabled, service not wired).
	ErrUnavailable = errors.New("approval: approvals are not available")
)

// PendingError is the refusal for a fourth pending proposal; it names the codes.
type PendingError struct{ Codes []string }

func (e *PendingError) Error() string {
	return fmt.Sprintf("approval: %d proposals are already waiting for a decision (%s); wait for them to be decided or to expire",
		len(e.Codes), strings.Join(e.Codes, ", "))
}

// StatusError tells the loser of a decision race what won.
type StatusError struct {
	Code   string
	Status Status
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("proposal %s is already %s", e.Code, e.Status)
}

// Bounds.
const (
	// MaxPending is how many proposals may wait for a decision at once.
	MaxPending = 3
	// MaxPerHour is how many may be created in a rolling hour.
	MaxPerHour = 10
	// MaxPendingMandate and MaxMandatePerHour are the same two bounds for mandate cards
	// and the questions of mandated runs, counted apart from orders so that they can
	// never keep an order (an exit above all) from being proposed.
	MaxPendingMandate = 8
	MaxMandatePerHour = 30
	// MaxRationaleRunes bounds the model's prose on a card.
	MaxRationaleRunes = 300
	// DefaultExpiry is the life of a proposal when the config sets none.
	DefaultExpiry = 10 * time.Minute
	// RetentionDays: finished rows older than this are pruned.
	RetentionDays = 180
)

// Reduced-risk trust floor for a proposal created by a background turn.
const minProactiveTrust = proactive.TrustExchange
