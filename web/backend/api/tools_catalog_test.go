package api

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// catalogConfigKeyAliases are the rows whose ConfigKey deliberately differs from
// their tool name, because their enablement does not live in a per-tool
// ToolConfig field.
//
//   - web_search is toggled by the whole web tool group (Web.Enabled).
//   - the two tool-search rows carry a nested config path, not a switch key:
//     buildToolSupport resolves them from cfg.Tools.MCP.Discovery.UseRegex /
//     UseBM25 directly (tools.go:648) and writes them back the same way, so
//     IsToolEnabled is never consulted for them.
//
// Anything not listed here is expected to be a plain per-tool toggle.
var catalogConfigKeyAliases = map[string]string{
	tools.NameToolSearchRegex: "mcp.discovery.use_regex",
	tools.NameToolSearchBM25:  "mcp.discovery.use_bm25",
	tools.NameWebSearch:       "web",
}

// TestCatalogToolsHaveEnableArms reconciles the tool catalog against
// ToolsConfig.IsToolEnabled.
//
// IsToolEnabled is a hand-maintained switch and is default-allow: an unknown
// name returns true. A tool wired into registration without a config field and
// a matching switch arm therefore runs unconditionally, and no setting an
// operator can write will turn it off. That is exactly how all five option
// tools shipped permanently enabled - including option_create_order, which
// places real options orders, while its spot and futures equivalents are
// disabled by default under a stated "opt-in for live trading" policy.
//
// The discriminator is a zero-value ToolsConfig: a key backed by a real arm
// reads a field that is false, so anything returning true has no arm at all.
// (pkg/config's own tests use the same zero-value idiom.)
//
// Rows in catalogConfigKeyAliases are skipped because their enablement is not
// resolved through this switch at all - checking them would report a gap that
// does not exist, and a security guard that cries wolf gets silenced.
func TestCatalogToolsHaveEnableArms(t *testing.T) {
	zero := &config.ToolsConfig{}

	for _, entry := range toolCatalog {
		if entry.ConfigKey == "" {
			continue // deliberately not operator-configurable
		}
		if _, aliased := catalogConfigKeyAliases[entry.Name]; aliased {
			continue // enablement lives outside IsToolEnabled
		}
		if zero.IsToolEnabled(entry.ConfigKey) {
			t.Errorf(
				"%q: IsToolEnabled returns true on a zero-value config, so it has no switch arm "+
					"in ToolsConfig.IsToolEnabled and cannot be disabled by any setting",
				entry.ConfigKey,
			)
		}
	}
}

// TestCatalogEntriesAreWellFormed guards presentation and wiring: an entry with
// an empty Name or Category renders as a blank dashboard row, and a ConfigKey
// that differs from its Name toggles a different tool than the row claims.
//
// The three legitimate aliases are allowlisted by name above, so this still
// fails on a new, unexplained mismatch rather than accepting any divergence.
func TestCatalogEntriesAreWellFormed(t *testing.T) {
	seen := make(map[string]bool, len(toolCatalog))

	for i, entry := range toolCatalog {
		if entry.Name == "" {
			t.Errorf("catalog entry %d has an empty Name", i)
			continue
		}
		if entry.Category == "" {
			t.Errorf("%q has an empty Category", entry.Name)
		}
		if entry.Description == "" {
			t.Errorf("%q has an empty Description", entry.Name)
		}
		if seen[entry.Name] {
			t.Errorf("%q appears in the catalog more than once", entry.Name)
		}
		seen[entry.Name] = true

		if entry.ConfigKey == "" || entry.ConfigKey == entry.Name {
			continue
		}
		want, aliased := catalogConfigKeyAliases[entry.Name]
		if !aliased {
			t.Errorf("%q has ConfigKey %q: a mismatch toggles a different tool than the row claims. "+
				"If this is deliberate, add it to catalogConfigKeyAliases with the reason",
				entry.Name, entry.ConfigKey)
			continue
		}
		if entry.ConfigKey != want {
			t.Errorf("%q has ConfigKey %q, but the alias table says %q", entry.Name, entry.ConfigKey, want)
		}
	}
}

// TestOptionToolsAreDisableable is the regression test for the gap this work
// closed: every option tool must be backed by a real switch arm, and the
// order-placing one must be off by default like create_order and
// futures_open_position.
func TestOptionToolsAreDisableable(t *testing.T) {
	zero := &config.ToolsConfig{}
	for _, name := range []string{
		tools.NameOptionQuote,
		tools.NameOptionCreateOrder,
		tools.NameOptionCancelOrder,
		tools.NameOptionGetOrder,
		tools.NameOptionOpenOrders,
	} {
		if zero.IsToolEnabled(name) {
			t.Errorf("%q has no IsToolEnabled arm: it cannot be disabled", name)
		}
	}

	def := config.DefaultConfig()
	if def.Tools.IsToolEnabled(tools.NameOptionCreateOrder) {
		t.Error("option_create_order is enabled by default; it places real orders and must be opt-in")
	}
	for _, name := range []string{
		tools.NameOptionQuote,
		tools.NameOptionCancelOrder,
		tools.NameOptionGetOrder,
		tools.NameOptionOpenOrders,
	} {
		if !def.Tools.IsToolEnabled(name) {
			t.Errorf("%q is disabled by default; only order placement should be opt-in", name)
		}
	}
}
