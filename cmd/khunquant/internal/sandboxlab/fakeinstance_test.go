//go:build !windows

package sandboxlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/pid"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// The MCP tests start a stand-in for a locked gateway: this test binary, re-executed
// with -test.run=^TestHelperFakeInstance$. Nothing is compiled and no real gateway
// runs. It serves what the supervisor talks to: /health, /api/sandbox/status, the lab
// routes (a "chat" group and the real journal's "inspect wait_for"), and a dev-MCP
// server. It prints its token into its log and leaks it in replies, so the tests can
// show that nothing reaches the agent.

const fakeInstanceMarker = "--fake-instance"

func TestHelperFakeInstance(t *testing.T) {
	if os.Getenv("KQ_SANDBOX_LOCKED") != "1" || !slices.Contains(os.Args, fakeInstanceMarker) {
		t.Skip("helper process for the MCP tests")
	}
	home, token := os.Getenv("KHUNQUANT_HOME"), os.Getenv("KQ_SANDBOX_TOKEN")
	fmt.Println("fake instance starting, token=" + token)

	var cfg struct {
		Gateway struct {
			Port int `json:"port"`
		} `json:"gateway"`
	}
	raw, err := os.ReadFile(os.Getenv("KHUNQUANT_CONFIG"))
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil || cfg.Gateway.Port == 0 {
		fmt.Println("fake instance: bad config:", err)
		os.Exit(2)
	}
	if _, err := pid.WritePidFile(home, "127.0.0.1", cfg.Gateway.Port); err != nil {
		fmt.Println("fake instance: pid file:", err)
		os.Exit(2)
	}
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(home, "lab", "journal.db")})
	if err != nil {
		fmt.Println("fake instance: journal:", err)
		os.Exit(2)
	}

	guard := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(http.ResponseWriter, *http.Request) {})
	mux.Handle("GET /api/sandbox/status", guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"enabled": true, "locked": true, "running": true})
	})))
	mux.Handle("GET /api/sandbox/lab", guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"groups": []any{
			map[string]any{"name": "chat", "description": "fake chat", "ops": []any{
				map[string]any{"name": "echo", "description": "echo the body", "args": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
				map[string]any{"name": "emit", "description": "append a journal entry", "args": map[string]any{"kind": map[string]any{"type": "string"}}},
				map[string]any{"name": "leak", "description": "reply with the token"},
			}},
			map[string]any{"name": "inspect", "description": "fake inspect", "ops": []any{
				map[string]any{"name": "wait_for", "description": "wait", "args": map[string]any{"match": map[string]any{"type": "object"}}, "required": []string{"match"}},
			}},
		}})
	})))
	mux.Handle("POST /api/sandbox/lab/{group}", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Op          string           `json:"op"`
			Kind        string           `json:"kind"`
			Match       labjournal.Match `json:"match"`
			AfterSeq    int64            `json:"after_seq"`
			TimeoutWall string           `json:"timeout_wall"`
		}
		_ = json.Unmarshal(body, &req)
		switch group := r.PathValue("group"); {
		case group == "chat" && req.Op == "echo":
			reply(w, 200, map[string]any{"got": string(body)})
		case group == "chat" && req.Op == "emit":
			j.Append(labjournal.Entry{Kind: req.Kind})
			_ = j.Flush(r.Context())
			reply(w, 200, map[string]any{"ok": true})
		case group == "chat" && req.Op == "leak":
			reply(w, 200, map[string]any{"token": token})
		case group == "inspect" && req.Op == "wait_for":
			wall, _ := time.ParseDuration(req.TimeoutWall)
			res, err := j.WaitFor(r.Context(), req.Match, req.AfterSeq, wall)
			if err != nil {
				reply(w, 400, map[string]any{"error": err.Error(), "code": "bad_args"})
				return
			}
			if res.Entries == nil {
				res.Entries = []labjournal.Entry{}
			}
			reply(w, 200, res)
		default:
			reply(w, 404, map[string]any{"error": fmt.Sprintf("no lab group %q", group), "code": "unknown_group"})
		}
	})))

	devSrv := func(*http.Request) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "fake-dev", Version: "1"}, nil)
		s.AddTool(&mcp.Tool{Name: "read_config", Description: "fake config", InputSchema: json.RawMessage(`{"type":"object","properties":{"section":{"type":"string"}}}`)},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				txt := "management_token=" + token + " args=" + string(req.Params.Arguments)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: txt}}}, nil
			})
		s.AddTool(&mcp.Tool{Name: "service_status", Description: "fake status", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
		return s
	}
	dev := guard(http.StripPrefix("/dev-mcp", mcp.NewStreamableHTTPHandler(devSrv, &mcp.StreamableHTTPOptions{JSONResponse: true})))
	mux.Handle("/dev-mcp", dev)
	mux.Handle("/dev-mcp/", dev)

	go func() { _ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", cfg.Gateway.Port), mux) }()

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	<-term
	_ = j.Close()
	pid.RemovePidFile(home)
	os.Exit(0)
}

// fakeSupervisor is a supervisor whose instances are the fake above.
func fakeSupervisor(t *testing.T) *supervisor.Supervisor {
	t.Helper()
	sup, err := supervisor.New(supervisor.Config{
		Binary: os.Args[0], TmpDir: sandboxEnv(t), MinFreeBytes: 1, TermGrace: 2 * time.Second,
		PreArgs: []string{"-test.run=^TestHelperFakeInstance$", "-test.timeout=2m", "--"}, ExtraArgs: []string{fakeInstanceMarker},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close(context.Background()) })
	return sup
}
