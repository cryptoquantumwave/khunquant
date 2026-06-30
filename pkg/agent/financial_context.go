package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// FinancialContributor renders one section of the agent's runtime financial context.
// Implementations wrap a store (or any data source) and a per-feature limit.
// To add a new feature (e.g. grid-bot): implement this interface, open its store in
// buildFinancialContributors, and append it to the contributors list. No other changes needed.
type FinancialContributor interface {
	// Name returns a short identifier used for logging, e.g. "portfolio", "dca", "dn".
	Name() string
	// Section returns a markdown body string (no "## Header" — the collector adds one
	// overall header). Return "", nil to omit this contributor from the output.
	// Must be safe to call concurrently and must respect ctx cancellation/timeout.
	Section(ctx context.Context) (string, error)
	// Close releases the underlying store or resource.
	Close() error
}

// FinancialContextCollector assembles the "## Financial Context" block from a list of
// contributors. It knows nothing about portfolio/DCA/DN specifics — those live in
// financial_contributors.go. A TTL string cache avoids DB hammering on the hot path
// (buildDynamicContext is called on every BuildMessages invocation).
type FinancialContextCollector struct {
	contributors []FinancialContributor
	ttl          time.Duration // 0 = no cache, rebuild every call

	mu       sync.RWMutex
	cached   string
	cachedAt time.Time
}

// NewFinancialContextCollector creates a new collector. ttl=0 disables caching.
func NewFinancialContextCollector(contributors []FinancialContributor, ttl time.Duration) *FinancialContextCollector {
	return &FinancialContextCollector{
		contributors: contributors,
		ttl:          ttl,
	}
}

// GetSummary returns the "## Financial Context\n..." markdown section, or "" if there
// is no data or all contributors are empty/errored. NEVER panics or blocks the caller —
// all errors are logged at debug and silently dropped.
func (c *FinancialContextCollector) GetSummary() string {
	// Fast path: return cached value if still fresh.
	if c.ttl > 0 {
		c.mu.RLock()
		if !c.cachedAt.IsZero() && time.Since(c.cachedAt) < c.ttl {
			v := c.cached
			c.mu.RUnlock()
			return v
		}
		c.mu.RUnlock()
	}

	// Slow path: rebuild.
	// Self-managed timeout — buildDynamicContext has no ctx param.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var sections []string
	for _, contrib := range c.contributors {
		section, err := contrib.Section(ctx)
		if err != nil {
			logger.DebugCF("agent", "financial context contributor error",
				map[string]any{"contributor": contrib.Name(), "error": err.Error()})
			continue
		}
		if section != "" {
			sections = append(sections, section)
		}
	}

	var result string
	if len(sections) > 0 {
		result = "## Financial Context\n" + strings.Join(sections, "\n")
	}

	if c.ttl > 0 {
		c.mu.Lock()
		c.cached = result
		c.cachedAt = time.Now()
		c.mu.Unlock()
	}

	return result
}

// Close closes all contributors (and therefore their underlying stores).
func (c *FinancialContextCollector) Close() error {
	for _, contrib := range c.contributors {
		if err := contrib.Close(); err != nil {
			logger.DebugCF("agent", "financial contributor close error",
				map[string]any{"contributor": contrib.Name(), "error": err.Error()})
		}
	}
	return nil
}
