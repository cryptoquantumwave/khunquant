package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

const (
	// lockEnv is the env switch that locks the process, the same as --sandbox-locked.
	lockEnv = "KQ_SANDBOX_LOCKED"
	// instanceMarker is the file whose presence in KHUNQUANT_HOME says that the home
	// was made for a sandbox instance. Whoever creates the home writes it.
	instanceMarker = ".kq-sandbox"
	// minLockTokenLen is the shortest instance token a locked gateway accepts.
	minLockTokenLen = 32
	// lockExitCode is the process exit status for a violated locked invariant.
	lockExitCode = 3
)

// lockFatalError ends a locked gateway: a locked invariant was violated or a locked
// (re)start failed. main exits with ExitCode (3), which the supervisor reads as
// "lock invariant", not as a crash.
type lockFatalError struct{ err error }

func (e *lockFatalError) Error() string { return "sandbox lock: " + e.err.Error() }
func (e *lockFatalError) Unwrap() error { return e.err }
func (e *lockFatalError) ExitCode() int { return lockExitCode }

func lockFatal(err error) error { return &lockFatalError{err: err} }

// lockRequested says whether this process was asked to lock: the flag or the env.
func lockRequested(flag bool) bool {
	return flag || os.Getenv(lockEnv) == "1"
}

// lockOptionsFromEnv reads and checks everything the lock needs from the process
// environment. It runs before the config is read and refuses anything that could let
// a locked gateway run on a real home. Errors never contain the token.
func lockOptionsFromEnv() (sandbox.LockOptions, error) {
	token := os.Getenv("KQ_SANDBOX_TOKEN")
	if len(token) < minLockTokenLen {
		return sandbox.LockOptions{}, fmt.Errorf(
			"sandbox lock: KQ_SANDBOX_TOKEN must be set to at least %d characters", minLockTokenLen)
	}

	home := os.Getenv("KHUNQUANT_HOME")
	if home == "" {
		return sandbox.LockOptions{}, errors.New("sandbox lock: KHUNQUANT_HOME must name the instance home")
	}
	// `sandbox up` runs the gateway with HOME=<instance home>, which has no .khunquant
	// of its own: there ~ is the instance, not an install, and the check is skipped.
	if userHome, err := os.UserHomeDir(); err == nil {
		realHome := filepath.Join(userHome, ".khunquant")
		_, statErr := os.Lstat(realHome)
		upHome := os.IsNotExist(statErr) && sandbox.PathInside(userHome, home) && sandbox.PathInside(home, userHome)
		if !upHome && (sandbox.PathInside(realHome, home) || sandbox.PathInside(home, realHome)) {
			return sandbox.LockOptions{}, errors.New("sandbox lock: KHUNQUANT_HOME must not be, contain or lie inside ~/.khunquant")
		}
	}
	if info, err := os.Stat(filepath.Join(home, instanceMarker)); err != nil || !info.Mode().IsRegular() {
		return sandbox.LockOptions{}, fmt.Errorf("sandbox lock: %s is not a sandbox instance home (no %s marker)", home, instanceMarker)
	}
	if !sandbox.PathInside(home, internal.GetConfigPath()) {
		return sandbox.LockOptions{}, errors.New("sandbox lock: KHUNQUANT_CONFIG must be inside KHUNQUANT_HOME")
	}

	allow, err := lockAllowHosts(os.Getenv)
	if err != nil {
		return sandbox.LockOptions{}, err
	}
	return sandbox.LockOptions{
		InstanceName: os.Getenv("KQ_SANDBOX_INSTANCE"),
		Token:        token,
		AllowHosts:   allow,
	}, nil
}

// armSandboxLock locks the process (arming the sandbox state as enabled with no
// server, so exchange traffic fails closed), makes SaveConfig refuse every write and
// installs the loopback-only egress guard.
func armSandboxLock(lk sandbox.LockOptions) error {
	if err := sandbox.Lock(lk); err != nil {
		return err
	}
	config.SetSaveGuard(func(string) error {
		return errors.New("config is read-only under the sandbox lock")
	})
	return egress.Install(egress.Policy{AllowHosts: lk.AllowHosts})
}

// lockAllowHosts is the egress opening of the lock, and it is exact: no host unless the
// instance is record or live, and then only the host of the inner model's API base, which
// the supervisor computed with the same sandbox.LLMInnerHost. scripted and replay open
// nothing, whatever KQ_SANDBOX_ALLOW_HOSTS says.
func lockAllowHosts(getenv func(string) string) ([]string, error) {
	var allow []string
	for _, h := range strings.Split(getenv(sandbox.EnvAllowHosts), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
	}
	mode := getenv(sandbox.EnvLLM)
	if mode != sim.ModeLive && mode != sim.ModeRecord {
		if len(allow) > 0 {
			return nil, fmt.Errorf("sandbox lock: %s is allowed only for a record or live instance", sandbox.EnvAllowHosts)
		}
		return nil, nil
	}
	want, err := sandbox.LLMInnerHost(getenv(sandbox.EnvLLMModel), getenv(sandbox.EnvLLMAPIBase))
	if err != nil {
		return nil, fmt.Errorf("sandbox lock: %s=%s: %w", sandbox.EnvLLM, mode, err)
	}
	if len(allow) != 1 || !strings.EqualFold(allow[0], want) {
		return nil, fmt.Errorf("sandbox lock: %s must be exactly the inner model's API host %q", sandbox.EnvAllowHosts, want)
	}
	return allow, nil
}

// applyLLMMode points every sim model at the instance's mode (and inner model). The
// profile on disk says sim/scripted; the lock puts the mode in memory, like the placeholder
// accounts, on every load and reload. mode "" leaves the config.
func applyLLMMode(cfg *config.Config, mode, inner string) error {
	switch mode {
	case "":
		return nil
	case sim.ModeScripted, sim.ModeReplay:
		if inner != "" {
			return fmt.Errorf("%s=%q takes no inner model", sandbox.EnvLLM, mode)
		}
	case sim.ModeRecord, sim.ModeLive:
		if _, _, err := sandbox.SplitInnerModel(inner); err != nil {
			return fmt.Errorf("%s=%q: %w", sandbox.EnvLLM, mode, err)
		}
	default:
		return fmt.Errorf("%s=%q: not a mode (scripted, replay, record, live)", sandbox.EnvLLM, mode)
	}
	for i := range cfg.ModelList {
		if protocol, _ := providers.ExtractProtocol(cfg.ModelList[i].Model); protocol == "sim" {
			cfg.ModelList[i].Model = sim.Ref(mode, inner)
		}
	}
	return nil
}

// readPlaceholderVenues reads the venues that get a placeholder account; none when the
// home records none.
func readPlaceholderVenues(home string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(home, sandbox.AccountsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sandbox.AccountsFile, err)
	}
	var venues []string
	if err := json.Unmarshal(b, &venues); err != nil {
		return nil, fmt.Errorf("%s is not a JSON list of venues: %w", sandbox.AccountsFile, err)
	}
	return venues, nil
}

// enforceLockedConfig forces the invariants of a locked config. It runs on the
// config loaded at start and on every reloaded one, before anything is built from
// it. It returns an error for what it cannot fix: a workspace or fixtures directory
// outside the instance home, an agent workspace that is the home itself, or a model
// the lock does not allow.
func enforceLockedConfig(cfg *config.Config, lk sandbox.LockOptions) error {
	home := config.HomeDir()

	workspace := cfg.WorkspacePath()
	if !workspaceInHome(home, workspace) {
		return fmt.Errorf("agents.defaults.workspace %q is not a directory inside the instance home", workspace)
	}
	// Every agent's tools are built on its own workspace, and the default agent's is
	// where the shared proactive database opens. Each must be inside the home, or a
	// relaxed list would open the developer's real ~/.khunquant from a locked instance.
	for i := range cfg.Agents.List {
		ws := agent.AgentWorkspace(&cfg.Agents.List[i], &cfg.Agents.Defaults)
		if !workspaceInHome(home, ws) {
			return fmt.Errorf("agents.list[%d] (%q) workspace %q is not a directory inside the instance home", i, cfg.Agents.List[i].ID, ws)
		}
	}
	fixtures := cfg.Debug.Sandbox.FixturesDir
	if fixtures == "" {
		fixtures = filepath.Join(workspace, "sandbox")
	}
	if !sandbox.PathInside(home, fixtures) {
		return fmt.Errorf("debug.sandbox.fixtures_dir %q is outside the instance home", fixtures)
	}

	for i := range cfg.ModelList {
		m := &cfg.ModelList[i]
		protocol, _ := providers.ExtractProtocol(m.Model)
		if err := sandbox.CheckLockedModel(protocol); err != nil {
			return fmt.Errorf("model_list[%d] %q: %w", i, m.ModelName, err)
		}
		if m.AuthMethod != "" {
			return fmt.Errorf("model_list[%d] %q: auth_method %q is not allowed", i, m.ModelName, m.AuthMethod)
		}
		m.Proxy = ""
	}
	if err := applyLLMMode(cfg, os.Getenv(sandbox.EnvLLM), os.Getenv(sandbox.EnvLLMModel)); err != nil {
		return err
	}

	cfg.Debug.Sandbox.Enabled = true
	cfg.Debug.Sandbox.FixturesDir = fixtures
	cfg.Debug.Sandbox.Token = "" // env.Parse copies KQ_SANDBOX_TOKEN here; the lab holds it
	cfg.Gateway.ManagementToken = lk.Token

	cfg.Tools.Exec.Enabled = false // also stops cron command jobs
	cfg.Tools.MCP.Enabled = false
	cfg.Tools.MCP.Servers = nil
	// The file tools stay in their workspace whatever the instance config says. Without
	// this an overlay could let the agent read the real ~/.khunquant secrets or write the
	// config.json a production gateway on the same machine hot-reloads.
	cfg.Agents.Defaults.RestrictToWorkspace = true
	cfg.Agents.Defaults.AllowReadOutsideWorkspace = false
	cfg.Tools.AllowReadPaths = nil
	cfg.Tools.AllowWritePaths = nil
	// No hardware from a locked instance.
	cfg.Tools.I2C.Enabled = false
	cfg.Tools.SPI.Enabled = false
	cfg.Tools.Serial.Enabled = false
	cfg.Devices.Enabled = false
	// proactive.owner.web_is_owner is left as configured. The profile writes false; an
	// instance that sets true (to exercise owner-level sandbox_manage) opens nothing
	// new: pico and every other real channel are refused under the lock, so the only
	// web-owner surface is the management routes, which need the instance token.

	// Legacy providers are merged into model_list at provider creation, after this
	// runs, and carry their own proxy. Only model_list is allowed under the lock.
	cfg.Providers = config.ProvidersConfig{}

	// The placeholder accounts a scenario asked for. They are set here, in memory, on every
	// load and reload, and replace whatever the file says about those venues: the
	// credentials never reach disk (SaveConfig is refused) and a patch cannot widen their
	// permissions or swap the account. The list is in the home, which the instance's file
	// tools cannot write: they are confined (forced above) to workspaces that are
	// directories inside the home, never the home itself.
	venues, err := readPlaceholderVenues(home)
	if err != nil {
		return err
	}
	if err := sandbox.ApplyPlaceholderAccounts(cfg, venues); err != nil {
		return err
	}

	// web_search and web_fetch are SimWeb's: Brave with the placeholder key, no other
	// provider. In memory on every load and reload, like the accounts above.
	sandbox.ApplyWebConfig(cfg)

	// A loopback proxy such as socks5://127.0.0.1:1080 would still forward to the
	// internet, so no proxy survives. Accounts are edited by index: ranging by value
	// would change a copy.
	cfg.Tools.Web.Proxy = ""
	cfg.Tools.Skills.Github.Proxy = ""
	ex := &cfg.Exchanges
	for i := range ex.Binance.Accounts {
		ex.Binance.Accounts[i].Proxy = ""
	}
	for i := range ex.BinanceTH.Accounts {
		ex.BinanceTH.Accounts[i].Proxy = ""
	}
	for i := range ex.Bitkub.Accounts {
		ex.Bitkub.Accounts[i].Proxy = ""
	}
	for i := range ex.OKX.Accounts {
		ex.OKX.Accounts[i].Proxy = ""
	}
	for i := range ex.Settrade.Accounts {
		ex.Settrade.Accounts[i].Proxy = ""
	}
	for i := range ex.Webull.Accounts {
		ex.Webull.Accounts[i].Proxy = ""
	}
	return nil
}

// workspaceInHome reports whether ws is inside home and is not home itself: a workspace
// at the home would hold config.json, .security.yml and the accounts record.
func workspaceInHome(home, ws string) bool {
	return sandbox.PathInside(home, ws) && !sandbox.PathInside(ws, home)
}
