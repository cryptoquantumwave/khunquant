package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

var _ channels.IDReportingSender = (*SlackChannel)(nil)

// newStubbedSlack points the Slack client at a local httptest server that
// answers chat.postMessage with the given body and records the form values.
func newStubbedSlack(t *testing.T, reply string) (*SlackChannel, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)

	api := slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	base := channels.NewBaseChannel("slack", config.SlackConfig{}, nil, nil)
	base.SetRunning(true)
	return &SlackChannel{BaseChannel: base, api: api}, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), forms...)
	}
}

func TestSlackSendWithIDs_CapturesTimestamp(t *testing.T) {
	ch, forms := newStubbedSlack(t, `{"ok":true,"channel":"C1","ts":"1700000000.000200"}`)

	ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "1700000000.000200" {
		t.Fatalf("ids = %v, want the message ts", ids)
	}
	if got := forms()[0]; got.Get("channel") != "C1" || got.Get("text") != "hi" {
		t.Fatalf("unexpected request: %v", got)
	}

	t.Run("thread targets are kept", func(t *testing.T) {
		if _, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1/1699999999.000100", Content: "in thread"}); err != nil {
			t.Fatal(err)
		}
		all := forms()
		if got := all[len(all)-1].Get("thread_ts"); got != "1699999999.000100" {
			t.Fatalf("thread_ts = %q", got)
		}
	})

	t.Run("Send delegates to the same path", func(t *testing.T) {
		before := len(forms())
		if err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"}); err != nil {
			t.Fatal(err)
		}
		if got := len(forms()); got != before+1 {
			t.Fatalf("Send made %d requests, want 1", got-before)
		}
	})
}

func TestSlackSendWithIDs_FailureAndGuards(t *testing.T) {
	ch, _ := newStubbedSlack(t, `{"ok":false,"error":"channel_not_found"}`)
	ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"})
	if !errors.Is(err, channels.ErrTemporary) || len(ids) != 0 {
		t.Fatalf("ids=%v err=%v, want no IDs and an ErrTemporary-class error (unchanged retry behaviour)", ids, err)
	}

	if _, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "", Content: "hi"}); err == nil {
		t.Fatal("an empty chat ID must fail")
	}
	ch.SetRunning(false)
	if _, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "hi"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}
