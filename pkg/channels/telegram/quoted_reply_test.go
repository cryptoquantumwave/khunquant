package telegram

import (
	"strings"
	"testing"

	"github.com/mymmrac/telego"
)

// A Telegram reply carries the message it answers. Without that context the
// agent saw only the new text, so "yes, do that" arrived with no indication of
// what "that" was.

func TestTelegramQuotedContent_DescribesWhatWasQuoted(t *testing.T) {
	tests := []struct {
		name string
		msg  *telego.Message
		want []string
	}{
		{"text", &telego.Message{Text: "the original"}, []string{"the original"}},
		{"caption", &telego.Message{Caption: "a caption"}, []string{"a caption"}},
		{"photo", &telego.Message{Photo: []telego.PhotoSize{{FileID: "f"}}}, []string{"[image: photo]"}},
		{"voice", &telego.Message{Voice: &telego.Voice{FileID: "f"}}, []string{"[voice]"}},
		{"audio", &telego.Message{Audio: &telego.Audio{FileID: "f"}}, []string{"[audio]"}},
		{"document", &telego.Message{Document: &telego.Document{FileID: "f"}}, []string{"[file]"}},
		{
			"caption alongside media",
			&telego.Message{Caption: "look at this", Photo: []telego.PhotoSize{{FileID: "f"}}},
			[]string{"look at this", "[image: photo]"},
		},
		{"nil message", nil, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := telegramQuotedContent(tc.msg)
			if len(tc.want) == 0 {
				if got != "" {
					t.Errorf("got %q, want empty", got)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("got %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestTelegramQuotedAuthor_PrefersUsername(t *testing.T) {
	tests := []struct {
		name string
		msg  *telego.Message
		want string
	}{
		{"username", &telego.Message{From: &telego.User{Username: "alice", FirstName: "Alice"}}, "alice"},
		{"falls back to first name", &telego.Message{From: &telego.User{FirstName: "Bob"}}, "Bob"},
		{"neither", &telego.Message{From: &telego.User{}}, "unknown"},
		{"no sender", &telego.Message{}, "unknown"},
		{"nil message", nil, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramQuotedAuthor(tc.msg); got != tc.want {
				t.Errorf("telegramQuotedAuthor() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The role tells the agent whether it is looking at its own earlier message or
// someone else's — which changes what a reply to it means.
func TestTelegramQuotedRole(t *testing.T) {
	c := &TelegramChannel{}

	tests := []struct {
		name string
		msg  *telego.Message
		want string
	}{
		{"human", &telego.Message{From: &telego.User{IsBot: false}}, "user"},
		{"another bot", &telego.Message{From: &telego.User{IsBot: true, Username: "other"}}, "bot"},
		{"channel post", &telego.Message{SenderChat: &telego.Chat{ID: 1}}, "chat"},
		{"unknown", &telego.Message{}, "unknown"},
		{"nil", nil, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.telegramQuotedRole(tc.msg); got != tc.want {
				t.Errorf("telegramQuotedRole() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrependTelegramQuotedReply(t *testing.T) {
	c := &TelegramChannel{}
	quoted := &telego.Message{
		Text: "should we deploy?",
		From: &telego.User{Username: "alice"},
	}

	t.Run("prepends labelled context", func(t *testing.T) {
		got := c.prependTelegramQuotedReply("yes", quoted)
		for _, want := range []string{"quoted", "alice", "should we deploy?", "yes"} {
			if !strings.Contains(got, want) {
				t.Errorf("got %q, want it to contain %q", got, want)
			}
		}
		// The reply must come after the quote, or the agent reads the answer
		// before the question.
		if strings.Index(got, "should we deploy?") > strings.Index(got, "yes") {
			t.Errorf("quote appears after the reply: %q", got)
		}
	})

	t.Run("reply with no text still carries the quote", func(t *testing.T) {
		got := c.prependTelegramQuotedReply("", quoted)
		if !strings.Contains(got, "should we deploy?") {
			t.Errorf("got %q, want the quoted text", got)
		}
	})

	t.Run("empty quote leaves content untouched", func(t *testing.T) {
		if got := c.prependTelegramQuotedReply("hello", &telego.Message{}); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})
}

// Audio in a quoted message is attached so the agent can hear what is being
// referred to, rather than only being told that a voice note exists.
func TestQuotedTelegramMediaRefs_ResolvesAudio(t *testing.T) {
	var asked []string
	resolve := func(fileID, ext, filename string) string {
		asked = append(asked, fileID+ext)
		return "ref://" + filename
	}

	refs := quotedTelegramMediaRefs(&telego.Message{
		Voice: &telego.Voice{FileID: "v"},
		Audio: &telego.Audio{FileID: "a"},
	}, resolve)

	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2: %v", len(refs), refs)
	}
	if refs[0] != "ref://voice.ogg" || refs[1] != "ref://audio.mp3" {
		t.Errorf("refs = %v, want voice then audio", refs)
	}
	if len(asked) != 2 || asked[0] != "v.ogg" || asked[1] != "a.mp3" {
		t.Errorf("resolver saw %v, want v.ogg then a.mp3", asked)
	}

	if got := quotedTelegramMediaRefs(nil, resolve); got != nil {
		t.Errorf("nil message returned %v", got)
	}
	if got := quotedTelegramMediaRefs(&telego.Message{}, nil); got != nil {
		t.Errorf("nil resolver returned %v", got)
	}
	// A resolver that fails must contribute nothing rather than an empty ref.
	empty := quotedTelegramMediaRefs(
		&telego.Message{Voice: &telego.Voice{FileID: "v"}},
		func(string, string, string) string { return "" },
	)
	if len(empty) != 0 {
		t.Errorf("failed resolution produced %v, want nothing", empty)
	}
}
