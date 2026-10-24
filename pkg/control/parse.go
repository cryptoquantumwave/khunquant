package control

import (
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
)

// Command names.
const (
	cmdApprove   = "approve"
	cmdReject    = "reject"
	cmdYes       = "yes" // a reply to an approval card that says "approve" in other words
	cmdSnooze    = "snooze"
	cmdMute      = "mute"
	cmdUnmute    = "unmute"
	cmdUseful    = "useful"
	cmdNotUseful = "notuseful"
	cmdWhoami    = "whoami"
	cmdStop      = "stop"
	cmdResume    = "resume"
)

// maxCommandBytes bounds the text of a command: nothing the control path
// recognises is longer, and a long message is never one.
const maxCommandBytes = 256

// command is a recognised input.
type command struct {
	name string
	args []string
	// nonce is the approval nonce a button carried; a typed command has none.
	nonce string
	// viaCallback marks a button press.
	viaCallback bool
}

// Button action IDs (bus.Action.ID) the callbacks understand.
const (
	actApprove   = "approve"
	actReject    = "reject"
	actSnooze1h  = "snooze_1h"
	actSnooze4h  = "snooze_4h"
	actSnooze1d  = "snooze_1d"
	actUseful    = "useful"
	actNotUseful = "not_useful"
	actMute      = "mute"
)

// classify decides, with no I/O and almost no allocation, whether in is a control
// input. A text command is recognised only as the WHOLE message: one line, the
// command, its arguments. Text inside a longer message, a quoted message or a
// forwarded one is never a command (it goes to the agent like any text, and no
// tool the model can call approves anything).
func classify(in Input) (command, bool) {
	if in.Callback != nil {
		return classifyCallback(in.Callback)
	}
	if in.Forwarded {
		return command{}, false
	}
	text := strings.TrimSpace(in.Text)
	if text == "" || len(text) > maxCommandBytes {
		return command{}, false
	}
	if text[0] != '/' {
		if in.ReplyToMessageID != "" && isYes(text) {
			return command{name: cmdYes}, true
		}
		return command{}, false
	}
	if strings.ContainsAny(text, "\r\n") {
		return command{}, false
	}
	fields := strings.Fields(text)
	name := strings.ToLower(fields[0][1:])
	if i := strings.IndexByte(name, '@'); i >= 0 { // Telegram's /name@bot
		name = name[:i]
	}
	args := fields[1:]
	switch name {
	case cmdApprove:
		// A proposal code is four characters. The operator approval of a held tool
		// call (pkg/tools/approval.go, the same /approve word) has an eight
		// character code; anything that is not shaped like a proposal code is left
		// to that path, which answers it as it always did.
		if len(args) == 0 || !approval.ValidCode(approval.NormalizeCode(args[0])) {
			return command{}, false
		}
		return command{name: cmdApprove, args: args}, true
	case cmdReject, cmdSnooze, cmdMute, cmdUnmute, cmdUseful, cmdNotUseful, cmdWhoami, cmdStop, cmdResume:
		return command{name: name, args: args}, true
	}
	return command{}, false
}

// yesWords are the replies to an approval card that approve it, compared in lower
// case: approve | yes | ok | อนุมัติ | ตกลง.
var yesWords = map[string]bool{
	"approve": true, "yes": true, "ok": true, "อนุมัติ": true, "ตกลง": true,
}

func isYes(text string) bool {
	if len(text) > 32 {
		return false
	}
	return yesWords[strings.ToLower(text)]
}

func classifyCallback(cb *Callback) (command, bool) {
	switch cb.Action {
	case actApprove, actReject:
		code, nonce, ok := approval.ParseButtonPayload(cb.Payload)
		if !ok {
			return command{}, false
		}
		name := cmdApprove
		if cb.Action == actReject {
			name = cmdReject
		}
		return command{name: name, args: []string{code}, nonce: nonce, viaCallback: true}, true
	case actSnooze1h:
		return command{name: cmdSnooze, args: []string{"1h"}, viaCallback: true}, true
	case actSnooze4h:
		return command{name: cmdSnooze, args: []string{"4h"}, viaCallback: true}, true
	case actSnooze1d:
		return command{name: cmdSnooze, args: []string{"1d"}, viaCallback: true}, true
	case actUseful:
		return command{name: cmdUseful, viaCallback: true}, true
	case actNotUseful:
		return command{name: cmdNotUseful, viaCallback: true}, true
	case actMute:
		return command{name: cmdMute, viaCallback: true}, true
	}
	return command{}, false
}
