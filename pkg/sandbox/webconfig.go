package sandbox

import "github.com/cryptoquantumwave/khunquant/pkg/config"

// PlaceholderWebKey is the Brave key of a locked instance. Like the exchange placeholders it
// is a constant that cannot be a real credential and exists in memory only: the locked
// gateway sets it on every load and reload (ApplyWebConfig) and the lock refuses every
// config write, so it never reaches disk. It sits under tools.web.brave.api_keys, a name in
// config.CredentialFieldNames, so it is masked in API responses, read_config and logs like
// any key.
const PlaceholderWebKey = "sbx-placeholder-NOT-A-REAL-BRAVE-KEY"

// ApplyWebConfig makes the web tools of a locked instance the ones SimWeb serves: web_search
// and web_fetch on, Brave the only search provider (the one SimWeb renders), with the
// placeholder key. Every other provider is replaced by its zero value, so a key or base URL
// the file held is neither used nor kept in memory; the lock would send their requests to
// SimWeb, which answers them 404, but they would still win the provider priority over Brave.
func ApplyWebConfig(cfg *config.Config) {
	w := &cfg.Tools.Web
	w.Enabled = true
	cfg.Tools.WebFetch.Enabled = true
	w.Brave = config.BraveConfig{Enabled: true, APIKeys: []string{PlaceholderWebKey}, MaxResults: w.Brave.MaxResults}
	w.Perplexity = config.PerplexityConfig{}
	w.Tavily = config.TavilyConfig{}
	w.DuckDuckGo = config.DuckDuckGoConfig{}
	w.SearXNG = config.SearXNGConfig{}
	w.GLMSearch = config.GLMSearchConfig{}
}
