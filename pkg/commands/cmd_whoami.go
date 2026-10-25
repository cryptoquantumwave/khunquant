package commands

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/control/whoami"
)

// whoamiCommand is the forwarding alias of /whoami.
//
// The command itself is answered by the control path (pkg/control), which sees a
// message before the agent does, on every channel. This definition stays so that
// the help text and a platform's command menu (Telegram's) still list it, and so
// that a message that reaches the agent anyway (before the control path is
// installed at startup, or on a front end that has none) gets the same answer.
// Both build the text with pkg/control/whoami.
func whoamiCommand() Definition {
	return Definition{
		Name:        "whoami",
		Description: "Show your channel, chat and sender IDs (for proactive.owner)",
		Usage:       "/whoami",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			return req.Reply(formatWhoami(req, rt))
		},
	}
}

func formatWhoami(req Request, rt *Runtime) string {
	q := whoami.Query{Channel: req.Channel, ChatID: req.ChatID, SenderID: req.SenderID, PeerKind: req.PeerKind}
	if rt == nil {
		return whoami.Text(q, nil)
	}
	return whoami.Text(q, rt.Config)
}
