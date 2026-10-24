package api

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// An explicit proactive.owner.web_is_owner:false survives every way a generic config
// save can touch the owner: unchanged, changed, cleared, or the switch itself flipped.
func TestResolveOwnerSave_KeepsTheWebSwitch(t *testing.T) {
	off, on := false, true
	stored := config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001", WebIsOwner: &off}

	t.Run("unchanged", func(t *testing.T) {
		in := stored
		in.WebIsOwner = &off // a different pointer, the same value
		got, err := resolveOwnerSave(stored, in)
		if err != nil || got.WebIsOwnerEnabled() {
			t.Fatalf("(%+v, %v): the switch was lost", got, err)
		}
	})
	t.Run("owner changed", func(t *testing.T) {
		in := config.OwnerConfig{Channel: "telegram", ChatID: "2002", SenderID: "telegram:2002", WebIsOwner: &off}
		got, err := resolveOwnerSave(stored, in)
		if err != nil || got.ChatID != "2002" || got.WebIsOwnerEnabled() {
			t.Fatalf("(%+v, %v)", got, err)
		}
	})
	t.Run("owner cleared", func(t *testing.T) {
		got, err := resolveOwnerSave(stored, config.OwnerConfig{WebIsOwner: &off})
		if err != nil || got.SenderID != "" || got.WebIsOwnerEnabled() {
			t.Fatalf("(%+v, %v)", got, err)
		}
	})
	t.Run("only the switch flipped", func(t *testing.T) {
		in := stored
		in.WebIsOwner = &on
		got, err := resolveOwnerSave(stored, in)
		if err != nil || got.SenderID != "telegram:1001" || !got.WebIsOwnerEnabled() {
			t.Fatalf("(%+v, %v)", got, err)
		}
	})
}
