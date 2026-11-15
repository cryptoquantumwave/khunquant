package simchat

import (
	"strings"
	"testing"
)

// An op decodes its arguments strictly: a misspelt key is a refusal, where it used to be
// dropped (mentoin: true sent a group message that the trigger then ignored).
func TestOpsRefuseAnUnknownArgument(t *testing.T) {
	r := newRig(t)
	if _, err := r.run("send", `{"as":"owner","text":"hi","mentoin":true}`); err == nil || !strings.Contains(err.Error(), "mentoin") {
		t.Fatalf("send with mentoin = %v, want an unknown-field refusal", err)
	}
	// The lab's op key rides in the body, as the scenario runner and the HTTP route send it.
	if res := r.mustRun("send", `{"op":"send","as":"owner","text":"hi"}`); res["delivered"] != true {
		t.Fatalf("send with op in the body = %v", res)
	}
	// Negative control: the same message spelt right is delivered.
	if res := r.mustRun("send", `{"as":"owner","text":"hi","mention":true}`); res["delivered"] != true {
		t.Fatalf("send with mention = %v", res)
	}
}
