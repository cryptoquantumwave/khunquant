package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// addProbeGroup registers a group whose only op echoes the request object.
func addProbeGroup(t *testing.T, l *lab.Lab) {
	t.Helper()
	err := l.AddGroup(lab.Group{Name: "probe", Ops: []lab.Op{{Name: "echo",
		Run: func(_ context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"args": args}, nil
		}}}})
	if err != nil {
		t.Fatal(err)
	}
}

func postJSON(url, token, body string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestLabAPIGuard(t *testing.T) {
	f := newLockedFixture(t)
	addProbeGroup(t, f.lab)
	url := f.url("/api/sandbox/lab/probe")

	for _, tc := range []struct {
		name, token, url string
		want             int
	}{
		{"no token", "", url, http.StatusUnauthorized},
		{"wrong token", "not-the-token", url, http.StatusUnauthorized},
		{"the config's management token is not the instance token", testMgmtToken + "x", url, http.StatusUnauthorized},
		{"instance token", lockTestToken, url, http.StatusOK},
		{"unknown group", lockTestToken, f.url("/api/sandbox/lab/nope"), http.StatusNotFound},
	} {
		code, body, err := postJSON(tc.url, tc.token, `{"op":"echo","x":1}`)
		if err != nil || code != tc.want {
			t.Errorf("%s: status %d (%v) %s, want %d", tc.name, code, err, body, tc.want)
		}
	}
	if code, body, _ := postJSON(url, lockTestToken, `{"op":"echo","x":1}`); !strings.Contains(body, `"x":1`) {
		t.Errorf("the op did not get the request object (status %d): %s", code, body)
	}
	if code, _, _ := getStatus(url, lockTestToken); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}

	// A caller that is not on loopback is refused before the token is looked at.
	h := labGuard(lockTestToken)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached the handler") }))
	req := httptest.NewRequest(http.MethodPost, "/api/sandbox/lab/probe", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("Authorization", "Bearer "+lockTestToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback = %d, want 403", rec.Code)
	}
}

// A locked process cannot have an empty token (Lock and lab.New refuse one), so the
// guard is built straight from "": bearerTokenMiddleware would let everything through.
func TestLabAPIRefusesEmptyToken(t *testing.T) {
	h := labGuard("")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached the handler") }))
	for _, auth := range []string{"", "Bearer ", "Bearer anything"} {
		req := httptest.NewRequest(http.MethodPost, "/api/sandbox/lab/inspect", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("Authorization %q: status %d, want 503", auth, rec.Code)
		}
	}
}

func labSubscribers(f *lockedFixture) (n int) {
	for _, s := range f.shared.eventBus().Stats().SubscriberStats {
		if s.Name == "labjournal" {
			n++
		}
	}
	return n
}

// Everything the lab adds is per process or per generation exactly once: a reload
// keeps the one observer (a second would journal every event twice) and the routes.
func TestLabJournalSurvivesReloadWithOneObserver(t *testing.T) {
	f := newLockedFixture(t)
	addProbeGroup(t, f.lab)
	if got := labSubscribers(f); got != 1 {
		t.Fatalf("labjournal subscribers after start = %d, want 1", got)
	}
	for i := 0; i < 2; i++ {
		if err := f.reload(nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := labSubscribers(f); got != 1 {
		t.Fatalf("labjournal subscribers after two reloads = %d, want 1", got)
	}
	if code, body, err := postJSON(f.url("/api/sandbox/lab/probe"), lockTestToken, `{"op":"echo"}`); err != nil || code != 200 {
		t.Fatalf("lab route after reloads: %d %v %s", code, err, body)
	}

	f.shared.eventBus().PublishNonBlocking(events.Event{
		Kind: events.KindAgentError, Source: events.Source{Component: "lab-test"}, Attrs: map[string]any{"subject": "marker"}})
	var got []labjournal.Entry
	for deadline := time.Now().Add(5 * time.Second); len(got) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var err error
		if got, err = f.lab.Journal.Query(context.Background(), labjournal.Query{Subject: "marker"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // a duplicate observer would have written by now
	got, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Subject: "marker"})
	if err != nil || len(got) != 1 || got[0].Kind != "event.agent.error" {
		t.Fatalf("journal entries for the marker event = %+v (%v), want exactly one event.agent.error", got, err)
	}
	if info, err := os.Stat(filepath.Join(f.lab.Home, "lab", "journal.db")); err != nil || info.IsDir() {
		t.Fatalf("journal file under the instance home: %v", err)
	}
}

// An ordinary gateway has no lab route, no lab journal and no observer, even with
// dev MCP and the sandbox on.
func TestLabNotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if code, body, err := postJSON(f.url("/api/sandbox/lab/inspect"), testSandboxToken, `{"op":"journal"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/inspect = %d (%v) %s, want 404", code, err, body)
	}
	for _, s := range f.shared.eventBus().Stats().SubscriberStats {
		if s.Name == "labjournal" {
			t.Fatal("an unlocked gateway has a labjournal subscriber")
		}
	}
	if f.shared.labObserver != nil || f.shared.lab != nil || f.shared.labTapped != nil || f.shared.labGen.Load() != nil {
		t.Fatal("an unlocked gateway holds lab state (observer, lab, tool tap or published generation)")
	}
	// No registry is tapped, and no lab tool reaches the dev-MCP server (which is on).
	if mcpTools(t, f, testDevMCPToken)["sandbox_inspect"] {
		t.Fatal("an unlocked gateway lists sandbox_inspect")
	}

	if _, err := os.Stat(filepath.Join(config.HomeDir(), "lab")); err == nil {
		t.Fatal("an unlocked gateway created a lab directory")
	}
}

// Under the lock the in-instance dev-MCP bearer is the instance token. The token in
// config (a random one is generated and saved when there is none) opens nothing.
func TestDevMCPLockedBearerIsInstanceToken(t *testing.T) {
	f := newLockedFixture(t)
	if err := f.reload(func(c *config.Config) { c.Debug.DevMCP.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	f.expectStatus("/dev-mcp", "", http.StatusUnauthorized)
	f.expectStatus("/dev-mcp", testDevMCPToken, http.StatusUnauthorized)
	if code, _, err := getStatus(f.url("/dev-mcp"), lockTestToken); err != nil || code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Fatalf("the instance token was refused by dev MCP: %d %v", code, err)
	}
}

// The locked gateway as a child process: with dev MCP on and a token in the config,
// the config file is not rewritten, neither token is printed or logged, only the
// instance token opens /dev-mcp, and the lab route answers.
func TestDevMCPLockedUsesInstanceToken(t *testing.T) {
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home) // for DefaultConfig in writeSandboxConfig only
	port := freePort(t)
	const cfgToken = "cfg-devmcp-token-0123456789abcdef0123456789"
	writeSandboxConfig(t, home, port, func(c *config.Config) {
		c.Debug.DevMCP.Enabled, c.Debug.DevMCP.Token = true, cfgToken
	})
	cfgPath := filepath.Join(home, "config.json")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	childHome, childTmp := t.TempDir(), t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "TMPDIR=" + childTmp, "HOME=" + childHome,
		"KHUNQUANT_HOME=" + home, "KHUNQUANT_CONFIG=" + cfgPath,
		"KQ_SANDBOX_LOCKED=1", "KQ_SANDBOX_TOKEN=" + lockTestToken, "KQ_SANDBOX_INSTANCE=subprocess",
	}
	p := launchLockedProcess(t, home, port, env, []string{home, childHome, childTmp})
	p.waitReady()
	p.waitOutput("Dev MCP", 20*time.Second)

	if code, _, _ := p.get("/dev-mcp", cfgToken); code != http.StatusUnauthorized {
		t.Errorf("/dev-mcp with the config's token = %d, want 401", code)
	}
	if code, _, _ := p.get("/dev-mcp", lockTestToken); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("/dev-mcp with the instance token = %d", code)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/sandbox/lab/inspect", port)
	if code, body, err := postJSON(url, lockTestToken, `{"op":"journal","limit":5}`); err != nil || code != http.StatusOK ||
		!strings.Contains(body, `"entries"`) || !strings.Contains(body, `"drops"`) {
		t.Errorf("inspect journal on the real gateway's first generation = %d (%v) %s, want 200 with entries and drops", code, err, body)
	}
	if !mcpToolsAt(t, fmt.Sprintf("http://127.0.0.1:%d/dev-mcp", port), lockTestToken)["sandbox_inspect"] {
		t.Error("tools/list of the real gateway's first generation has no sandbox_inspect")
	}
	if code, _, _ := postJSON(url, "", `{}`); code != http.StatusUnauthorized {
		t.Errorf("lab route without a token = %d, want 401", code)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("config.json changed while the locked gateway ran (err %v)", err)
	}
	out := p.out.String()
	for name, tok := range map[string]string{"instance": lockTestToken, "config dev-MCP": cfgToken} {
		if strings.Contains(out, tok) {
			t.Errorf("the %s token is in the gateway's output:\n%s", name, out)
		}
	}
	// Log files: the instance token nowhere, the config's token only in the config itself.
	scanned := 0
	for _, dir := range p.scanDirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			scanned++
			data, _ := os.ReadFile(path)
			if bytes.Contains(data, []byte(lockTestToken)) {
				t.Errorf("the instance token is on disk in %s", path)
			}
			if base := filepath.Base(path); base != "config.json" && base != ".security.yml" && bytes.Contains(data, []byte(cfgToken)) {
				t.Errorf("the config's dev-MCP token was written to %s", path)
			}
			return nil
		})
	}
	if scanned < 2 {
		t.Fatalf("only %d files scanned", scanned)
	}
}
