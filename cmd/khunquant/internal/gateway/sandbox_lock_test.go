package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func TestNewGatewayCommand_HasSandboxLockedFlag(t *testing.T) {
	f := NewGatewayCommand().Flags().Lookup("sandbox-locked")
	if f == nil || f.DefValue != "false" {
		t.Fatalf("--sandbox-locked flag = %+v, want a bool defaulting to false", f)
	}
}

func TestLockFatalError_ExitCode3(t *testing.T) {
	var err error = lockFatal(errors.New("boom"))
	var ee interface{ ExitCode() int }
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("lockFatal must carry exit code 3, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error text lost the cause: %q", err)
	}
}

func TestLockOptionsFromEnv(t *testing.T) {
	home := newInstanceHome(t)
	setLockEnv(t, home)

	lk, err := lockOptionsFromEnv()
	if err != nil {
		t.Fatalf("valid environment refused: %v", err)
	}
	if lk.Token != lockTestToken || lk.InstanceName != "test" || len(lk.AllowHosts) != 0 {
		t.Fatalf("options = %+v", lk)
	}

	t.Setenv("KQ_SANDBOX_ALLOW_HOSTS", " a.example , ,b.example")
	if lk, _ = lockOptionsFromEnv(); len(lk.AllowHosts) != 2 || lk.AllowHosts[0] != "a.example" || lk.AllowHosts[1] != "b.example" {
		t.Fatalf("allow hosts = %v", lk.AllowHosts)
	}
	t.Setenv("KQ_SANDBOX_ALLOW_HOSTS", "")

	// newInstanceHome pointed HOME at a temp dir, so these "real" homes are temp dirs
	// too. They carry the marker, so only the containment rule can refuse them.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realHome, ".khunquant"), 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(realHome)
	for _, d := range []string{parent, realHome, filepath.Join(realHome, ".khunquant")} {
		if err := os.WriteFile(filepath.Join(d, instanceMarker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		set  map[string]string
	}{
		{"no token", map[string]string{"KQ_SANDBOX_TOKEN": ""}},
		{"short token", map[string]string{"KQ_SANDBOX_TOKEN": strings.Repeat("a", 31)}},
		{"no home", map[string]string{"KHUNQUANT_HOME": ""}},
		{"real home", map[string]string{
			"KHUNQUANT_HOME":   filepath.Join(realHome, ".khunquant"),
			"KHUNQUANT_CONFIG": filepath.Join(realHome, ".khunquant", "config.json"),
		}},
		{"home that contains the real home", map[string]string{
			"KHUNQUANT_HOME":   realHome,
			"KHUNQUANT_CONFIG": filepath.Join(realHome, "config.json"),
		}},
		{"parent of the real home", map[string]string{
			"KHUNQUANT_HOME":   parent,
			"KHUNQUANT_CONFIG": filepath.Join(parent, "config.json"),
		}},
		{"no marker", map[string]string{
			"KHUNQUANT_HOME":   t.TempDir(),
			"KHUNQUANT_CONFIG": "",
		}},
		{"config outside home", map[string]string{"KHUNQUANT_CONFIG": filepath.Join(t.TempDir(), "config.json")}},
	}
	for _, tc := range cases {
		for k, v := range tc.set {
			t.Setenv(k, v)
		}
		if tc.name == "no marker" {
			t.Setenv("KHUNQUANT_CONFIG", filepath.Join(os.Getenv("KHUNQUANT_HOME"), "config.json"))
		}
		_, err := lockOptionsFromEnv()
		if err == nil {
			t.Errorf("%s: lockOptionsFromEnv succeeded, want an error", tc.name)
		} else if strings.Contains(err.Error(), lockTestToken) {
			t.Errorf("%s: the error leaks the token: %v", tc.name, err)
		}
		setLockEnv(t, home) // restore the valid environment
	}
}

// `khunquant sandbox up` runs the gateway with HOME set to the instance home. The
// overlap check against ~/.khunquant must not refuse that (it would make every
// instance unstartable), yet must keep refusing a home that really overlaps it.
func TestLockOptionsFromEnv_HomeIsInstanceHome(t *testing.T) {
	home := newInstanceHome(t)
	setLockEnv(t, home)
	t.Setenv("HOME", home)
	if _, err := lockOptionsFromEnv(); err != nil {
		t.Fatalf("HOME=<instance home> refused: %v", err)
	}

	// HOME elsewhere and the instance home inside <HOME>/.khunquant: still refused.
	other := t.TempDir()
	t.Setenv("HOME", other)
	inside := filepath.Join(other, ".khunquant", "x")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, instanceMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KHUNQUANT_HOME", inside)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(inside, "config.json"))
	if _, err := lockOptionsFromEnv(); err == nil {
		t.Fatal("a home inside ~/.khunquant was accepted")
	}
}

// The lock is decided before the config is read: a locked start without a token
// fails on the token, not on the (unreadable) config.
func TestGatewayCmd_LockedChecksEnvBeforeReadingConfig(t *testing.T) {
	home := newInstanceHome(t)
	setLockEnv(t, home)
	t.Setenv("KQ_SANDBOX_TOKEN", "")
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)

	err := gatewayCmd(false, true)
	if err == nil || !strings.Contains(err.Error(), "KQ_SANDBOX_TOKEN") {
		t.Fatalf("gatewayCmd error = %v, want the token error", err)
	}
	if sandbox.Locked() {
		t.Fatal("a refused start must not leave the process locked")
	}

	// The env switch locks the same way as the flag.
	t.Setenv("KQ_SANDBOX_LOCKED", "1")
	if err := gatewayCmd(false, false); err == nil || !strings.Contains(err.Error(), "KQ_SANDBOX_TOKEN") {
		t.Fatalf("KQ_SANDBOX_LOCKED=1: gatewayCmd error = %v, want the token error", err)
	}
}

func TestEnforceLockedConfig(t *testing.T) {
	home := newInstanceHome(t)
	lk := lockProcessForTest(t, home)
	const proxy = "socks5://127.0.0.1:1080"

	dirty := func() *config.Config {
		cfg := config.DefaultConfig()
		cfg.Agents.Defaults.Workspace = filepath.Join(home, "workspace")
		cfg.ModelList = []config.ModelConfig{
			{ModelName: "sandbox", Model: "openai/sandbox-offline", APIBase: "http://127.0.0.1:9/v1", Proxy: proxy},
			{ModelName: "local", Model: "ollama/llama3", Proxy: proxy},
		}
		cfg.Debug.Sandbox.Enabled = false
		cfg.Debug.Sandbox.Token = lockTestToken // what env.Parse copies in from KQ_SANDBOX_TOKEN
		cfg.Gateway.ManagementToken = "other"
		cfg.Tools.Exec.Enabled = true
		cfg.Tools.MCP.Enabled = true
		cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{"x": {Enabled: true, Command: "npx"}}
		on := true
		cfg.Proactive.Owner.WebIsOwner = &on
		cfg.Tools.Web.Proxy = proxy
		cfg.Tools.Skills.Github.Proxy = proxy
		cfg.Providers.OpenAI.Proxy = proxy

		acct := config.ExchangeAccount{Name: "a", Proxy: proxy}
		cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{acct, acct}
		cfg.Exchanges.BinanceTH.Accounts = []config.ExchangeAccount{acct}
		cfg.Exchanges.Bitkub.Accounts = []config.ExchangeAccount{acct}
		cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{{ExchangeAccount: acct}}
		cfg.Exchanges.Settrade.Accounts = []config.SettradeExchangeAccount{{ExchangeAccount: acct}}
		cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{{ExchangeAccount: acct}}
		return cfg
	}

	t.Run("forces every field", func(t *testing.T) {
		cfg := dirty()
		if err := enforceLockedConfig(cfg, lk); err != nil {
			t.Fatalf("enforceLockedConfig: %v", err)
		}
		if !cfg.Debug.Sandbox.Enabled {
			t.Error("debug.sandbox.enabled not forced on")
		}
		if cfg.Debug.Sandbox.Token != "" {
			t.Error("debug.sandbox.token not cleared")
		}
		if cfg.Gateway.ManagementToken != lockTestToken {
			t.Error("gateway.management_token is not the instance token")
		}
		if cfg.Tools.Exec.Enabled {
			t.Error("tools.exec.enabled not forced off")
		}
		if cfg.Tools.MCP.Enabled || cfg.Tools.MCP.Servers != nil {
			t.Error("tools.mcp not forced off and emptied")
		}
		if cfg.Proactive.Owner.WebIsOwnerEnabled() {
			t.Error("proactive.owner.web_is_owner not forced off")
		}
		if got, want := cfg.Debug.Sandbox.FixturesDir, filepath.Join(home, "workspace", "sandbox"); got != want {
			t.Errorf("fixtures_dir = %q, want %q", got, want)
		}

		proxies := map[string]string{
			"model_list[0]":    cfg.ModelList[0].Proxy,
			"model_list[1]":    cfg.ModelList[1].Proxy,
			"tools.web":        cfg.Tools.Web.Proxy,
			"skills.github":    cfg.Tools.Skills.Github.Proxy,
			"providers.openai": cfg.Providers.OpenAI.Proxy,
		}
		proxies["binance[0]"] = cfg.Exchanges.Binance.Accounts[0].Proxy
		proxies["binance[1]"] = cfg.Exchanges.Binance.Accounts[1].Proxy
		proxies["binanceth"] = cfg.Exchanges.BinanceTH.Accounts[0].Proxy
		proxies["bitkub"] = cfg.Exchanges.Bitkub.Accounts[0].Proxy
		proxies["okx"] = cfg.Exchanges.OKX.Accounts[0].Proxy
		proxies["settrade"] = cfg.Exchanges.Settrade.Accounts[0].Proxy
		proxies["webull"] = cfg.Exchanges.Webull.Accounts[0].Proxy
		for name, p := range proxies {
			if p != "" {
				t.Errorf("%s proxy survived: %q", name, p)
			}
		}
	})

	t.Run("explicit fixtures dir inside home is kept", func(t *testing.T) {
		cfg := dirty()
		cfg.Debug.Sandbox.FixturesDir = filepath.Join(home, "workspace", "sandbox")
		if err := enforceLockedConfig(cfg, lk); err != nil {
			t.Fatal(err)
		}
	})

	for name, mutate := range map[string]func(*config.Config){
		"workspace outside home": func(c *config.Config) { c.Agents.Defaults.Workspace = t.TempDir() },
		"fixtures outside home":  func(c *config.Config) { c.Debug.Sandbox.FixturesDir = repoSandboxFixtures(t) },
		"claude-cli model": func(c *config.Config) {
			c.ModelList = append(c.ModelList, config.ModelConfig{ModelName: "cli", Model: "claude-cli/x"})
		},
		"codex-cli model": func(c *config.Config) {
			c.ModelList = append(c.ModelList, config.ModelConfig{ModelName: "cli", Model: "codexcli/x"})
		},
		"real api host": func(c *config.Config) {
			c.ModelList = append(c.ModelList, config.ModelConfig{ModelName: "real", Model: "openai/gpt-4o"})
		},
		"oauth on a loopback base": func(c *config.Config) {
			c.ModelList = append(c.ModelList, config.ModelConfig{
				ModelName: "o", Model: "openai/x", APIBase: "http://127.0.0.1:9/v1", AuthMethod: "oauth"})
		},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			cfg := dirty()
			mutate(cfg)
			if err := enforceLockedConfig(cfg, lk); err == nil {
				t.Fatal("enforceLockedConfig succeeded, want an error")
			}
		})
	}
}

// lockedFixture is the assembly fixture of a locked instance: the real lab, the real
// reload path, a stub simchat as the one channel.
type lockedFixture struct {
	*assemblyFixture
	lab *lab.Lab
	// nilProvider makes the next createProvider return no provider and no error, so
	// the agent loop's ReloadProviderAndConfig is the step that fails.
	nilProvider bool
}

// newLockedFixture starts a locked gateway; mutate, if any, changes the first
// generation's config (on top of lockedBaseConfig).
func newLockedFixture(t *testing.T, mutate ...func(*config.Config)) *lockedFixture {
	t.Helper()
	home := newInstanceHome(t)
	lk := lockProcessForTest(t, home)

	f := &assemblyFixture{
		t:         t,
		workspace: filepath.Join(home, "workspace"),
		port:      freePort(t),
		provider:  noopProvider{},
	}
	lf := &lockedFixture{assemblyFixture: f}

	prevCreate := createProvider
	createProvider = func(*config.Config) (providers.LLMProvider, string, error) {
		if f.providerErr != nil {
			return nil, "", f.providerErr
		}
		if lf.nilProvider {
			return nil, "", nil
		}
		return noopProvider{}, "", nil
	}
	t.Cleanup(func() { createProvider = prevCreate })
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	f.bus = bus.NewMessageBus()
	f.initial = f.config(func(c *config.Config) {
		lockedBaseConfig(c)
		for _, m := range mutate {
			m(c)
		}
	})
	if err := enforceLockedConfig(f.initial, lk); err != nil {
		t.Fatalf("enforceLockedConfig: %v", err)
	}
	f.al = agent.NewAgentLoop(f.initial, f.bus, f.provider)
	f.shared = newSharedResources()

	l, err := buildLab(f.initial, lk)
	if err != nil {
		t.Fatalf("buildLab: %v", err)
	}
	lf.lab = l
	f.shared.lab = l
	f.shared.add("sandbox lab", l)
	if err := registerLabGroups(l, f.shared, f.initial); err != nil { // as gatewayCmd does, before the first build
		t.Fatalf("registerLabGroups: %v", err)
	}

	t.Cleanup(func() {
		if f.services != nil {
			stopServices(f.services, 5*time.Second)
		}
		f.al.Close()
		f.bus.Close()
		_ = f.shared.Close()
	})
	f.build(f.initial)
	return lf
}

// lockedBaseConfig gives a config the one model the lock allows (the defaults point
// at real hosts, which enforceLockedConfig refuses).
func lockedBaseConfig(c *config.Config) {
	c.ModelList = []config.ModelConfig{{
		ModelName: "sandbox", Model: "openai/sandbox-offline",
		APIBase: "http://127.0.0.1:9/v1", APIKey: *config.NewSecureString("sandbox"),
	}}
	c.Agents.Defaults.ModelName = "sandbox"
	c.Channels.SimChat.Enabled, c.Channels.SimChat.AllowFrom = true, config.FlexibleStringSlice{"*"}
}

// reload drives the production reload path with a config that is valid under the
// lock apart from what mutate changes.
func (f *lockedFixture) reload(mutate func(*config.Config)) error {
	f.t.Helper()
	return f.assemblyFixture.reload(func(c *config.Config) {
		lockedBaseConfig(c)
		if mutate != nil {
			mutate(c)
		}
	})
}

// placeBinanceOrder places a market order on the lab's simulator the way an exchange
// client would (through the simulator's request handler) and returns its id.
func placeBinanceOrder(t *testing.T, l *lab.Lab) string {
	t.Helper()
	state := l.State.GetState("binance")
	var symbol string
	for s := range state.Markets {
		symbol = s
		break
	}
	if symbol == "" {
		t.Fatal("the binance fixtures seeded no market")
	}
	form := url.Values{"symbol": {symbol}, "side": {"BUY"}, "type": {"market"}, "quantity": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/fapi/v1/order", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, ok := l.Sim.Respond("binance", http.MethodPost, "/fapi/v1/order", req)
	if !ok || resp.Status != http.StatusOK {
		t.Fatalf("simulator refused the order: ok=%v resp=%+v", ok, resp)
	}
	for id := range l.State.GetState("binance").ClosedOrders {
		return id
	}
	t.Fatal("the order did not reach ClosedOrders")
	return ""
}

func hasClosedOrder(l *lab.Lab, id string) bool {
	_, ok := l.State.GetState("binance").ClosedOrders[id]
	return ok
}

type sandboxStatus struct {
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Locked  bool   `json:"locked"`
	BaseURL string `json:"base_url"`
}

func (f *lockedFixture) status() sandboxStatus {
	f.t.Helper()
	body := f.expectStatus("/api/sandbox/status", lockTestToken, http.StatusOK)
	var s sandboxStatus
	if err := json.Unmarshal(body, &s); err != nil {
		f.t.Fatalf("decode status: %v (%s)", err, body)
	}
	return s
}

// assertLabSurvived is what every locked reload path must leave behind: the same
// server on the same port, the order placed before the reload, the routes served by
// the new generation, and the sandbox still armed.
func (f *lockedFixture) assertLabSurvived(base, orderID string) {
	f.t.Helper()
	if !f.lab.Server.IsRunning() || f.lab.Server.BaseURL() != base {
		f.t.Fatalf("lab server changed: running=%v base=%q want %q", f.lab.Server.IsRunning(), f.lab.Server.BaseURL(), base)
	}
	if f.services.SandboxServer != f.lab.Server {
		f.t.Fatal("the new generation does not serve the lab's server")
	}
	if f.services.lab != f.lab {
		f.t.Fatal("the new generation lost the lab")
	}
	if !hasClosedOrder(f.lab, orderID) {
		f.t.Fatalf("order %s placed before the reload is gone from ClosedOrders", orderID)
	}
	st := f.status()
	if !st.Enabled || !st.Locked || !st.Running || st.BaseURL != base {
		f.t.Fatalf("status after reload = %+v, want enabled+locked+running at %s", st, base)
	}
	if en, b := sandbox.GlobalState(); !en || b != base {
		f.t.Fatalf("GlobalState = (%v,%q), want (true,%q)", en, b, base)
	}
	// The instance token is the one bearer; the config's own tokens are not.
	f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusUnauthorized)
	f.expectStatus("/api/cron/jobs", lockTestToken, http.StatusOK)
}

func TestLockedReload_Success_SameServer(t *testing.T) {
	f := newLockedFixture(t)
	base := f.lab.Server.BaseURL()
	orderID := placeBinanceOrder(t, f.lab)

	st := f.status()
	if !st.Enabled || !st.Locked || !st.Running || st.BaseURL != base {
		t.Fatalf("status at start = %+v", st)
	}
	firstCron := f.services.CronService

	// The reloaded config tries to switch the sandbox off and bring its own tokens.
	err := f.reload(func(c *config.Config) {
		c.Debug.Sandbox.Enabled = false
		c.Debug.Sandbox.Token = "attacker-token"
		c.Gateway.ManagementToken = "attacker-token"
	})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.services.CronService == firstCron {
		t.Fatal("the reload did not rebuild the services")
	}
	f.assertLabSurvived(base, orderID)
	f.expectStatus("/api/sandbox/status", "attacker-token", http.StatusUnauthorized)
}

func TestLockedReload_ProviderFails_KeepsLab(t *testing.T) {
	f := newLockedFixture(t)
	base := f.lab.Server.BaseURL()
	orderID := placeBinanceOrder(t, f.lab)

	f.providerErr = errors.New("bad provider config")
	if err := f.reload(nil); err == nil {
		t.Fatal("reload with a failing provider must report an error")
	}
	f.assertLabSurvived(base, orderID)
}

func TestLockedReload_AgentReloadFails_KeepsLab(t *testing.T) {
	f := newLockedFixture(t)
	base := f.lab.Server.BaseURL()
	orderID := placeBinanceOrder(t, f.lab)

	f.nilProvider = true
	if err := f.reload(nil); err == nil {
		t.Fatal("reload whose agent loop refuses the provider must report an error")
	}
	f.assertLabSurvived(base, orderID)
}

// A reloaded config that breaks a locked invariant is refused before anything stops.
func TestLockedReload_RefusedConfigStopsNothing(t *testing.T) {
	f := newLockedFixture(t)
	base := f.lab.Server.BaseURL()
	orderID := placeBinanceOrder(t, f.lab)
	firstCron := f.services.CronService

	err := f.reload(func(c *config.Config) {
		c.ModelList = []config.ModelConfig{{ModelName: "sandbox", Model: "claude-cli/x"}}
		c.Agents.Defaults.ModelName = "sandbox"
	})
	if err == nil {
		t.Fatal("a claude-cli model must be refused")
	}
	if f.services.CronService != firstCron {
		t.Fatal("a refused config must not stop or rebuild the services")
	}
	f.assertLabSurvived(base, orderID)
}

func TestLockedConfigIsNeverWritten(t *testing.T) {
	f := newLockedFixture(t)
	path := filepath.Join(os.Getenv("KHUNQUANT_HOME"), "config.json")
	if err := config.SaveConfig(path, f.initial); err == nil {
		t.Fatal("SaveConfig succeeded under the sandbox lock")
	}
	for _, name := range []string{"config.json", ".security.yml"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err == nil {
			t.Errorf("%s exists after a refused save", name)
		}
	}
}

func TestUnlockedGateway_NoLabNoChange(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)

	if sandbox.Locked() {
		t.Fatal("the process must not be locked without the flag")
	}
	if f.shared.lab != nil || f.services.lab != nil {
		t.Fatal("an unlocked gateway built a lab")
	}
	if f.services.SandboxServer == nil || !f.services.SandboxServer.IsRunning() {
		t.Fatal("the unlocked sandbox must still start as before")
	}

	body := f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusOK)
	var st sandboxStatus
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || !st.Running || st.Locked {
		t.Fatalf("unlocked status = %+v, want enabled+running and not locked", st)
	}

	// The save guard is the lock's; unlocked, SaveConfig is untouched.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, config.DefaultConfig()); err != nil {
		t.Fatalf("unlocked SaveConfig refused: %v", err)
	}

	// Unlocked reload still switches the sandbox off, as it always did.
	if err := f.reload(func(c *config.Config) { c.Debug.Sandbox.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	if f.services.SandboxServer != nil {
		t.Fatal("an unlocked reload with the sandbox disabled must stop it")
	}
	f.expectStatus("/api/sandbox/status", testSandboxToken, http.StatusNotFound)
}
