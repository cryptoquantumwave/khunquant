package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
)

// Request-level: a session that opens with a proactive message, sent through
// the real OpenAI-compatible provider, reaches the wire starting with a user
// turn after the system prompt.
func TestProactiveFirstSession_OpenAICompatRequestOpensWithUserTurn(t *testing.T) {
	var (
		mu   sync.Mutex
		body []byte
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer ts.Close()

	al, _ := ownerLoop(t, providers.NewHTTPProvider("k", ts.URL, ""))
	key := routing.NewRouteResolver(al.GetConfig()).ResolveRoute(routing.RouteInput{
		Channel: "telegram", Peer: &routing.RoutePeer{Kind: "direct", ID: "424242"},
	}).SessionKey
	if err := al.AppendAssistantMessage(context.Background(), key, "[proactive · info · X]\nhello"); err != nil {
		t.Fatal(err)
	}
	if _, err := al.processMessage(context.Background(), dmFrom("424242", "yes")); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v (%q)", err, body)
	}
	var got []string
	for _, m := range req.Messages {
		got = append(got, m.Role)
	}
	want := []string{"system", "user", "assistant", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire roles = %v, want %v", got, want)
	}
	if req.Messages[1].Content != providers.ConversationStartPlaceholder {
		t.Fatalf("opening user turn = %v", req.Messages[1].Content)
	}
}
