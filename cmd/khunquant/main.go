// KhunQuant - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package main

import (
	"fmt"
	"os"

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
	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/status"
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
		update.NewUpdateCommand(),
		version.NewVersionCommand(),
	)

	return cmd
}

var banner = "\r\n" + brand.SideBySide(brand.ANSIBlue, brand.ANSIRed, brand.ANSIReset) + "\r\n"

func main() {
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
