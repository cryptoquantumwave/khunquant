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

	"github.com/khunquant/khunquant/cmd/khunquant/internal"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/agent"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/auth"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/cron"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/gateway"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/migrate"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/model"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/onboard"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/skills"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/status"
	"github.com/khunquant/khunquant/cmd/khunquant/internal/version"
	"github.com/khunquant/khunquant/pkg/config"
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
		migrate.NewMigrateCommand(),
		skills.NewSkillsCommand(),
		model.NewModelCommand(),
		version.NewVersionCommand(),
	)

	return cmd
}

const (
	colorBlue = "\033[1;38;2;62;93;185m"
	colorRed  = "\033[1;38;2;213;70;70m"
	banner    = "\r\n" +
		colorBlue + "██████╗ ██╗ ██████╗ ██████╗ " + colorRed + " ██████╗██╗      █████╗ ██╗    ██╗\n" +
		colorBlue + "██╔══██╗██║██╔════╝██╔═══██╗" + colorRed + "██╔════╝██║     ██╔══██╗██║    ██║\n" +
		colorBlue + "██████╔╝██║██║     ██║   ██║" + colorRed + "██║     ██║     ███████║██║ █╗ ██║\n" +
		colorBlue + "██╔═══╝ ██║██║     ██║   ██║" + colorRed + "██║     ██║     ██╔══██║██║███╗██║\n" +
		colorBlue + "██║     ██║╚██████╗╚██████╔╝" + colorRed + "╚██████╗███████╗██║  ██║╚███╔███╔╝\n" +
		colorBlue + "╚═╝     ╚═╝ ╚═════╝ ╚═════╝ " + colorRed + " ╚═════╝╚══════╝╚═╝  ╚═╝ ╚══╝╚══╝\n " +
		"\033[0m\r\n"
)

func main() {
	fmt.Printf("%s", banner)
	cmd := NewKhunquantCommand()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
