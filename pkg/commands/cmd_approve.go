package commands

import (
	"context"
	"fmt"
	"strings"
)

func approveCommand() Definition {
	return Definition{
		Name:        "approve",
		Description: "Approve a tool call that is waiting for you",
		Usage:       "/approve <code>",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.ApproveToolCall == nil {
				return req.Reply(unavailableMsg)
			}
			code, refusal := approvalCode(req, "approve")
			if refusal != "" {
				return req.Reply(refusal)
			}
			tool, err := rt.ApproveToolCall(code, req.Channel, req.ChatID, req.SenderID)
			if err != nil {
				return req.Reply("Not approved: " + err.Error())
			}
			return req.Reply(fmt.Sprintf(
				"✅ Approved %s. Tell the agent to go ahead - it must repeat exactly the same call "+
					"before the approval expires, and it will run once.", tool))
		},
	}
}

func denyCommand() Definition {
	return Definition{
		Name:        "deny",
		Description: "Reject a tool call that is waiting for you",
		Usage:       "/deny <code>",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.DenyToolCall == nil {
				return req.Reply(unavailableMsg)
			}
			code, refusal := approvalCode(req, "deny")
			if refusal != "" {
				return req.Reply(refusal)
			}
			tool, err := rt.DenyToolCall(code, req.Channel, req.ChatID, req.SenderID)
			if err != nil {
				return req.Reply("Nothing denied: " + err.Error())
			}
			return req.Reply(fmt.Sprintf("🚫 Denied %s. It will not run.", tool))
		},
	}
}

// approvalCode extracts the code and refuses requests that cannot come from a
// person.
//
// handleCommand dispatches every inbound message that starts with "/", and not
// all of those are typed by the operator. Async tool results are re-published
// inbound on the "system" channel with an "async:<tool>" sender and content the
// model influences; subagent turns arrive on "subagent"; scheduled jobs carry
// the sender "cron". The approval store already rejects any identity other
// than the requester's, so this is defence in depth - but it is the cheap,
// explicit statement that none of these sources may approve anything.
//
// The one exception is the local CLI, which ProcessDirect also labels "cron".
// There the operator is at the terminal, and the code was written to stderr
// rather than to anything the model can read.
//
// It returns the code, or a non-empty refusal to reply with instead.
func approvalCode(req Request, verb string) (code, refusal string) {
	if req.Channel == "system" || req.Channel == "subagent" ||
		strings.HasPrefix(req.SenderID, "async:") ||
		(req.SenderID == "cron" && req.Channel != "cli") {
		return "", fmt.Sprintf("/%s is only accepted from a person.", verb)
	}

	code = nthToken(req.Text, 1)
	if code == "" {
		return "", fmt.Sprintf("Usage: /%s <code>", verb)
	}
	return code, ""
}
