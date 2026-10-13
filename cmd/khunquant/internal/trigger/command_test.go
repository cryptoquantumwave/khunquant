package trigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

const cliToken = "cli-test-management-token-8841"

// fakeGateway is a gateway stand-in on a loopback port, with the pid file that
// points at it in a temporary home.
type fakeGateway struct {
	t      *testing.T
	srv    *httptest.Server
	home   string
	status int
	body   string
	header map[string]string

	mu   sync.Mutex
	got  []capturedRequest
	deps deps
}

type capturedRequest struct {
	method, path, auth, contentType string
	body                            map[string]any
	raw                             string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t, status: http.StatusAccepted, home: t.TempDir()}
	g.body = `{"event_id":"01HZZZZZZZZZZZZZZZZZZZZZZZ","status":"accepted","severity":"notice","mode":"live"}`
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		g.got = append(g.got, capturedRequest{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: body, raw: string(raw),
		})
		status, resp, hdr := g.status, g.body, g.header
		g.mu.Unlock()
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(g.srv.Close)

	port := g.srv.Listener.Addr().(*net.TCPAddr).Port
	// A real pid file, written by the real writer: this process is the "gateway".
	_, err := pid.WritePidFile(g.home, "127.0.0.1", port)
	require.NoError(t, err)
	t.Cleanup(func() { pid.RemovePidFile(g.home) })

	cfg := config.DefaultConfig()
	cfg.Gateway.ManagementToken = cliToken
	g.deps = deps{
		home:    func() string { return g.home },
		load:    func() (*config.Config, error) { return cfg, nil },
		readPid: pid.ReadPidFileWithCheck,
		client:  defaultDeps().client,
	}
	return g
}

func (g *fakeGateway) requests() []capturedRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]capturedRequest(nil), g.got...)
}

// exec runs the command with args and stdin, and returns what it printed and the
// exit code it asks for (0 on success).
func exec(t *testing.T, d deps, stdin string, args ...string) (stdout, stderr string, code int, err error) {
	t.Helper()
	cmd := newTriggerCommand(d)
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.Execute()
	var ee *ExitError
	if errors.As(err, &ee) {
		code = ee.Code
	} else if err != nil {
		code = 1
	}
	return out.String(), errb.String(), code, err
}

func noToken(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		assert.NotContains(t, s, cliToken, "the token was printed")
	}
}

// TestTriggerCLI_ReadsPidFile: the command finds the gateway through the pid file,
// sends the management token as a bearer and the request as JSON, and exits 0.
func TestTriggerCLI_ReadsPidFile(t *testing.T) {
	g := newFakeGateway(t)
	out, errOut, code, err := exec(t, g.deps, "", "backup-done", "NAS backup finished with 2 errors",
		"--severity", "Warning", "--data", ` { "errors": 2, "files": ["a", "b"] } `)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	reqs := g.requests()
	require.Len(t, reqs, 1)
	r := reqs[0]
	assert.Equal(t, "POST", r.method)
	assert.Equal(t, "/api/proactive/trigger", r.path)
	assert.Equal(t, "Bearer "+cliToken, r.auth)
	assert.Equal(t, "application/json", r.contentType)
	assert.Equal(t, "backup-done", r.body["name"])
	assert.Equal(t, "NAS backup finished with 2 errors", r.body["message"])
	assert.Equal(t, "warning", r.body["severity"])
	assert.Equal(t, map[string]any{"errors": float64(2), "files": []any{"a", "b"}}, r.body["data"])
	assert.Contains(t, r.raw, `"errors":2`, "the data is sent compact")

	assert.Contains(t, out, "Accepted for consideration")
	assert.Contains(t, out, "01HZZZZZZZZZZZZZZZZZZZZZZZ")
	assert.Contains(t, out, "not a delivery")
	noToken(t, out, errOut)
}

func TestTriggerCLI_NameOnlyAndShadowNote(t *testing.T) {
	g := newFakeGateway(t)
	g.body = `{"event_id":"01H","status":"accepted","severity":"notice","mode":"shadow"}`
	out, _, code, err := exec(t, g.deps, "", "ping")
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	r := g.requests()[0]
	assert.Equal(t, map[string]any{"name": "ping"}, r.body, "nothing but the name is sent")
	assert.Contains(t, out, "shadow mode")
}

func TestTriggerCLI_Stdin(t *testing.T) {
	g := newFakeGateway(t)
	_, _, code, err := exec(t, g.deps, "disk at 95%\nmount /var\n\n", "disk-full", "--stdin")
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "disk at 95%\nmount /var", g.requests()[0].body["message"], "trailing line breaks are trimmed")

	// The cap is the message cap, in characters.
	_, _, code, err = exec(t, g.deps, strings.Repeat("é", 2000), "disk-full", "--stdin")
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	for name, in := range map[string]string{
		"over by one":         strings.Repeat("x", 2001),
		"huge":                strings.Repeat("x", 1<<20),
		"over in wide runes":  strings.Repeat("\U0001F600", 2001),
		"a control character": "bad \x1b[31m escape",
	} {
		before := len(g.requests())
		_, _, code, err = exec(t, g.deps, in, "disk-full", "--stdin")
		require.Error(t, err, name)
		assert.Equal(t, ExitUsage, code, name)
		assert.Len(t, g.requests(), before, "%s: nothing was sent", name)
	}

	// Both ways of giving a message is a usage error.
	_, _, code, err = exec(t, g.deps, "x", "disk-full", "message", "--stdin")
	require.Error(t, err)
	assert.Equal(t, ExitUsage, code)
}

// The joiners emoji and several scripts need are not refused, and a bidirectional
// control is refused on standard input as well.
func TestTriggerCLI_BidiControlsAndJoiners(t *testing.T) {
	g := newFakeGateway(t)
	_, _, code, err := exec(t, g.deps, "", "a", "\U0001F468\u200d\U0001F469 and a\u200cb")
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	before := len(g.requests())
	_, _, code, err = exec(t, g.deps, "pay \u202e100\n", "a", "--stdin")
	require.Error(t, err)
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, err.Error(), "bidirectional")
	assert.Len(t, g.requests(), before)
}

func TestTriggerCLI_UsageErrorsExit2AndSendNothing(t *testing.T) {
	g := newFakeGateway(t)
	cases := map[string][]string{
		"no arguments":      {},
		"three arguments":   {"a", "b", "c"},
		"bad name":          {"Backup Done"},
		"name with colon":   {"a:b"},
		"name too long":     {strings.Repeat("a", 42)},
		"message too long":  {"a", strings.Repeat("x", 2001)},
		"bad severity":      {"a", "m", "--severity", "critical"},
		"unknown severity":  {"a", "m", "--severity", "loud"},
		"data not JSON":     {"a", "m", "--data", "{oops"},
		"data an array":     {"a", "m", "--data", "[1]"},
		"data too big":      {"a", "m", "--data", `{"k":"` + strings.Repeat("x", 4100) + `"}`},
		"unknown flag":      {"a", "m", "--nope"},
		"control character": {"a", "bad \x00 nul"},
		"bidi override":     {"a", "pay \u202e100"},
		"bidi isolate":      {"a", "x\u2066y"},
		"left-to-right mk":  {"a", "x\u200ey"},
		"right-to-left mk":  {"a", "x\u200fy"},
		"arabic letter mk":  {"a", "x\u061cy"},
	}
	for name, args := range cases {
		before := len(g.requests())
		out, errOut, code, err := exec(t, g.deps, "", args...)
		require.Error(t, err, name)
		assert.Equal(t, ExitUsage, code, name)
		assert.NotEmpty(t, err.Error(), name)
		assert.Len(t, g.requests(), before, "%s: something was sent", name)
		noToken(t, out, errOut, err.Error())
	}
}

func TestTriggerCLI_GatewayNotRunningExits3(t *testing.T) {
	g := newFakeGateway(t)

	t.Run("no pid file", func(t *testing.T) {
		d := g.deps
		d.readPid = func(string) *pid.PidFileData { return nil }
		_, _, code, err := exec(t, d, "", "a", "m")
		require.Error(t, err)
		assert.Equal(t, ExitNoGate, code)
		assert.Contains(t, err.Error(), "not running")
		noToken(t, err.Error())
	})
	t.Run("a pid file for a gateway that cannot be reached", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		d := g.deps
		d.readPid = func(string) *pid.PidFileData { return &pid.PidFileData{PID: 1, Host: "127.0.0.1", Port: port} }
		out, errOut, code, err := exec(t, d, "", "a", "m")
		require.Error(t, err)
		assert.Equal(t, ExitNoGate, code)
		noToken(t, out, errOut, err.Error())
	})
	t.Run("a gateway on a public address: the token is not sent", func(t *testing.T) {
		d := g.deps
		before := len(g.requests())
		d.readPid = func(string) *pid.PidFileData { return &pid.PidFileData{PID: 1, Host: "203.0.113.7", Port: 8080} }
		_, _, code, err := exec(t, d, "", "a", "m")
		require.Error(t, err)
		assert.Equal(t, ExitNoGate, code)
		assert.Contains(t, err.Error(), "loopback")
		assert.Len(t, g.requests(), before)
	})
	t.Run("an unspecified bind address means loopback", func(t *testing.T) {
		d := g.deps
		port := g.srv.Listener.Addr().(*net.TCPAddr).Port
		for _, h := range []string{"", "0.0.0.0", "::", "localhost"} {
			d.readPid = func(string) *pid.PidFileData { return &pid.PidFileData{PID: 1, Host: h, Port: port} }
			_, _, code, err := exec(t, d, "", "a", "m")
			if h == "::" || h == "localhost" {
				// "::1" and "localhost" may not resolve to the stub's IPv4 listener; a
				// refusal to send is not what is being tested, so only a 0 or a 3 is fine.
				if code != 0 && code != ExitNoGate {
					t.Errorf("host %q: exit %d (%v)", h, code, err)
				}
				continue
			}
			require.NoError(t, err, "host %q", h)
		}
	})
}

func TestTriggerCLI_RefusedByTheGatewayExits4(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   string
	}{
		{"unauthorized", 401, "unauthorized", nil, "management token"},
		{"forbidden", 403, `{"error":"access denied"}`, nil, "management token"},
		{"perception off: no route", 404, "404 page not found", nil, "perception is not enabled"},
		{"triggers off", 409, `{"error":"triggers are off (proactive.triggers.mode); nothing would act on a trigger"}`, nil, "triggers are off"},
		{"rate limited", 429, `{"error":"too many trigger requests; slow down"}`, map[string]string{"Retry-After": "2"}, "try again in 2 seconds"},
		{"busy", 503, `{"error":"the pipeline is busy; try again shortly"}`, nil, "pipeline is busy"},
		{"server error", 500, `oops`, nil, "status 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGateway(t)
			g.status, g.body, g.header = tc.status, tc.body, tc.header
			out, errOut, code, err := exec(t, g.deps, "", "a", "m")
			require.Error(t, err)
			assert.Equal(t, ExitRefused, code)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, out, "Accepted")
			noToken(t, out, errOut, err.Error())
		})
	}
}

func TestTriggerCLI_GatewayRefusingTheContentExits2(t *testing.T) {
	for _, status := range []int{400, 413} {
		g := newFakeGateway(t)
		g.status = status
		g.body = `{"error":"name must match something"}`
		_, _, code, err := exec(t, g.deps, "", "a", "m")
		require.Error(t, err)
		assert.Equal(t, ExitUsage, code, "status %d", status)
		assert.Contains(t, err.Error(), "name must match something")
	}
}

// Nothing the gateway says can make the command print the token or a control
// character, and a redirect is never followed.
func TestTriggerCLI_NeverPrintsTheTokenOrControlCharacters(t *testing.T) {
	g := newFakeGateway(t)
	g.status = 409
	g.body = `{"error":"your token is ` + cliToken + ` \u001b[31m and \u0007 bell"}`
	out, errOut, code, err := exec(t, g.deps, "", "a", "m")
	require.Error(t, err)
	assert.Equal(t, ExitRefused, code)
	noToken(t, out, errOut, err.Error())
	assert.NotContains(t, err.Error(), "\x1b")
	assert.NotContains(t, err.Error(), "\a")

	// Redirects are not followed.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a redirect was followed with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	g.status, g.body, g.header = 307, "", map[string]string{"Location": other.URL + "/steal"}
	_, _, code, err = exec(t, g.deps, "", "a", "m")
	require.Error(t, err)
	assert.Equal(t, ExitRefused, code)

	// Not even in the long help.
	cmd := newTriggerCommand(g.deps)
	var help bytes.Buffer
	cmd.SetOut(&help)
	require.NoError(t, cmd.Help())
	noToken(t, help.String())
}

func TestTriggerCLI_NoTokenInTheConfig(t *testing.T) {
	g := newFakeGateway(t)
	cfg := config.DefaultConfig()
	cfg.Gateway.ManagementToken = ""
	g.deps.load = func() (*config.Config, error) { return cfg, nil }
	_, _, code, err := exec(t, g.deps, "", "a", "m")
	require.Error(t, err)
	assert.Equal(t, ExitRefused, code)
	assert.Empty(t, g.requests(), "a request went out without a token")

	g.deps.load = func() (*config.Config, error) { return nil, errors.New("broken config") }
	_, _, code, err = exec(t, g.deps, "", "a", "m")
	require.Error(t, err)
	assert.Equal(t, 1, code)
	noToken(t, err.Error())
}

func TestExitError(t *testing.T) {
	var err error = &ExitError{Code: 3, Msg: "m"}
	var ee interface{ ExitCode() int }
	require.True(t, errors.As(err, &ee))
	assert.Equal(t, 3, ee.ExitCode())
	assert.Equal(t, "m", err.Error())
}
