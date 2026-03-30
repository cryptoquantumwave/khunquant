// KhunQuant - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/khunquant/khunquant/cmd/khunquant/internal"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/agent"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/auth"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/clean"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/cron"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/gateway"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/migrate"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/model"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/onboard"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/skills"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/status"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/version"
	"github.com/khunquant/khunquant/pkg/brand"
	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/updater"
)

func NewKhunquantCommand() *cobra.Command {
	short := fmt.Sprintf("%s khunquant - Personal AI Assistant v%s\n\n", internal.Logo, config.GetVersion())

	// Start update check in background immediately; result is read in PersistentPreRun.
	updateCh := make(chan *updater.UpdateInfo, 1)
	go func() {
		info, _ := updater.CheckForUpdate(context.Background(), "armmer016", "khunquant", config.GetVersion())
		updateCh <- info
	}()

	cmd := &cobra.Command{
		Use:     "khunquant",
		Short:   short,
		Example: "khunquant version",
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			// Skip update notice when running the version command (it already
			// shows version info) or any help invocation.
			if cmd.Name() == "version" || cmd.Name() == "v" {
				return
			}
			// Wait briefly for the goroutine; don't block startup if slow.
			select {
			case info := <-updateCh:
				if info != nil && info.IsOutdated {
					fmt.Printf(
						"\n%s Update available: %s (you have %s)\n   → %s\n\n",
						internal.Logo,
						info.LatestVersion,
						info.CurrentVersion,
						info.ReleaseURL,
					)
				}
			case <-time.After(1500 * time.Millisecond):
				// timed out — proceed silently
			}
		},
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
		version.NewVersionCommand(),
	)

	return cmd
}

var banner = "\r\n" + brand.SideBySide(brand.ANSIBlue, brand.ANSIRed, brand.ANSIReset) + "\r\n"

func main() {
	fmt.Printf("%s", banner)
	cmd := NewKhunquantCommand()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
