package agent

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

func waitAgentIdle(t *testing.T, al *AgentLoop) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !al.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("agent loop did not go idle")
		}
		time.Sleep(time.Millisecond)
	}
}

// A turn is busy from the moment Run takes its message until the reply is published:
// parked inside the model it is registered, and afterwards it is gone. Asking changes
// nothing.
func TestAgentLoop_IdleCoversATurn(t *testing.T) {
	inLLM, release := newSignal(), newSignal()
	prov := newRoutedProvider().
		on("USER_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			inLLM.fire()
			select {
			case <-release.fired():
				return textResponse("done")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	al, msgBus := newTurnTestLoop(t, prov, false)
	ctx, stop := runLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, msgBus)
	if !al.Idle() {
		t.Fatal("a loop with no turn is idle")
	}

	publishInbound(t, msgBus, "chat-1", "USER_TURN hello")
	inLLM.wait(t, "the turn to reach the model")
	if al.Idle() || al.Idle() {
		t.Fatal("idle while a turn is inside the model call")
	}
	if n := len(al.activeTurnStates.byID); n != 1 {
		t.Fatalf("Idle changed the registry: %d turns", n)
	}

	release.fire()
	out.waitFor(t, "the reply", outboundTo("chat-1", "done"))
	waitAgentIdle(t, al)
}

// An async sub-turn that outlives its root keeps the loop busy until it returns.
func TestAgentLoop_IdleIncludesAsyncSubTurn(t *testing.T) {
	al := &AgentLoop{}
	root := &turnState{turnID: "root", sessionKey: "s"}
	al.activeTurnStates.register(root)
	if al.Idle() {
		t.Fatal("idle with a registered root turn")
	}
	child := &turnState{turnID: "child", parentTurnState: root}
	al.activeTurnStates.addSubTurn(child)
	al.activeTurnStates.unregister(root) // the root returned; the async child runs on
	if al.Idle() {
		t.Fatal("idle while an async sub-turn is running")
	}
	al.activeTurnStates.removeSubTurn(child)
	if !al.Idle() {
		t.Fatal("busy after the sub-turn returned")
	}
}

// Run counts a message it has taken off the bus until it has handed it on or answered.
func TestAgentLoop_IdleCountsHandledMessage(t *testing.T) {
	al := &AgentLoop{}
	al.handling.Add(1)
	if al.Idle() {
		t.Fatal("idle while Run holds a message")
	}
	al.handling.Add(-1)
	if !al.Idle() {
		t.Fatal("busy after Run released the message")
	}
}
