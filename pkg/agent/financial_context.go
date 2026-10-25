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

// liveContributor is optionally implemented by a contributor whose section must
// not come from the collector's cache: it is rebuilt on every call, and it is
// told whether the prompt is a compact (background) one. The intents digest is
// one: an intent the owner has just set must be in the next prompt, not in one
// built half an hour from now, and a lapsed one must be gone from it.
type liveContributor interface {
	FinancialContributor
	// LiveSection renders the section for a full prompt (compact false) or for a
	// light or proactive one (compact true).
	LiveSection(ctx context.Context, compact bool) (string, error)
}

// FinancialContextCollector assembles the "## Financial Context" block from a list of
// contributors. It knows nothing about portfolio/DCA/DN specifics — those live in
// financial_contributors.go. A TTL string cache avoids DB hammering on the hot path
// (buildDynamicContext is called on every BuildMessages invocation); a liveContributor
// is the exception and is rendered on every call.
type FinancialContextCollector struct {
	contributors []FinancialContributor
	ttl          time.Duration // 0 = no cache, rebuild every call

	mu       sync.RWMutex
	cached   []string // one entry per contributor; a live contributor's stays ""
	cachedAt time.Time
}

// NewFinancialContextCollector creates a new collector. ttl=0 disables caching.
func NewFinancialContextCollector(contributors []FinancialContributor, ttl time.Duration) *FinancialContextCollector {
	return &FinancialContextCollector{
		contributors: contributors,
		ttl:          ttl,
	}
}

// GetSummary returns the "## Financial Context\n..." markdown section of a full
// prompt, or "" if there is no data or all contributors are empty/errored. NEVER
// panics or blocks the caller — all errors are logged at debug and silently
// dropped.
func (c *FinancialContextCollector) GetSummary() string { return c.GetSummaryFor(false) }

// GetSummaryFor is GetSummary for a full prompt (compact false) or for a light or
// proactive one (compact true). Only a liveContributor looks at the difference;
// with none attached the two are the same string, from the same cache.
func (c *FinancialContextCollector) GetSummaryFor(compact bool) string {
	// Self-managed timeout — buildDynamicContext has no ctx param.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cached := c.cachedSections(ctx)
	var sections []string
	for i, contrib := range c.contributors {
		section := cached[i]
		if lc, ok := contrib.(liveContributor); ok {
			var err error
			if section, err = lc.LiveSection(ctx, compact); err != nil {
				logger.DebugCF("agent", "financial context contributor error",
					map[string]any{"contributor": contrib.Name(), "error": err.Error()})
				continue
			}
		}
		if section != "" {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		return ""
	}
	return "## Financial Context\n" + strings.Join(sections, "\n")
}

// cachedSections returns each non-live contributor's section, from the cache
// while it is fresh and rebuilt otherwise. An error is logged and leaves "".
func (c *FinancialContextCollector) cachedSections(ctx context.Context) []string {
	if c.ttl > 0 {
		c.mu.RLock()
		if c.cached != nil && !c.cachedAt.IsZero() && time.Since(c.cachedAt) < c.ttl {
			v := c.cached
			c.mu.RUnlock()
			return v
		}
		c.mu.RUnlock()
	}

	secs := make([]string, len(c.contributors))
	for i, contrib := range c.contributors {
		if _, live := contrib.(liveContributor); live {
			continue
		}
		section, err := contrib.Section(ctx)
		if err != nil {
			logger.DebugCF("agent", "financial context contributor error",
				map[string]any{"contributor": contrib.Name(), "error": err.Error()})
			continue
		}
		secs[i] = section
	}

	if c.ttl > 0 {
		c.mu.Lock()
		c.cached = secs
		c.cachedAt = time.Now()
		c.mu.Unlock()
	}
	return secs
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
