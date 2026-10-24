package seahorse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// scopeFixture holds two conversations, each with one distinctive message and
// summary, so cross-conversation leaks are detectable by keyword.
type scopeFixture struct {
	re         *RetrievalEngine
	msgA, msgB *Message
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()

	convA, err := s.GetOrCreateConversation(ctx, "test:scope-A")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	convB, err := s.GetOrCreateConversation(ctx, "test:scope-B")
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	msgA, err := s.AddMessage(ctx, convA.ConversationID, "user", "alphaword shared pineapple note", 5)
	if err != nil {
		t.Fatalf("add A: %v", err)
	}
	msgB, err := s.AddMessage(ctx, convB.ConversationID, "user", "betaword secret pineapple note", 5)
	if err != nil {
		t.Fatalf("add B: %v", err)
	}
	for _, in := range []CreateSummaryInput{
		{ConversationID: convA.ConversationID, Kind: SummaryKindLeaf, Content: "alphaword summary pineapple", TokenCount: 5},
		{ConversationID: convB.ConversationID, Kind: SummaryKindLeaf, Content: "betaword summary pineapple", TokenCount: 5},
	} {
		if _, err := s.CreateSummary(ctx, in); err != nil {
			t.Fatalf("create summary: %v", err)
		}
	}
	return scopeFixture{re: &RetrievalEngine{store: s}, msgA: msgA, msgB: msgB}
}

type grepOutput struct {
	Summaries []struct {
		Content string `json:"content"`
	} `json:"summaries"`
	Messages []struct {
		Snippet string `json:"snippet"`
	} `json:"messages"`
}

func runGrep(t *testing.T, tool *GrepTool, ctx context.Context, args map[string]any) grepOutput {
	t.Helper()
	res := tool.Execute(ctx, args)
	if res.IsError {
		t.Fatalf("grep error: %s", res.ForLLM)
	}
	var out grepOutput
	if err := json.Unmarshal([]byte(res.ForLLM), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return out
}

func TestGrepToolScopedToCallerConversation(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewGrepTool(f.re)
	ctxA := tools.WithToolSessionKey(context.Background(), "test:scope-A")

	// Shared keyword: only A's own rows may come back, via FTS and via LIKE.
	for _, pattern := range []string{"pineapple", "%pineapple%"} {
		out := runGrep(t, tool, ctxA, map[string]any{"pattern": pattern})
		if len(out.Messages) != 1 || len(out.Summaries) != 1 {
			t.Fatalf("pattern %q: got %d messages / %d summaries, want 1 / 1",
				pattern, len(out.Messages), len(out.Summaries))
		}
		if strings.Contains(out.Messages[0].Snippet, "betaword") ||
			strings.Contains(out.Summaries[0].Content, "betaword") {
			t.Errorf("pattern %q leaked conversation B content: %+v", pattern, out)
		}
	}

	// A keyword that exists only in B finds nothing from A.
	for _, pattern := range []string{"betaword", "%betaword%"} {
		out := runGrep(t, tool, ctxA, map[string]any{"pattern": pattern})
		if len(out.Messages) != 0 || len(out.Summaries) != 0 {
			t.Errorf("pattern %q from A returned B content: %+v", pattern, out)
		}
	}
}

func TestGrepToolSameConversationStillWorks(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewGrepTool(f.re)
	ctxB := tools.WithToolSessionKey(context.Background(), "test:scope-B")

	out := runGrep(t, tool, ctxB, map[string]any{"pattern": "betaword"})
	if len(out.Messages) != 1 || len(out.Summaries) != 1 {
		t.Fatalf("got %d messages / %d summaries, want 1 / 1", len(out.Messages), len(out.Summaries))
	}
}

func TestGrepToolRefusesAllConversationsFlag(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewGrepTool(f.re)
	ctxA := tools.WithToolSessionKey(context.Background(), "test:scope-A")

	res := tool.Execute(ctxA, map[string]any{"pattern": "betaword", "all_conversations": true})
	if !res.IsError {
		t.Fatalf("expected refusal, got: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "secret") {
		t.Errorf("refusal leaked content: %s", res.ForLLM)
	}
}

func TestGrepToolFailsClosedWithoutConversation(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewGrepTool(f.re)

	cases := map[string]context.Context{
		"no session key":      context.Background(),
		"empty session key":   tools.WithToolSessionKey(context.Background(), ""),
		"unknown session key": tools.WithToolSessionKey(context.Background(), "test:no-such-session"),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			res := tool.Execute(ctx, map[string]any{"pattern": "pineapple"})
			if !res.IsError {
				t.Fatalf("expected refusal, got: %s", res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, "no conversation") {
				t.Errorf("message should explain missing conversation: %s", res.ForLLM)
			}
			if strings.Contains(res.ForLLM, "alphaword") || strings.Contains(res.ForLLM, "betaword") {
				t.Errorf("result leaked content: %s", res.ForLLM)
			}
		})
	}
}

func TestExpandToolRefusesOtherConversationMessage(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewExpandTool(f.re)
	ctxA := tools.WithToolSessionKey(context.Background(), "test:scope-A")

	// B's id alone, and B's id mixed with A's own id: both refused, no content.
	for _, ids := range [][]any{
		{fmt.Sprintf("%d", f.msgB.ID)},
		{fmt.Sprintf("%d", f.msgA.ID), fmt.Sprintf("%d", f.msgB.ID)},
	} {
		res := tool.Execute(ctxA, map[string]any{"message_ids": ids})
		if !res.IsError {
			t.Fatalf("ids %v: expected refusal, got: %s", ids, res.ForLLM)
		}
		if !strings.Contains(res.ForLLM, "does not belong to this conversation") {
			t.Errorf("ids %v: refusal should say so: %s", ids, res.ForLLM)
		}
		if strings.Contains(res.ForLLM, "secret") || strings.Contains(res.ForLLM, "alphaword") {
			t.Errorf("ids %v: refusal leaked content: %s", ids, res.ForLLM)
		}
	}
}

func TestExpandToolSameConversationWorks(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewExpandTool(f.re)
	ctxA := tools.WithToolSessionKey(context.Background(), "test:scope-A")

	res := tool.Execute(ctxA, map[string]any{"message_ids": []any{fmt.Sprintf("%d", f.msgA.ID)}})
	if res.IsError {
		t.Fatalf("expand own message failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "alphaword") {
		t.Errorf("expected own content, got: %s", res.ForLLM)
	}
}

func TestExpandToolFailsClosedWithoutConversation(t *testing.T) {
	f := newScopeFixture(t)
	tool := NewExpandTool(f.re)

	res := tool.Execute(context.Background(), map[string]any{"message_ids": []any{fmt.Sprintf("%d", f.msgA.ID)}})
	if !res.IsError {
		t.Fatalf("expected refusal, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "no conversation") {
		t.Errorf("message should explain missing conversation: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "alphaword") {
		t.Errorf("result leaked content: %s", res.ForLLM)
	}
}
