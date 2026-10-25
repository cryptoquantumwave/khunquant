package api

import (
	"errors"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The owner's chat and sender IDs are identifiers, not credentials: they are
// not in config.CredentialFieldNames (that set is keyed by field name alone and
// "chat_id" / "sender_id" also name fields in channel configs that the dashboard
// must show and edit). They still say who receives the assistant's unprompted
// messages, so config responses carry them masked, exactly as the proactive page
// does (maskID), and a save that echoes the mask back keeps the stored value.
//
// The rule on the way back in, for the two ID fields of proactive.owner:
//
//   - a value equal to the mask of the stored ID: the stored ID is kept;
//   - omitted (PUT only; in a PATCH an omitted key is untouched anyway): the
//     stored value is kept, and so is the channel when it is omitted;
//   - any other value that contains "***": rejected, it is a mask of something
//     else and must never be stored as an ID;
//   - anything else, including an explicit "" or null, is a new value. A changed
//     owner is judged by config.ValidateOwnerInput, the validator behind the
//     dedicated owner route, so a save cannot set an owner that route would
//     refuse. The only way to clear the owner here is to send all three fields
//     as "" (the dedicated DELETE does the same in one call); clearing one ID
//     alone leaves an incomplete owner, which is refused.
//
// An owner identical to the stored one is never re-validated, so a no-op save
// still works when a hand-edited config.json holds a partial owner.

// ownerMaskMarker is what every non-empty mask produced by maskID contains.
const ownerMaskMarker = "***"

var errOwnerMaskMismatch = errors.New(
	"proactive.owner chat_id / sender_id look masked but do not match the stored owner; " +
		"send the real ID, or omit the field to keep the stored value")

// maskOwnerInTree masks proactive.owner.chat_id and sender_id in a marshaled
// config tree. Only that exact path is touched: channel configs have chat_id,
// sender_id and allow_from fields of their own that stay as they are.
func maskOwnerInTree(tree any) {
	root, _ := tree.(map[string]any)
	proactive, _ := root["proactive"].(map[string]any)
	owner, _ := proactive["owner"].(map[string]any)
	for _, key := range []string{"chat_id", "sender_id"} {
		if s, ok := owner[key].(string); ok {
			owner[key] = maskID(s)
		}
	}
}

// restoreOwnerIDs rewrites proactive.owner in an incoming document (a PUT body,
// or a PATCH body when fillAbsent is false) against the stored owner, following
// the rule above. It returns errOwnerMaskMismatch for a mask-shaped value that
// is not the stored owner's mask. Values that are not strings are left for the
// decoder to reject.
func restoreOwnerIDs(doc map[string]any, stored config.OwnerConfig, fillAbsent bool) error {
	section, ok := doc["proactive"].(map[string]any)
	if !ok {
		if v, present := doc["proactive"]; (present && v != nil) || !fillAbsent {
			return nil
		}
		section = map[string]any{}
		doc["proactive"] = section
	}
	owner, ok := section["owner"].(map[string]any)
	if !ok {
		if v, present := section["owner"]; (present && v != nil) || !fillAbsent {
			return nil
		}
		owner = map[string]any{}
		section["owner"] = owner
	}

	if fillAbsent {
		for key, val := range map[string]string{
			"channel":   stored.Channel,
			"chat_id":   stored.ChatID,
			"sender_id": stored.SenderID,
		} {
			if _, present := owner[key]; !present {
				owner[key] = val
			}
		}
	}

	for key, storedVal := range map[string]string{"chat_id": stored.ChatID, "sender_id": stored.SenderID} {
		s, ok := owner[key].(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		switch {
		case storedVal != "" && s == maskID(storedVal):
			owner[key] = storedVal
		case strings.Contains(s, ownerMaskMarker):
			return errOwnerMaskMismatch
		}
	}
	return nil
}

// resolveOwnerSave decides what owner a generic config save stores. An owner
// unchanged from the stored one passes through untouched; an entirely empty one
// clears the owner; anything else must satisfy config.ValidateOwnerInput and is
// stored in the normalised form that route would store.
func resolveOwnerSave(stored, incoming config.OwnerConfig) (config.OwnerConfig, error) {
	// web_is_owner is a switch, not part of the identity: a pointer, so compared by
	// value, and carried through whatever is stored below.
	web := incoming.WebIsOwner
	same := func(a, b *bool) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	incNoWeb, stNoWeb := incoming, stored
	incNoWeb.WebIsOwner, stNoWeb.WebIsOwner = nil, nil
	if incNoWeb == stNoWeb {
		if same(web, stored.WebIsOwner) {
			return stored, nil
		}
		out := stored
		out.WebIsOwner = web
		return out, nil
	}
	if strings.TrimSpace(incoming.Channel) == "" && strings.TrimSpace(incoming.ChatID) == "" &&
		strings.TrimSpace(incoming.SenderID) == "" {
		return config.OwnerConfig{WebIsOwner: web}, nil
	}
	owner, err := config.ValidateOwnerInput(incoming.Channel, incoming.ChatID, incoming.SenderID)
	if err != nil {
		return config.OwnerConfig{}, errors.New("proactive.owner not saved: " + err.Error())
	}
	owner.WebIsOwner = web
	return owner, nil
}
