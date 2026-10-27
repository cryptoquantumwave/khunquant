//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// instanceToken reads the token out of the environment of the instance process. It is
// the test's own side channel (same user, same machine) to learn what the agent must
// never be shown.
func instanceToken(t *testing.T, pid int) string {
	t.Helper()
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
		for _, kv := range strings.Split(string(b), "\x00") {
			if v, ok := strings.CutPrefix(kv, "KQ_SANDBOX_TOKEN="); ok {
				return v
			}
		}
	}
	out, err := exec.Command("ps", "eww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err == nil {
		for _, f := range strings.Fields(string(out)) {
			if v, ok := strings.CutPrefix(f, "KQ_SANDBOX_TOKEN="); ok {
				return v
			}
		}
	}
	t.Fatalf("could not read KQ_SANDBOX_TOKEN from pid %d", pid)
	return ""
}

// descendants lists the command lines of every process below pid.
func descendants(t *testing.T, pid int) []string {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	parent := map[int]int{}
	cmdline := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		parent[p], cmdline[p] = pp, strings.Join(f[2:], " ")
	}
	var found []string
	for p := range parent {
		for q := p; q > 1; q = parent[q] {
			if parent[q] == pid {
				found = append(found, cmdline[p])
				break
			}
		}
	}
	return found
}

// TestE2E_UpProxyDown drives the real binary over stdio: `khunquant sandbox mcp`
// starts a locked gateway from the prebuilt binary (nothing is compiled), the agent
// scripts the LLM, chats as the owner and waits for the reply, kill-9s the gateway and
// finds the journal intact, and closing the client leaves no gateway behind.
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_UpProxyDown(t *testing.T) {
	bin := os.Getenv("KQ_SANDBOX_E2E_BIN")
	if bin == "" {
		t.Skip("set KQ_SANDBOX_E2E_BIN to a prebuilt khunquant binary to run this test")
	}
	base := sandboxEnv(t)
	cmd := exec.Command(bin, "sandbox", "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var changed atomic.Int32
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed.Add(1) },
	}).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `sandbox mcp`: %v\n%s", err, stderr.String())
	}
	var texts []string
	call := func(name string, args any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		texts = append(texts, sb.String())
		if res.IsError {
			t.Fatalf("%s %v: %s\nstderr:\n%s", name, args, sb.String(), stderr.String())
		}
		return sb.String()
	}
	type info struct {
		Name     string `json:"name"`
		PID      int    `json:"pid"`
		State    string `json:"state"`
		LastExit *int   `json:"last_exit"`
	}
	decode := func(s string, v any) {
		t.Helper()
		if err := json.Unmarshal([]byte(s), v); err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) < 29 {
		t.Fatalf("tools/list before any instance: %d tools, %v", len(tools.Tools), err)
	}

	var up info
	decode(call("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t)}), &up)
	if up.State != "running" || up.PID <= 0 {
		t.Fatalf("up = %+v", up)
	}
	t.Logf("up: %+v", up)
	token := instanceToken(t, up.PID)
	for _, d := range descendants(t, cmd.Process.Pid) {
		if f := strings.Fields(d); len(f) > 0 && (f[0] == "go" || strings.HasSuffix(f[0], "/go")) {
			t.Errorf("a go process runs below the supervisor: %s", d)
		}
	}

	// GET /api/sandbox/lab and the dev-MCP endpoint work on the real gateway: the group
	// schemas are the instance's own, and a dev tool answers with no instance argument.
	waitFor(t, "tools/list_changed", func() bool { return changed.Load() > 0 })
	refined, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range refined.Tools {
		b, _ := json.Marshal(tl.InputSchema)
		switch tl.Name {
		case "sandbox_chat":
			if !strings.Contains(string(b), `"as"`) {
				t.Errorf("sandbox_chat was not refined from the instance: %s", b)
			}
		case "sandbox_llm":
			if !strings.Contains(string(b), `"rules"`) {
				t.Errorf("sandbox_llm was not refined from the instance: %s", b)
			}
		}
	}
	if status := call("service_status", nil); !strings.Contains(status, "{") {
		t.Errorf("service_status through the dev-MCP proxy = %q", status)
	}
	if call("list_tools", nil) == "" {
		t.Error("list_tools through the dev-MCP proxy is empty")
	}

	call("sandbox_llm", map[string]any{"op": "script_put", "rules": []any{
		map[string]any{"id": "any-user", "match": map[string]any{"turn": "user"}, "respond": map[string]any{"text": "scripted hello"}},
	}})
	call("sandbox_chat", map[string]any{"op": "send", "as": "owner", "text": "hello"})
	var waited struct {
		Matched bool `json:"matched"`
		Count   int  `json:"count"`
		NextSeq int  `json:"next_seq"`
	}
	decode(call("sandbox_wait_for", map[string]any{"match": map[string]any{"kind": "chat.out"}, "timeout_wall": "60s"}), &waited)
	if !waited.Matched || waited.Count < 1 {
		t.Fatalf("wait_for chat.out = %+v, want a match", waited)
	}
	type journal struct {
		Entries []struct{ Seq int } `json:"entries"`
	}
	var before, after journal
	decode(call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"chat.out"}, "limit": 20}), &before)
	t.Logf("wait_for chat.out: %+v; journal entries before the kill: %+v", waited, before.Entries)
	if len(before.Entries) == 0 {
		t.Fatal("the journal has no chat.out entry before the kill")
	}

	var killed info
	decode(call("sandbox_instance", map[string]any{"op": "restart", "mode": "kill"}), &killed)
	if killed.LastExit == nil || *killed.LastExit != 137 || killed.State != "running" || killed.PID == up.PID {
		t.Fatalf("restart kill = %+v, want last_exit 137 and a new pid", killed)
	}
	decode(call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"chat.out"}, "limit": 20}), &after)
	t.Logf("restart kill: pid %d -> %d, last_exit %d; journal entries after: %+v", up.PID, killed.PID, *killed.LastExit, after.Entries)
	if len(after.Entries) < len(before.Entries) || after.Entries[0].Seq != before.Entries[0].Seq {
		t.Errorf("the journal written before the kill is not readable after it: before %+v, after %+v", before, after)
	}

	call("sandbox_instance", map[string]any{"op": "down"})
	var gone struct {
		Instances []info `json:"instances"`
	}
	decode(call("sandbox_instance", map[string]any{"op": "list"}), &gone)
	if len(gone.Instances) != 0 {
		t.Errorf("instances after down: %+v", gone.Instances)
	}

	// Closing the client with an instance still up leaves no gateway behind.
	var second info
	decode(call("sandbox_instance", map[string]any{"op": "up", "name": "left-behind", "fixtures_from": repoFixtures(t)}), &second)
	if err := cs.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	waitFor(t, "the instance to die with its supervisor", func() bool { return syscall.Kill(second.PID, 0) != nil })

	if strings.Contains(stderr.String(), "could not refine") {
		t.Errorf("the supervisor could not refine its tools against the real gateway:\n%s", stderr.String())
	}
	all := strings.Join(texts, "\n") + stderr.String()
	if strings.Contains(all, token) {
		t.Error("the instance token reached the agent or the supervisor's stderr")
	}
}
