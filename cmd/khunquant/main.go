// KhunQuant - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/agent"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/auth"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/clean"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/cron"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/gateway"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/heartbeat"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/launcher"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/migrate"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/model"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/onboard"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/owner"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/replay"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/risk"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/sandboxlab"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/skills"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/start"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/status"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/trigger"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/uninstall"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/update"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/version"
	"github.com/cryptoquantumwave/khunquant/pkg/brand"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
	"github.com/cryptoquantumwave/khunquant/pkg/updater"
)

func NewKhunquantCommand() *cobra.Command {
	short := fmt.Sprintf("%s khunquant - Personal AI Assistant v%s\n\n", internal.Logo, config.GetVersion())

	cmd := &cobra.Command{
		Use:     "khunquant",
		Short:   short,
		Example: "khunquant version",
	}

	cmd.AddCommand(
		start.NewStartCommand(),
		onboard.NewOnboardCommand(),
		agent.NewAgentCommand(),
		auth.NewAuthCommand(),
		gateway.NewGatewayCommand(),
		heartbeat.NewHeartbeatCommand(),
		launcher.NewLauncherCommand(),
		status.NewStatusCommand(),
		trigger.NewTriggerCommand(),
		replay.NewReplayCommand(),
		cron.NewCronCommand(),
		clean.NewCleanCommand(),
		migrate.NewMigrateCommand(),
		skills.NewSkillsCommand(),
		model.NewModelCommand(),
		risk.NewRiskCommand(),
		owner.NewOwnerCommand(),
		sandboxlab.NewSandboxCommand(),
		uninstall.NewUninstallCommand(),
		update.NewUpdateCommand(),
		version.NewVersionCommand(),
	)

	return cmd
}

var banner = "\r\n" + brand.SideBySide(brand.ANSIBlue, brand.ANSIRed, brand.ANSIReset) + "\r\n"

// initTermuxSSL detects Termux environment and sets SSL_CERT_FILE if not already set.
// This fixes X509 certificate errors when running KhunQuant inside Termux or termux-chroot.
func initTermuxSSL() {
	// Only applicable on Linux/Android
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return
	}

	// Skip if already set
	if os.Getenv("SSL_CERT_FILE") != "" {
		return
	}

	// Check for Termux prefix in PATH or HOME
	home := os.Getenv("HOME")
	path := os.Getenv("PATH")

	isTermux := strings.Contains(home, "com.termux") ||
		strings.Contains(path, "com.termux") ||
		strings.Contains(home, "/data/data/com.termux")

	if !isTermux {
		return
	}

	// Check common CA bundle locations in Termux
	caPaths := []string{
		"$PREFIX/etc/tls/cert.pem",
		os.Getenv("PREFIX") + "/etc/tls/cert.pem",
		"/data/data/com.termux/files/usr/etc/tls/cert.pem",
		"/usr/etc/tls/cert.pem",
	}

	for _, caPath := range caPaths {
		expanded := os.ExpandEnv(caPath)
		if _, err := os.Stat(expanded); err == nil {
			os.Setenv("SSL_CERT_FILE", expanded)
			return
		}
	}
}

// isReplay says whether the process was started as `khunquant replay`.
func isReplay(args []string) bool { return len(args) > 1 && args[1] == "replay" }

// isSandboxJSON says whether the process was started as `khunquant sandbox ... --json`,
// whose standard output must be nothing but the JSON, or as `khunquant sandbox mcp`,
// whose standard output is the MCP protocol (and which makes no update check either), or
// as `khunquant sandbox run`, whose output is a report and which touches nothing of the
// developer's: no banner, no update check, no cache under their home.
func isSandboxJSON(args []string) bool {
	return len(args) > 1 && args[1] == "sandbox" && (slices.Contains(args, "--json") || (len(args) > 2 && (args[2] == "mcp" || args[2] == "run")))
}

// isLockedGateway says whether the process was asked to run as a locked sandbox
// gateway (the gateway's own lockRequested: the env switch or the flag). It runs
// before the gateway arms its lock, so it only reads the request.
func isLockedGateway(args []string) bool {
	return os.Getenv("KQ_SANDBOX_LOCKED") == "1" ||
		slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--sandbox-locked") }) // also =true
}

// exitStatus is how the process ends for an error from a command. A command that
// silences cobra's own error output (`khunquant trigger`) returns an error that
// names its exit status; its message is printed here, on stderr. Any other error
// was already printed by cobra and ends the process with 1.
func exitStatus(err error) int {
	var ee interface {
		error
		ExitCode() int
	}
	if errors.As(err, &ee) {
		if msg := ee.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, "Error:", msg)
		}
		return ee.ExitCode()
	}
	return 1
}

func main() {
	// Initialize Termux SSL certificate detection before anything else
	initTermuxSSL()

	// `khunquant replay` prints its report and writes nothing but the file it was told
	// to: no banner on its standard output, no cache file, no network call for news of
	// an update.
	quiet := isReplay(os.Args) || isSandboxJSON(os.Args)
	if !quiet {
		fmt.Printf("%s", banner)
	}

	// Install a file-backed PassphraseProvider so enc:// credentials are
	// decrypted automatically if ~/.khunquant/.passphrase exists.
	// KHUNQUANT_KEY_PASSPHRASE env var still takes precedence when set.
	credential.InstallFileBackedProvider()

	// Read the cached update result instantly (no network wait), and kick off
	// a background refresh so the cache stays fresh for the next invocation.
	// A locked sandbox gateway makes no update check: it would call GitHub before the
	// egress guard is armed and write a cache under the instance home, which the lock
	// then refuses as a second ~/.khunquant.
	var info *updater.UpdateInfo
	if !quiet && !isLockedGateway(os.Args) {
		info = updater.CheckForUpdateCached(updater.DefaultOwner, updater.DefaultRepo, config.GetVersion())
	}
	if info != nil && info.IsOutdated {
		fmt.Printf(
			"%s Update available: %s (you have %s)\n   → %s\n\n",
			internal.Logo,
			info.LatestVersion,
			info.CurrentVersion,
			info.ReleaseURL,
		)
	}

	cmd := NewKhunquantCommand()
	if err := cmd.Execute(); err != nil {
		os.Exit(exitStatus(err))
	}
}
