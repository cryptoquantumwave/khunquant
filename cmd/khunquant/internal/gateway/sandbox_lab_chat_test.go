package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/channels/simchat"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// chatOutbox returns the messages the chat group's outbox shows for a chat.
func chatOutbox(t *testing.T, url, to string) []map[string]any {
	t.Helper()
	code, body, err := postJSON(url, lockTestToken, `{"op":"outbox","to":"`+to+`"}`)
	if err != nil || code != http.StatusOK {
		t.Fatalf("POST chat outbox = %d (%v) %s", code, err, body)
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode outbox: %v (%s)", err, body)
	}
	return out.Messages
}

// waitChatOutbox waits until the chat's outbox holds n messages (a control reply is
// delivered by the manager's worker, a moment after send returns).
func waitChatOutbox(t *testing.T, url, to string, n int) []map[string]any {
	t.Helper()
	var msgs []map[string]any
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if msgs = chatOutbox(t, url, to); len(msgs) >= n {
			return msgs
		}
	}
	t.Fatalf("outbox of %s has %d messages, want %d", to, len(msgs), n)
	return nil
}

// A locked gateway runs simchat (built by the real adapter, no stub), serves the chat
// group over HTTP and MCP, and still does after a reload built a new channel: the
// world is the process's, the channel is the generation's, and the reload re-attached.
func TestLabChatSurvivesReload(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	chatURL := f.url("/api/sandbox/lab/chat")

	if got := f.services.ChannelManager.GetEnabledChannels(); !slices.Equal(got, []string{"simchat"}) {
		t.Fatalf("enabled channels = %v, want [simchat]", got)
	}
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_chat"] {
		t.Fatal("tools/list of the first generation has no sandbox_chat")
	}
	first, _ := f.services.ChannelManager.GetChannel("simchat")
	if f.shared.simWorld.Attached() != first {
		t.Fatal("the first generation's channel is not the world's attached channel")
	}

	// /whoami is answered by the control path, which needs no LLM and no owner: the
	// reply leaves through the real manager and lands in the outbox.
	if code, body, err := postJSON(chatURL, lockTestToken, `{"op":"send","as":"stranger","text":"/whoami"}`); err != nil || code != http.StatusOK {
		t.Fatalf("POST chat send = %d (%v) %s", code, err, body)
	}
	msgs := waitChatOutbox(t, chatURL, "dm:stranger", 1)
	if txt, _ := msgs[0]["text"].(string); !strings.Contains(txt, "simchat:stranger") || msgs[0]["delivered"] != true {
		t.Fatalf("the /whoami reply = %v, want delivered text naming simchat:stranger", msgs[0])
	}
	// The same message is what inspect's outbox reads from the journal.
	code, body, err := postJSON(f.url("/api/sandbox/lab/inspect"), lockTestToken, `{"op":"outbox","to":"dm:stranger"}`)
	if err != nil || code != http.StatusOK || !strings.Contains(body, `"chat.out"`) {
		t.Fatalf("inspect outbox = %d (%v) %s, want the chat.out entry", code, err, body)
	}

	if err := f.reload(func(c *config.Config) { c.Debug.DevMCP.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	second, ok := f.services.ChannelManager.GetChannel("simchat")
	if !ok || second == first {
		t.Fatalf("the reload did not build a new simchat channel (%v, same=%v)", ok, second == first)
	}
	if f.shared.simWorld.Attached() != second.(*simchat.Channel) {
		t.Fatal("after the reload the world is attached to the old channel, not the new one")
	}
	if code, body, err := postJSON(chatURL, lockTestToken, `{"op":"send","as":"alice","text":"/whoami"}`); err != nil || code != http.StatusOK {
		t.Fatalf("POST chat send after the reload = %d (%v) %s", code, err, body)
	}
	waitChatOutbox(t, chatURL, "dm:alice", 1)
	if len(chatOutbox(t, chatURL, "dm:stranger")) != 1 {
		t.Fatal("the outbox lost the message from before the reload")
	}
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_chat"] {
		t.Fatal("tools/list after the reload has no sandbox_chat")
	}
}

// An ordinary gateway has no chat group and builds no simchat, whatever its config says.
func TestLabChatNotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, func(c *config.Config) {
		enableAllRoutes(c)
		c.Channels.SimChat.Enabled = true
	})
	if code, body, err := postJSON(f.url("/api/sandbox/lab/chat"), testSandboxToken, `{"op":"outbox"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/chat = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_chat"] {
		t.Fatal("an unlocked gateway lists sandbox_chat")
	}
	if f.shared.simWorld != nil || slices.Contains(f.services.ChannelManager.GetEnabledChannels(), "simchat") {
		t.Fatal("an unlocked gateway holds a simchat world or channel")
	}
}

// The real binary path: the profile `sandbox up` writes, a re-executed locked gateway,
// and the chat group answering over HTTP. Nothing is stubbed.
func TestLockedGatewayStartsWithSimChat(t *testing.T) {
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", "") // the profile must not pick it up
	port := freePort(t)
	if _, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: port}); err != nil {
		t.Fatal(err)
	}

	childHome, childTmp := t.TempDir(), t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "TMPDIR=" + childTmp, "HOME=" + childHome,
		"KHUNQUANT_HOME=" + home, "KHUNQUANT_CONFIG=" + home + "/config.json",
		"KQ_SANDBOX_LOCKED=1", "KQ_SANDBOX_TOKEN=" + lockTestToken, "KQ_SANDBOX_INSTANCE=subprocess",
	}
	p := launchLockedProcess(t, home, port, env, []string{home, childHome, childTmp})
	p.waitReady()
	p.waitOutput("Channels enabled: [simchat]", 20*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d/api/sandbox/lab/chat", port)
	if code, body, err := postJSON(url, lockTestToken, `{"op":"send","as":"stranger","text":"/whoami"}`); err != nil || code != http.StatusOK {
		t.Fatalf("POST chat send = %d (%v) %s\n%s", code, err, body, p.out.String())
	}
	msgs := waitChatOutbox(t, url, "dm:stranger", 1)
	if txt, _ := msgs[0]["text"].(string); !strings.Contains(txt, "simchat:stranger") {
		t.Fatalf("the reply = %v", msgs[0])
	}
}
