package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
)

// messageResponse is a successful Bot API reply carrying the given message ID.
func messageResponse(t *testing.T, id int) *ta.Response {
	t.Helper()
	b, err := json.Marshal(&telego.Message{MessageID: id})
	require.NoError(t, err)
	return &ta.Response{Ok: true, Result: b}
}

// sentBody decodes the JSON parameters of a recorded sendMessage call.
func sentBody(t *testing.T, call stubCall) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(call.Data.BodyRaw, &body))
	return body
}

var _ channels.IDReportingSender = (*TelegramChannel)(nil)

func TestTelegramSendWithIDs_CapturesMessageID(t *testing.T) {
	next := 4241
	caller := &stubCaller{
		callFn: func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			next++
			return messageResponse(t, next), nil
		},
	}
	ch := newTestChannel(t, caller)

	ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "Hello"})
	require.NoError(t, err)
	assert.Equal(t, []string{"4242"}, ids, "the Bot API message_id, as a decimal string")

	t.Run("send delegates to the same path", func(t *testing.T) {
		before := len(caller.calls)
		require.NoError(t, ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "Hello"}))
		assert.Len(t, caller.calls, before+1, "Send must make exactly one API call, like SendWithIDs")
	})

	t.Run("HTML expansion that forces a re-split reports every message in order", func(t *testing.T) {
		next = 100
		caller.calls = nil
		// 3000 '&' runes stay under the manager's limit but expand to
		// 15000 runes of "&amp;" in HTML, so the adapter must split.
		ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: "12345", Content: strings.Repeat("& ", 1500),
		})
		require.NoError(t, err)
		require.Greater(t, len(caller.calls), 1, "the fixture should have forced several API calls")
		require.Len(t, ids, len(caller.calls))
		for i, id := range ids {
			assert.Equal(t, 101+i, mustAtoi(t, id), "ids must be in send order")
		}
	})

	t.Run("the plain-text fallback reports the fallback message", func(t *testing.T) {
		caller.calls = nil
		calls := 0
		caller.callFn = func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("can't parse entities")
			}
			return messageResponse(t, 777), nil
		}
		ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "x"})
		require.NoError(t, err)
		assert.Equal(t, []string{"777"}, ids)
	})

	t.Run("a failed send reports no ID and keeps the retryable error class", func(t *testing.T) {
		caller.callFn = func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			return nil, errors.New("network down")
		}
		ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "x"})
		require.Error(t, err)
		assert.ErrorIs(t, err, channels.ErrTemporary)
		assert.Empty(t, ids)
	})

	t.Run("not running and empty content", func(t *testing.T) {
		stopped := newTestChannel(t, caller)
		stopped.SetRunning(false)
		_, err := stopped.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "1", Content: "x"})
		assert.ErrorIs(t, err, channels.ErrNotRunning)

		ids, err := ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "1", Content: ""})
		assert.NoError(t, err)
		assert.Empty(t, ids)
	})
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

func TestTelegramDisableNotification(t *testing.T) {
	caller := &stubCaller{
		callFn: func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			return messageResponse(t, 1), nil
		},
	}
	ch := newTestChannel(t, caller)

	require.NoError(t, ch.Send(context.Background(), bus.OutboundMessage{
		ChatID: "12345", Content: "digest", DisableNotification: true,
	}))
	require.Len(t, caller.calls, 1)
	assert.Equal(t, true, sentBody(t, caller.calls[0])["disable_notification"])

	caller.calls = nil
	require.NoError(t, ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: "reply"}))
	require.Len(t, caller.calls, 1)
	assert.NotContains(t, sentBody(t, caller.calls[0]), "disable_notification",
		"an ordinary message must keep notifying")

	t.Run("every send of a re-split message is silent", func(t *testing.T) {
		caller.calls = nil
		require.NoError(t, ch.Send(context.Background(), bus.OutboundMessage{
			ChatID: "12345", Content: strings.Repeat("& ", 1500), DisableNotification: true,
		}))
		require.Greater(t, len(caller.calls), 1)
		for i, call := range caller.calls {
			assert.Equal(t, true, sentBody(t, call)["disable_notification"], "call %d", i)
		}
	})

	t.Run("the plain-text fallback stays silent", func(t *testing.T) {
		caller.calls = nil
		n := 0
		caller.callFn = func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
			n++
			if n == 1 {
				return nil, errors.New("can't parse entities")
			}
			return messageResponse(t, 2), nil
		}
		require.NoError(t, ch.Send(context.Background(), bus.OutboundMessage{
			ChatID: "12345", Content: "x", DisableNotification: true,
		}))
		require.Len(t, caller.calls, 2)
		assert.Equal(t, true, sentBody(t, caller.calls[1])["disable_notification"])
	})
}
