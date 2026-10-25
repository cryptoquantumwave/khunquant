package gateway

import (
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// With the digest switched off, live attention says once, at assembly, that held
// items are never sent. With it on, it says nothing.
func TestAttention_DigestOffIsSaidOnce(t *testing.T) {
	const phrase = "Daily digest is switched off"
	for _, digest := range []bool{false, true} {
		buf := &syncBuf{}
		f := newAssemblyFixtureNoBuild(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
			func(c *config.Config) { c.Proactive.Attention.Digest = digest }))
		logger.SetAdditionalWriter(buf)
		f.build(f.initial)
		got := strings.Count(buf.String(), phrase)
		if want := map[bool]int{false: 1, true: 0}[digest]; got != want {
			t.Fatalf("digest=%v: warning logged %d times, want %d", digest, got, want)
		}
	}
}
