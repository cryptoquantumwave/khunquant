package channels

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

// The button codec.
//
// A button carries "kq1|<action>|<payload>": a version, a short action code and
// an opaque payload. Every channel that renders bus.Action as buttons uses this
// one codec, so a tap means the same thing everywhere; only the size limit of the
// platform's own field differs (Telegram callback_data 64 bytes, LINE postback
// data 300, Discord custom_id 100, Slack button value 2000), and the longest
// string the codec produces is 10 + bus.MaxActionPayloadBytes bytes, which fits
// them all. Nothing in the string names a sender, a chat or an outcome.

// CallbackVersion starts every callback data string. It is bumped if the layout
// changes; a string with another prefix is dropped.
const CallbackVersion = "kq1"

const callbackSep = "|"

// callbackActions maps the short code in the callback data to the action ID the
// control path knows (pkg/control's button action IDs). Confirm-intent ("ci") is
// not here because the control path has no handler for it yet: a button for an
// action that is not listed is not sent.
var callbackActions = map[string]string{
	"apv":   "approve",
	"rej":   "reject",
	"snz1h": "snooze_1h",
	"snz4h": "snooze_4h",
	"snz1d": "snooze_1d",
	"use":   "useful",
	"nu":    "not_useful",
	"mute":  "mute",
}

// callbackCodes is callbackActions the other way round.
var callbackCodes = func() map[string]string {
	m := make(map[string]string, len(callbackActions))
	for code, id := range callbackActions {
		m[id] = code
	}
	return m
}()

// CallbackCodes returns a copy of the action ID -> short code table.
func CallbackCodes() map[string]string {
	m := make(map[string]string, len(callbackCodes))
	for id, code := range callbackCodes {
		m[id] = code
	}
	return m
}

// CallbackCode returns the short code of an action ID.
func CallbackCode(actionID string) (string, bool) {
	code, ok := callbackCodes[actionID]
	return code, ok
}

// validPayloadByte is what a payload may be made of: an approval payload
// (<code>.<nonce>) and an intervention id both fit. The separator of the data
// string and any whitespace are excluded, so a payload cannot shift the fields.
func validPayloadByte(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	}
	return b == '.' || b == '_' || b == '-' || b == ':'
}

// checkPayload validates the payload of a button. Approve and reject need the
// <code>.<nonce> shape; the other actions take whatever opaque id they carry, or
// nothing.
func checkPayload(code, payload string) error {
	if len(payload) > bus.MaxActionPayloadBytes {
		return fmt.Errorf("payload is %d bytes, the limit is %d", len(payload), bus.MaxActionPayloadBytes)
	}
	for i := 0; i < len(payload); i++ {
		if !validPayloadByte(payload[i]) {
			return errors.New("payload has a character that is not allowed")
		}
	}
	if code == "apv" || code == "rej" {
		before, after, ok := strings.Cut(payload, ".")
		if !ok || before == "" || after == "" || strings.Contains(after, ".") {
			return errors.New("an approval payload must be <code>.<nonce>")
		}
	}
	return nil
}

// EncodeCallbackData builds "kq1|<action>|<payload>" for a button. A string over
// maxBytes (the platform's limit), an action the control path does not know and a
// payload that does not fit are programming errors: they are returned, never
// truncated.
func EncodeCallbackData(a bus.Action, maxBytes int) (string, error) {
	code, ok := callbackCodes[a.ID]
	if !ok {
		return "", fmt.Errorf("action %q has no callback code", a.ID)
	}
	if err := checkPayload(code, a.Payload); err != nil {
		return "", fmt.Errorf("action %q: %w", a.ID, err)
	}
	data := CallbackVersion + callbackSep + code + callbackSep + a.Payload
	if len(data) > maxBytes {
		return "", fmt.Errorf("action %q: callback data is %d bytes, the limit is %d",
			a.ID, len(data), maxBytes)
	}
	return data, nil
}

// ParseCallbackData is the strict inverse of EncodeCallbackData. It returns the
// control path's action ID and the payload. An unknown version, an unknown action
// code, a field count other than three, a malformed payload or data over maxBytes
// is an error.
func ParseCallbackData(data string, maxBytes int) (actionID, payload string, err error) {
	if data == "" || len(data) > maxBytes {
		return "", "", errors.New("callback data has the wrong size")
	}
	parts := strings.Split(data, callbackSep)
	if len(parts) != 3 {
		return "", "", errors.New("callback data does not have three fields")
	}
	if parts[0] != CallbackVersion {
		return "", "", errors.New("unknown callback version")
	}
	actionID, ok := callbackActions[parts[1]]
	if !ok {
		return "", "", errors.New("unknown callback action")
	}
	if err := checkPayload(parts[1], parts[2]); err != nil {
		return "", "", err
	}
	return actionID, parts[2], nil
}
