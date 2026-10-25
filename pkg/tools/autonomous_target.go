package tools

import (
	"context"
	"fmt"
)

// autonomousTargetRefusal is the check a ClassNotify tool that can deliver to
// a chosen target makes behind the policy gate. The class ceiling lets every
// autonomous turn notify, so without it a prompt-injected heartbeat or cron
// turn (a news headline, a fetched page) could read balances and message them
// to any chat the bot can reach.
//
// In an autonomous turn the target must be the turn's own route, the place the
// heartbeat or job delivers to. channel and chatID are the values the tool is
// about to send to, after it has filled blanks from the context. A turn a
// person started is not affected.
func autonomousTargetRefusal(ctx context.Context, tool, channel, chatID string) *ToolResult {
	if !IsAutonomousOrigin(ToolOrigin(ctx)) {
		return nil
	}
	if channel == ToolChannel(ctx) && chatID == ToolChatID(ctx) {
		return nil
	}
	return policyRefusal(tool, fmt.Sprintf(
		"%s: an autonomous turn may only message its own chat; %s:%s is not that chat.",
		tool, channel, chatID))
}
