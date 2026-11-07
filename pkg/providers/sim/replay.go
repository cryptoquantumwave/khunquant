package sim

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// ruleReplay is the "rule" the journal shows for an answer taken from the cassette.
const ruleReplay = "replay"

// LoadCassette replaces the replay set. Positions restart, so a loaded cassette answers
// from its first entry per key.
func (e *Engine) LoadCassette(entries []Entry, miss string) error {
	c, err := NewCassette(entries, miss)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.cass = c
	e.mu.Unlock()
	return nil
}

// CassetteStat says how many entries are loaded and what a miss does ("" and 0: none).
func (e *Engine) CassetteStat() (entries int, miss string) {
	e.mu.Lock()
	c := e.cass
	e.mu.Unlock()
	if c == nil {
		return 0, ""
	}
	return c.Len(), c.miss
}

// replay answers from the cassette. A request with no entry never gets an invented
// answer: it fails, naming the key, unless the cassette says miss: scripted, when the
// scripted rules answer. Either way the journal marks the call cassette_miss.
func (e *Engine) replay(ci CallInfo, msgs []protocoltypes.Message, key string) decision {
	e.mu.Lock()
	c := e.cass
	e.mu.Unlock()
	if c != nil {
		if ent, ok := c.next(key); ok {
			e.mu.Lock()
			defer e.mu.Unlock()
			return decision{rule: ruleReplay, resp: ent.response(func() string {
				e.callSeq++
				return callID(ruleReplay, e.callSeq)
			})}
		}
		if c.miss == MissScripted {
			d := e.answer(ci, msgs)
			d.miss = true
			return d
		}
	}
	why := "no cassette is loaded"
	if c != nil {
		why = "the cassette has no entry for this request"
	}
	return decision{rule: ruleReplay, miss: true, unmatched: true, err: fmt.Errorf("sim: cassette miss, key %s: %s", key, why)}
}
