package gateway

import (
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
)

// The web launcher chat as a second owner route (pkg/config/web_owner.go) needs one
// fact the gateway cannot read from its own config: that the launcher which proxies
// the pico token to the browser authenticates its callers. That lives in the
// launcher's own file, launcher-config.json beside config.json.
//
// The condition is met when a dashboard password is set (a loopback caller or a
// launcher-token holder then gets no session without it), or when nothing but this
// machine can reach the launcher or the gateway's pico endpoint: the launcher is not
// public, trusts no proxy, and gateway.host is a loopback address. Anything else,
// including a file that cannot be read, is not met.
//
// Known gap, accepted and documented in SECURITY.md: the launcher's -public flag
// overrides the file and the gateway cannot see it, and a same-host tunnel looks
// like a loopback caller. Only a dashboard password closes both.

// launcherProbe evaluates the condition against launcher-config.json, re-reading
// the file only when its size or modification time changes, so asking on every
// tool call costs one stat.
type launcherProbe struct {
	path string

	mu     sync.Mutex
	loaded bool
	mtime  time.Time
	size   int64
	cfg    launcherconfig.Config
	err    error
}

func (p *launcherProbe) read() (launcherconfig.Config, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fi, err := os.Stat(p.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No launcher has ever been configured here: its defaults (loopback only, no
		// password) are what a launcher started now would use.
		p.loaded, p.cfg, p.err = false, launcherconfig.Default(), nil
		return p.cfg, nil
	case err != nil:
		p.loaded = false
		return launcherconfig.Config{}, err
	}
	if p.loaded && fi.ModTime().Equal(p.mtime) && fi.Size() == p.size {
		return p.cfg, p.err
	}
	p.cfg, p.err = launcherconfig.Load(p.path, launcherconfig.Default())
	p.loaded, p.mtime, p.size = true, fi.ModTime(), fi.Size()
	return p.cfg, p.err
}

// gate is config.WebLauncherGate.
func (p *launcherProbe) gate(gatewayHost string) (bool, string) {
	lc, err := p.read()
	if err != nil {
		return false, "launcher-config.json could not be read (" + err.Error() + ")"
	}
	if lc.DashboardPasswordHash != "" {
		return true, ""
	}
	var why []string
	if lc.Public {
		why = append(why, "the launcher listens beyond loopback (public)")
	}
	if len(lc.TrustedProxyCIDRs) > 0 {
		why = append(why, "the launcher trusts proxy headers (trusted_proxy_cidrs)")
	}
	if !isLoopbackHost(gatewayHost) {
		why = append(why, "gateway.host is not a loopback address")
	}
	if len(why) == 0 {
		return true, ""
	}
	return false, "no dashboard password is set (khunquant launcher set-password) and " + strings.Join(why, " and ")
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webOwnerNotice says how the web owner route stands at start, or "" when there is
// nothing to say: no pico channel, no valid owner or the switch off are choices, not
// problems, and only a route the operator has asked for that (c) refuses is a warning.
func webOwnerNotice(cfg *config.Config) (warn bool, msg string) {
	if !cfg.Channels.Pico.Enabled {
		return false, ""
	}
	if _, ok := cfg.Proactive.OwnerRoute(); !ok || !cfg.Proactive.Owner.WebIsOwnerEnabled() {
		return false, ""
	}
	active, missing := cfg.WebOwnerStatus()
	if active {
		return false, "The authenticated web launcher chat counts as the owner " +
			"(proactive.owner.web_is_owner); set it to false to restore the configured chat as the only owner"
	}
	return true, "SECURITY: the web launcher chat is NOT the owner: " + missing
}

// armWebOwner arms condition (c) of the web owner route for this process and says,
// once, how it stands. Called at start. The condition itself is evaluated each time
// it is asked (the probe re-reads the file when it changes), so a reload needs no
// re-arming.
func armWebOwner(configPath string, cfg *config.Config) {
	p := &launcherProbe{path: launcherconfig.PathForAppConfig(configPath)}
	config.SetWebLauncherGate(p.gate)
	switch warn, msg := webOwnerNotice(cfg); {
	case warn:
		logger.WarnCF("gateway", msg, nil)
	case msg != "":
		logger.InfoCF("gateway", msg, nil)
	}
}
