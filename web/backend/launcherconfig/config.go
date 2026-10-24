package launcherconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	// FileName is the launcher-specific settings file name.
	FileName = "launcher-config.json"
	// DefaultPort is the default port for the web launcher.
	DefaultPort = 18800
	// EnvLauncherHost overrides launcher listen host.
	EnvLauncherHost = "PICOCLAW_LAUNCHER_HOST"
)

// Config stores launch parameters for the web backend service.
type Config struct {
	Port                 int      `json:"port"`
	Public               bool     `json:"public"`
	AllowedCIDRs         []string `json:"allowed_cidrs,omitempty"`
	AllowLocalhostBypass bool     `json:"allow_localhost_bypass"`
	TrustedProxyCIDRs    []string `json:"trusted_proxy_cidrs,omitempty"`
	// AllowedHosts lists extra Host names the launcher answers to, for a
	// reverse proxy serving it under a domain. localhost, *.localhost and IP
	// literals are always allowed; any other Host gets 421 (DNS rebinding).
	AllowedHosts          []string `json:"allowed_hosts,omitempty"`
	LauncherToken         string   `json:"launcher_token,omitempty"`
	DashboardPasswordHash string   `json:"dashboard_password_hash,omitempty"`
	// BehindTLSProxy marks session cookies Secure even though the launcher
	// itself serves plain HTTP, for deployments behind a TLS-terminating
	// reverse proxy. An explicit setting, not X-Forwarded-Proto: that header
	// is written by the client unless a proxy overwrites it.
	BehindTLSProxy bool `json:"behind_tls_proxy,omitempty"`
}

// Default returns default launcher settings.
func Default() Config {
	return Config{Port: DefaultPort, Public: false, AllowLocalhostBypass: true}
}

// Validate checks if launcher settings are valid.
func Validate(cfg Config) error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port %d is out of range (1-65535)", cfg.Port)
	}
	for _, cidr := range cfg.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid CIDR %q", cidr)
		}
	}
	for _, cidr := range cfg.TrustedProxyCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid trusted proxy CIDR %q", cidr)
		}
		// M-15: a /0 trusts X-Forwarded-For from every peer, letting any
		// client claim to be any address - including an allowed one.
		if ones, _ := ipNet.Mask.Size(); ones == 0 {
			return fmt.Errorf("trusted proxy CIDR %q trusts every address; list the proxy's own address instead", cidr)
		}
	}
	for _, h := range cfg.AllowedHosts {
		if strings.ContainsAny(h, "/@*\\ \t") || strings.Contains(h, "://") {
			return fmt.Errorf("invalid allowed host %q: give a bare host name, e.g. bot.example.com", h)
		}
	}
	return nil
}

// NormalizeHosts trims entries, lower-cases them, strips a trailing dot, removes
// empty values, and deduplicates.
func NormalizeHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		h := strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), ".")
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeCIDRs trims entries, removes empty values, and deduplicates CIDRs.
func NormalizeCIDRs(cidrs []string) []string {
	if len(cidrs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cidrs))
	seen := make(map[string]struct{}, len(cidrs))
	for _, raw := range cidrs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PathForAppConfig returns launcher-config path near the app config file.
func PathForAppConfig(appConfigPath string) string {
	dir := filepath.Dir(appConfigPath)
	if dir == "" || dir == "." {
		dir = "."
	}
	return filepath.Join(dir, FileName)
}

// Load reads launcher settings; fallback is returned when file does not exist.
func Load(path string, fallback Config) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fallback, nil
		}
		return Config{}, err
	}

	cfg := fallback
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.AllowedCIDRs = NormalizeCIDRs(cfg.AllowedCIDRs)
	cfg.TrustedProxyCIDRs = NormalizeCIDRs(cfg.TrustedProxyCIDRs)
	cfg.AllowedHosts = NormalizeHosts(cfg.AllowedHosts)
	cfg.LauncherToken = strings.TrimSpace(cfg.LauncherToken)
	cfg.DashboardPasswordHash = strings.TrimSpace(cfg.DashboardPasswordHash)
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes launcher settings to disk.
func Save(path string, cfg Config) error {
	cfg.AllowedCIDRs = NormalizeCIDRs(cfg.AllowedCIDRs)
	cfg.TrustedProxyCIDRs = NormalizeCIDRs(cfg.TrustedProxyCIDRs)
	cfg.AllowedHosts = NormalizeHosts(cfg.AllowedHosts)
	cfg.LauncherToken = strings.TrimSpace(cfg.LauncherToken)
	cfg.DashboardPasswordHash = strings.TrimSpace(cfg.DashboardPasswordHash)
	if err := Validate(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
