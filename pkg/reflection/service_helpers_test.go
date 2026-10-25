package reflection

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// fakeLog is the decision log: fixed decisions and mutes, whatever the window.
type fakeLog struct {
	mu        sync.Mutex
	decisions []attention.DayDecision
	mutes     []attention.MuteEvent
}

func (f *fakeLog) DecisionsBetween(_ context.Context, from, to time.Time, _ int) ([]attention.DayDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []attention.DayDecision
	for _, d := range f.decisions {
		if !d.At.Before(from) && d.At.Before(to) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeLog) MuteEventsBetween(_ context.Context, from, to time.Time) ([]attention.MuteEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []attention.MuteEvent
	for _, m := range f.mutes {
		if !m.Created.Before(from) && m.Created.Before(to) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeLog) add(d ...attention.DayDecision) {
	f.mu.Lock()
	f.decisions = append(f.decisions, d...)
	f.mu.Unlock()
}

type fakeSession struct {
	mu   sync.Mutex
	msgs []Message
	fp   string
}

func (f *fakeSession) Tail(context.Context, int) ([]Message, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.msgs...), f.fp, nil
}

func (f *fakeSession) say(text string) {
	f.mu.Lock()
	f.msgs = append(f.msgs, Message{Role: "user", Text: text})
	f.fp = fmt.Sprintf("%d:%s", len(f.msgs), text)
	f.mu.Unlock()
}

// fakeTurn plays the model: fn sees the input and answers.
type fakeTurn struct {
	mu     sync.Mutex
	inputs []string
	fn     func(input string) (TurnOutcome, error)
}

func (f *fakeTurn) RunReflectionTurn(_ context.Context, input string) (TurnOutcome, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, input)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return TurnOutcome{Called: true}, nil
	}
	return fn(input)
}

func (f *fakeTurn) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

// srig is a rig with the reflection service over fakes for the log, the session and the model.
type srig struct {
	*rig
	log     *fakeLog
	session *fakeSession
	turn    *fakeTurn
	svc     *Service
}

func newSRig(t *testing.T) *srig {
	t.Helper()
	r := &srig{rig: newRig(t), log: &fakeLog{}, session: &fakeSession{}, turn: &fakeTurn{}}
	svc, err := New(context.Background(), Options{
		DB: r.db, Turn: r.turn, Proposer: r.approval, AtLocal: "21:00", Clock: r.clock,
		Sources: Sources{Decisions: r.log, Specs: r.reg, Intents: r.intents, Profile: r.intents, Session: r.session},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

// dismissed adds n SET move alerts of the day at 'at'; delivered or dropped as muted.
func (r *srig) dismissed(prefix string, at time.Time, n int, delivered bool) []string {
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		disp := proactive.DispDrop
		if delivered {
			disp = proactive.DispNotify
		}
		r.log.add(attention.DayDecision{ID: id, At: at.Add(time.Duration(i) * time.Minute), TriggerID: "move_vs_vol:pf-move_vs_vol",
			DedupKey: "move_vs_vol:SET:tier1", Severity: proactive.SeverityNotice, Disposition: disp, Reason: "muted", Delivered: delivered})
		ids = append(ids, id)
	}
	return ids
}

// idsIn returns the decision ids of the input whose trigger starts with prefix.
func idsIn(input, prefix string) []string {
	var ids []string
	sc := bufio.NewScanner(strings.NewReader(input))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var row struct{ ID, Trigger string }
		if json.Unmarshal(sc.Bytes(), &row) == nil && row.ID != "" && strings.HasPrefix(row.Trigger, prefix) {
			ids = append(ids, row.ID)
		}
	}
	return ids
}

func answer(items ...proactive.ReflectionItem) func(string) (TurnOutcome, error) {
	return func(string) (TurnOutcome, error) { return TurnOutcome{Items: items, Called: true}, nil }
}
