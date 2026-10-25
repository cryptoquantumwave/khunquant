package attention

import (
	"strconv"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestOutbox_BufferIsCapped(t *testing.T) {
	e := newObEnv(t)
	for i := 0; i < maxBuffered+1; i++ {
		expect(t, e.submit(cand("k"+strconv.Itoa(i), notice)), proactive.DispNotify, ReasonTemplate)
	}
	// no Tick, no window elapsed: the full group went out when the next arrived.
	msgs := e.del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != maxBuffered {
		t.Fatalf("got %d messages, want one with %d decisions: %+v", len(msgs), maxBuffered, msgs)
	}
	e.ob.mu.Lock()
	held := len(e.ob.buf)
	e.ob.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d decisions still buffered, want 1", held)
	}
	// the straggler opened a window of its own and goes out with it.
	e.tickAt(e.clock.Now().Add(time.Minute))
	msgs = e.del.messages()
	if len(msgs) != 2 || len(msgs[1].Decisions) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
}
