//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/seahorse"
)

// The seahorse summarizer's call carries prompt_cache_key "seahorse" and no tools; the
// simulated model routes it to the seahorse caller by that key (see sim_signals_test.go).
func TestSeahorseCompleteCacheKey(t *testing.T) {
	rec := &seahorseRecordingProvider{}
	if _, err := providerToCompleteFn(rec, "m")(context.Background(), "p", seahorse.CompleteOptions{MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if rec.options[sim.OptionCacheKey] != sim.CacheKeySeahorse || len(rec.tools) != 0 {
		t.Errorf("seahorse call options %v, tools %v, want %s=%q and no tools",
			rec.options, rec.tools, sim.OptionCacheKey, sim.CacheKeySeahorse)
	}
}
