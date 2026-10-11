package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/routing"
)

// AppendAssistantMessage adds content to sessionKey's history as an assistant
// message and saves it. It is how a message the assistant sent on its own
// initiative (a proactive notification) becomes part of the conversation, so
// that a reply such as "yes, do it" has something to refer to.
//
// It takes the session's lock, waiting for a running turn to finish, so the
// message can never land between a tool call and its result; it honours ctx
// while waiting. It does no summarisation or compression of its own: the next
// turn on the session does that as usual.
//
// The session belongs to the agent named in the key ("agent:<id>:..."), which
// is the agent an inbound message for that key would be handled by; a key that
// names no registered agent falls back to the default agent. A session that
// does not exist yet is created, exactly as the first inbound message would.
func (al *AgentLoop) AppendAssistantMessage(ctx context.Context, sessionKey, content string) error {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return errors.New("append assistant message: empty session key")
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("append assistant message: empty content")
	}

	registry := al.GetRegistry()
	if registry == nil {
		return errors.New("append assistant message: no agent registry")
	}
	agent := registry.GetDefaultAgent()
	if parsed := routing.ParseAgentSessionKey(sessionKey); parsed != nil {
		if a, ok := registry.GetAgent(parsed.AgentID); ok {
			agent = a
		}
	}
	if agent == nil || agent.Sessions == nil {
		return errors.New("append assistant message: no session store")
	}

	return al.WithSessionLockContext(ctx, sessionKey, func(context.Context) error {
		agent.Sessions.AddMessage(sessionKey, "assistant", content)
		if err := agent.Sessions.Save(sessionKey); err != nil {
			return fmt.Errorf("append assistant message: save session: %w", err)
		}
		return nil
	})
}
