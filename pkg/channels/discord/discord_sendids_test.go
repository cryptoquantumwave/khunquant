package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

var _ channels.IDReportingSender = (*DiscordChannel)(nil)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type recordedRequest struct {
	method, path, body string
}

// newStubbedChannel returns a running channel whose REST client never touches
// the network: every request is answered by respond and recorded.
func newStubbedChannel(t *testing.T, respond func(n int) (int, string)) (*DiscordChannel, func() []recordedRequest) {
	t.Helper()
	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reqs []recordedRequest
	session.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedRequest{r.Method, r.URL.Path, string(body)})
		n := len(reqs)
		mu.Unlock()
		status, payload := respond(n)
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
			Request:    r,
		}, nil
	})}

	base := channels.NewBaseChannel("discord", config.DiscordConfig{}, nil, nil)
	base.SetRunning(true)
	return &DiscordChannel{BaseChannel: base, session: session}, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

func TestDiscordSendWithIDs_CapturesMessageID(t *testing.T) {
	ch, requests := newStubbedChannel(t, func(int) (int, string) {
		return http.StatusOK, `{"id":"1122334455","channel_id":"C1","content":"hi"}`
	})

	ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "1122334455" {
		t.Fatalf("ids = %v, want [1122334455]", ids)
	}
	reqs := requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || !strings.HasSuffix(reqs[0].path, "/channels/C1/messages") {
		t.Fatalf("unexpected request: %+v", reqs)
	}

	t.Run("a reply target is kept and its new message ID reported", func(t *testing.T) {
		ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "yo", ReplyToMessageID: "42"})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != "1122334455" {
			t.Fatalf("ids = %v", ids)
		}
		reqs := requests()
		if body := reqs[len(reqs)-1].body; !strings.Contains(body, `"message_id":"42"`) {
			t.Errorf("reply reference lost: %s", body)
		}
	})

	t.Run("Send delegates to the same path", func(t *testing.T) {
		before := len(requests())
		if err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"}); err != nil {
			t.Fatal(err)
		}
		if got := len(requests()); got != before+1 {
			t.Fatalf("Send made %d requests, want 1", got-before)
		}
	})

	t.Run("empty content and a stopped channel", func(t *testing.T) {
		before := len(requests())
		if ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1"}); err != nil || len(ids) != 0 {
			t.Fatalf("ids=%v err=%v", ids, err)
		}
		if len(requests()) != before {
			t.Fatal("empty content must not hit the API")
		}
		ch.SetRunning(false)
		if _, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "x"}); err != channels.ErrNotRunning {
			t.Fatalf("err = %v, want ErrNotRunning", err)
		}
	})
}

func TestDiscordSendWithIDs_FailureKeepsErrorClassAndReportsNoID(t *testing.T) {
	ch, _ := newStubbedChannel(t, func(int) (int, string) {
		return http.StatusBadRequest, `{"message":"Invalid Form Body","code":50035}`
	})
	ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"})
	if err == nil || !errors.Is(err, channels.ErrTemporary) {
		t.Fatalf("err = %v, want an ErrTemporary-class error (unchanged retry behaviour)", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}
