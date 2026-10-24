package seahorse

import (
	"context"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// errNoConversation is returned to the model when a retrieval tool is called
// without a resolvable conversation. The tools fail closed on it: they never
// fall back to searching every conversation.
var errNoConversation = errors.New(
	"no conversation is associated with this call, so nothing was searched. " +
		"short_grep and short_expand only read the current conversation")

// callerConversationID resolves the conversation the calling turn belongs to
// from the session key the agent loop places in ctx. It returns
// errNoConversation when ctx carries no session key or the session has no
// stored conversation.
func (r *RetrievalEngine) callerConversationID(ctx context.Context) (int64, error) {
	sessionKey := tools.ToolSessionKey(ctx)
	if sessionKey == "" {
		return 0, errNoConversation
	}
	conv, err := r.store.GetConversationBySessionKey(ctx, sessionKey)
	if err != nil {
		return 0, err
	}
	if conv == nil || conv.ConversationID <= 0 {
		return 0, errNoConversation
	}
	return conv.ConversationID, nil
}
