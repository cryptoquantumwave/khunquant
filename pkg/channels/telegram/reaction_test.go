package telegram

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
)

// reactionRouter is a control router that also observes feedback, and remembers what it
// was offered.
type reactionRouter struct {
	mu        sync.Mutex
	reactions []reactionCall
}

type reactionCall struct {
	in    channels.ControlInput
	emoji string
}

func (r *reactionRouter) TryHandle(context.Context, channels.ControlInput) (channels.ControlReply, bool) {
	return channels.ControlReply{}, false
}
func (r *reactionRouter) Recognises(channels.ControlInput) bool { return false }
func (r *reactionRouter) ObserveReply(context.Context, channels.ControlInput) {
}

func (r *reactionRouter) ObserveReaction(_ context.Context, in channels.ControlInput, emoji string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = append(r.reactions, reactionCall{in, emoji})
}

func (r *reactionRouter) got() []reactionCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reactionCall(nil), r.reactions...)
}

// reaction is the message_reaction update of a user changing their reactions on a
// message. old and new are emoji lists.
func reaction(updateID int, from int64, chatID int64, msgID int, old, now []string) map[string]any {
	list := func(emojis []string) []map[string]any {
		out := []map[string]any{}
		for _, e := range emojis {
			out = append(out, map[string]any{"type": "emoji", "emoji": e})
		}
		return out
	}
	return map[string]any{"update_id": updateID, "message_reaction": map[string]any{
		"chat":         map[string]any{"id": chatID, "type": "private"},
		"message_id":   msgID,
		"user":         map[string]any{"id": from, "is_bot": false, "first_name": "U"},
		"date":         1700000000,
		"old_reaction": list(old),
		"new_reaction": list(now),
	}}
}

const up, down = "\U0001F44D", "\U0001F44E"

func TestReactionMapsToControlPath(t *testing.T) {
	router := &reactionRouter{}
	r := newRig(t, rigOpts{router: router, learning: true, allow: []string{ownerID, otherID}})

	r.api.updates <- reaction(1, 1001, 1001, 900, nil, []string{up})
	r.api.updates <- reaction(2, 1001, 1001, 901, []string{up}, []string{down}) // swapped: the thumbs down is new
	require.Eventually(t, func() bool { return len(router.got()) == 2 }, 5e9, 10e6)

	got := router.got()
	// The two may arrive in either order: they are handled on their own goroutines.
	byMsg := map[string]reactionCall{}
	for _, g := range got {
		byMsg[g.in.ReplyToMessageID] = g
	}
	assert.Equal(t, up, byMsg["900"].emoji)
	assert.Equal(t, down, byMsg["901"].emoji)
	in := byMsg["900"].in
	assert.Equal(t, "telegram", in.Channel)
	assert.Equal(t, "1001", in.ChatID)
	assert.Equal(t, "900", in.MessageID, "the message reacted to")
	assert.Equal(t, "telegram:1001", in.SenderID)
	assert.Equal(t, "telegram:1001", in.Sender.CanonicalID)
	assert.Equal(t, "direct", in.PeerKind)
	assert.Nil(t, in.Callback)
	assert.Empty(t, in.Text)
}

func TestReactionIgnored(t *testing.T) {
	router := &reactionRouter{}
	r := newRig(t, rigOpts{router: router, learning: true, allow: []string{ownerID}})

	r.api.updates <- reaction(1, 1001, 1001, 900, []string{up}, nil)                    // a thumbs up taken back
	r.api.updates <- reaction(2, 1001, 1001, 900, nil, []string{"❤"})                   // another emoji
	r.api.updates <- reaction(3, 1001, 1001, 900, nil, []string{up, down})              // both: ambiguous
	r.api.updates <- reaction(4, 1001, 1001, 900, []string{up}, []string{up, "x"})      // thumbs up was already there
	r.api.updates <- reaction(5, 2002, 2002, 900, nil, []string{up})                    // not on the allow-list
	r.api.updates <- map[string]any{"update_id": 6, "message_reaction": map[string]any{ // anonymous
		"chat": map[string]any{"id": 1001, "type": "private"}, "message_id": 900, "date": 1,
		"actor_chat":   map[string]any{"id": -5, "type": "channel"},
		"old_reaction": []any{}, "new_reaction": []map[string]any{{"type": "emoji", "emoji": up}},
	}}
	// A marker the channel does handle, to know the earlier updates were processed.
	r.api.updates <- reaction(7, 1001, 1001, 777, nil, []string{down})
	require.Eventually(t, func() bool { return len(router.got()) >= 1 }, 5e9, 10e6)
	r.api.settle()

	got := router.got()
	require.Len(t, got, 1, "only the marker is offered to the control path: %+v", got)
	assert.Equal(t, "777", got[0].in.ReplyToMessageID)
}

func TestReactionNotHandledWithLearningOff(t *testing.T) {
	router := &reactionRouter{}
	r := newRig(t, rigOpts{router: router, allow: []string{ownerID}})
	r.api.updates <- reaction(1, 1001, 1001, 900, nil, []string{up})
	r.api.settle()
	assert.Empty(t, router.got())
}
