package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
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

	var allow []string
	for _, h := range strings.Split(os.Getenv("KQ_SANDBOX_ALLOW_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
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

// enforceLockedConfig forces the invariants of a locked config. It runs on the
// config loaded at start and on every reloaded one, before anything is built from
// it. It returns an error for what it cannot fix: a workspace or fixtures directory
// outside the instance home, or a model the lock does not allow.
func enforceLockedConfig(cfg *config.Config, lk sandbox.LockOptions) error {
	home := config.HomeDir()

	workspace := cfg.WorkspacePath()
	if !sandbox.PathInside(home, workspace) {
		return fmt.Errorf("agents.defaults.workspace %q is outside the instance home", workspace)
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

	cfg.Debug.Sandbox.Enabled = true
	cfg.Debug.Sandbox.FixturesDir = fixtures
	cfg.Debug.Sandbox.Token = "" // env.Parse copies KQ_SANDBOX_TOKEN here; the lab holds it
	cfg.Gateway.ManagementToken = lk.Token

	cfg.Tools.Exec.Enabled = false // also stops cron command jobs
	cfg.Tools.MCP.Enabled = false
	cfg.Tools.MCP.Servers = nil
	off := false
	cfg.Proactive.Owner.WebIsOwner = &off

	// Legacy providers are merged into model_list at provider creation, after this
	// runs, and carry their own proxy. Only model_list is allowed under the lock.
	cfg.Providers = config.ProvidersConfig{}

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
