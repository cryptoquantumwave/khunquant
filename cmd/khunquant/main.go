// KhunQuant - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/agent"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/auth"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/clean"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/cron"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/gateway"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/migrate"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/model"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/onboard"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/skills"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/start"
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/status"
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
		status.NewStatusCommand(),
		cron.NewCronCommand(),
		clean.NewCleanCommand(),
		migrate.NewMigrateCommand(),
		skills.NewSkillsCommand(),
		model.NewModelCommand(),
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

func main() {
	// Initialize Termux SSL certificate detection before anything else
	initTermuxSSL()

	fmt.Printf("%s", banner)

	// Install a file-backed PassphraseProvider so enc:// credentials are
	// decrypted automatically if ~/.khunquant/.passphrase exists.
	// KHUNQUANT_KEY_PASSPHRASE env var still takes precedence when set.
	credential.InstallFileBackedProvider()

	// Read the cached update result instantly (no network wait), and kick off
	// a background refresh so the cache stays fresh for the next invocation.
	info := updater.CheckForUpdateCached(updater.DefaultOwner, updater.DefaultRepo, config.GetVersion())
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
		os.Exit(1)
	}
}
