package exchanges

import (
	"context"
	"errors"
	"time"
)

// Balance represents the balance of a single asset on an exchange.
type Balance struct {
	Asset  string
	Free   float64
	Locked float64
}

// WalletBalance extends Balance with wallet-type metadata and optional extra fields.
type WalletBalance struct {
	Balance
	WalletType string
	Extra      map[string]string // additional fields (e.g. "unrealized_pnl", "borrowed")
}

// Exchange is the interface that all exchange adapters must implement.
type Exchange interface {
	Name() string
	GetBalances(ctx context.Context) ([]Balance, error)
}

// WalletExchange is an optional extension for exchanges that support multiple wallet types.
type WalletExchange interface {
	Exchange
	SupportedWalletTypes() []string
	GetWalletBalances(ctx context.Context, walletType string) ([]WalletBalance, error)
}

// PricedExchange extends WalletExchange with asset price lookup.
// FetchPrice returns the last traded price of asset in terms of quote currency (e.g. "USDT").
// Returns (0, nil) if asset IS the quote currency or a recognized USD-equivalent stablecoin.
// Returns (0, error) if price cannot be determined.
type PricedExchange interface {
	WalletExchange
	FetchPrice(ctx context.Context, asset, quote string) (float64, error)
}

// QuoteLister is an optional extension for PricedExchange implementations
// that can enumerate which quote currencies they support.
// Used to produce actionable error messages when an unsupported quote is requested.
type QuoteLister interface {
	SupportedQuotes() []string
}

// ErrNeedsReauth signals that an exchange session requires interactive
// re-authentication (e.g. Webull's in-app 2FA approval step) before the
// requested call can succeed. It is a data/session-state condition, not a
// transient network failure — callers should not blind-retry on it; instead
// they should direct the user/LLM to the exchange's reconnect flow (e.g. the
// webull_reconnect tool) and wait for it to report success.
//
// Callers detect this with errors.Is(err, exchanges.ErrNeedsReauth); every
// layer that wraps the underlying error must use %w so the sentinel survives
// the wrap chain (client -> adapter -> exchange -> tool).
var ErrNeedsReauth = errors.New("exchange session needs re-authentication")

// ReauthExchange is implemented by exchanges that require interactive
// (in-app 2FA/approval) re-authentication rather than a simple credential
// refresh. Status strings are provider-neutral: "NORMAL" (ready to use),
// "PENDING" (awaiting in-app approval), "INVALID", or "EXPIRED".
type ReauthExchange interface {
	// Reconnect forces a fresh login/token request and returns the resulting
	// status. This is the fast, synchronous "start a login" step.
	Reconnect(ctx context.Context) (status string, err error)
	// CheckReauth polls once for whether a pending login has been approved,
	// returning the current status. Callers poll this on an interval.
	CheckReauth(ctx context.Context) (status string, err error)
}

// SessionInfoExchange is implemented by exchanges that can report their
// current re-auth session status and expiry without making a network call
// (e.g. by reading a cached/persisted token). Used by UIs that want to show
// live session state — a "connected, expires in N days" badge — safely on a
// polling interval, distinct from ReauthExchange.CheckReauth which always
// hits the exchange.
type SessionInfoExchange interface {
	// SessionInfo returns the last known status and expiry. expiresAt is
	// the zero time if unknown or if there has never been a session.
	SessionInfo() (status string, expiresAt time.Time)
}

// Re-auth session status values shared by ReauthExchange implementations
// (mirroring Webull's token states, kept provider-neutral).
const (
	ReauthStatusNormal  = "NORMAL"
	ReauthStatusPending = "PENDING"
	ReauthStatusInvalid = "INVALID"
	ReauthStatusExpired = "EXPIRED"
)

// PollReauth polls re.CheckReauth every interval until the status resolves
// to a terminal value — NORMAL (approved), INVALID, or EXPIRED — or ctx is
// done (timeout/cancel), in which case ctx.Err() is returned. A transient
// CheckReauth error does not abort the wait; only ctx ending does. This is
// the single shared wait-for-in-app-approval loop used by both the chat
// tool (pkg/tools/webull_reconnect.go) and the web launcher backend so
// their timing/terminal-status behavior cannot drift apart.
func PollReauth(ctx context.Context, re ReauthExchange, interval time.Duration) (string, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			status, err := re.CheckReauth(ctx)
			if err != nil {
				continue
			}
			switch status {
			case ReauthStatusNormal, ReauthStatusInvalid, ReauthStatusExpired:
				return status, nil
			default:
				// PENDING (or any transitional value): keep waiting.
			}
		}
	}
}
