package sources

import (
	"math"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
)

// SourceHealthPayload is the Payload of a perception.source.health event: a source
// says that it cannot see everything it is meant to. It carries names of accounts
// and a fixed reason, never a balance, an exchange message or a number from the
// exchange.
type SourceHealthPayload struct {
	Source         string   `json:"source"`
	State          string   `json:"state"` // "degraded"
	Reason         string   `json:"reason"`
	FailedAccounts []string `json:"failed_accounts,omitempty"` // "exchange/account", sorted
}

// finite reports whether v is neither NaN nor infinite.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// nonNegative reports whether v is a finite number that is not below zero: the
// test every price, quantity and value passes before it goes into an event.
func nonNegative(v float64) bool { return finite(v) && v >= 0 }

// healthTracker keeps what a source reports through Health. It is safe for
// concurrent use: Health is read from another goroutine.
type healthTracker struct {
	expected time.Duration

	mu          sync.Mutex
	lastErrAt   time.Time
	lastErr     string
	consecutive int
}

func (h *healthTracker) fail(at time.Time, msg string) {
	h.mu.Lock()
	h.lastErrAt, h.lastErr = at, msg
	h.consecutive++
	h.mu.Unlock()
}

func (h *healthTracker) ok() {
	h.mu.Lock()
	h.consecutive = 0
	h.mu.Unlock()
}

func (h *healthTracker) streak() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.consecutive
}

func (h *healthTracker) report() perception.SourceHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	return perception.SourceHealth{
		ExpectedEvery:     h.expected,
		LastErrorAt:       h.lastErrAt,
		LastError:         h.lastErr,
		ConsecutiveErrors: h.consecutive,
	}
}
