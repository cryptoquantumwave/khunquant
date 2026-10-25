package channels

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

type tapRouter struct {
	fn func(ControlInput) (ControlReply, bool)
}

func (r tapRouter) TryHandle(_ context.Context, in ControlInput) (ControlReply, bool) {
	return r.fn(in)
}
func (r tapRouter) Recognises(ControlInput) bool { return true }

func TestCallbackCodec_RoundTripsForEveryChannelLimit(t *testing.T) {
	// Telegram 64, Discord 100, LINE 300, Slack 2000.
	for _, limit := range []int{64, 100, 300, 2000} {
		for id, code := range CallbackCodes() {
			payload := strings.Repeat("a", bus.MaxActionPayloadBytes)
			if code == "apv" || code == "rej" {
				payload = "7F3K." + strings.Repeat("a", bus.MaxActionPayloadBytes-5)
			}
			data, err := EncodeCallbackData(bus.Action{ID: id, Payload: payload}, limit)
			require.NoError(t, err, "%s at %d", id, limit)
			gotID, gotPayload, err := ParseCallbackData(data, limit)
			require.NoError(t, err)
			assert.Equal(t, id, gotID)
			assert.Equal(t, payload, gotPayload)
		}
	}
	data, err := EncodeCallbackData(bus.Action{ID: "approve", Payload: "7F3K.abcd1234"}, 64)
	require.NoError(t, err)
	assert.Equal(t, "kq1|apv|7F3K.abcd1234", data, "the format Telegram has always sent")
}

func TestRouteTap(t *testing.T) {
	in := NewTapInput("x", "chat", "msg", "direct", bus.SenderInfo{CanonicalID: "x:1"}, ControlCallback{Action: "approve"})
	assert.Equal(t, "x:1", in.SenderID)
	assert.Equal(t, "msg", in.MessageID)
	assert.Equal(t, "msg", in.ReplyToMessageID)

	t.Run("no router", func(t *testing.T) {
		assert.Equal(t, TapOutcome{Toast: TapUnavailable}, RouteTap(context.Background(), nil, in))
	})
	t.Run("a router that does not know the action", func(t *testing.T) {
		out := RouteTap(context.Background(), tapRouter{func(ControlInput) (ControlReply, bool) { return ControlReply{}, false }}, in)
		assert.Equal(t, TapInvalidButton, out.Toast)
	})
	t.Run("a router that panics", func(t *testing.T) {
		out := RouteTap(context.Background(), tapRouter{func(ControlInput) (ControlReply, bool) { panic("boom") }}, in)
		assert.Equal(t, TapFailed, out.Toast)
	})
	t.Run("the toast falls back to the first line of the text", func(t *testing.T) {
		out := RouteTap(context.Background(), tapRouter{func(ControlInput) (ControlReply, bool) {
			return ControlReply{Text: "Refused: no.\nBecause.", EditOriginal: true}, true
		}}, in)
		assert.Equal(t, TapOutcome{Toast: "Refused: no.", Text: "Refused: no.\nBecause.", Edit: true}, out)
		assert.Equal(t, "Refused: no.", out.OutcomeLine())
		assert.Equal(t, "Refused: no.\nBecause.", out.Words())
	})
	t.Run("a toast alone is the words", func(t *testing.T) {
		out := TapOutcome{Toast: "Done"}
		assert.Equal(t, "Done", out.Words())
		assert.Equal(t, "Done", out.OutcomeLine())
	})
}
