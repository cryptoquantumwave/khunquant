package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

type exitErr struct{ code int }

func (e exitErr) Error() string { return "boom" }
func (e exitErr) ExitCode() int { return e.code }

func TestExitStatus(t *testing.T) {
	assert.Equal(t, 1, exitStatus(fmt.Errorf("plain")))
	assert.Equal(t, 3, exitStatus(exitErr{3}))
	assert.Equal(t, 4, exitStatus(fmt.Errorf("wrapped: %w", exitErr{4})))
}

func TestIsReplay(t *testing.T) {
	assert.True(t, isReplay([]string{"khunquant", "replay", "--events", "x"}))
	assert.False(t, isReplay([]string{"khunquant"}))
	assert.False(t, isReplay([]string{"khunquant", "status", "replay"}))
}

func TestNewKhunquantCommand(t *testing.T) {
	cmd := NewKhunquantCommand()

	require.NotNil(t, cmd)

	short := fmt.Sprintf("%s khunquant - Personal AI Assistant v%s\n\n", internal.Logo, config.GetVersion())

	assert.Equal(t, "khunquant", cmd.Use)
	assert.Equal(t, short, cmd.Short)

	assert.True(t, cmd.HasSubCommands())
	assert.True(t, cmd.HasAvailableSubCommands())

	assert.False(t, cmd.HasFlags())

	assert.Nil(t, cmd.Run)
	assert.Nil(t, cmd.RunE)

	assert.Nil(t, cmd.PersistentPreRun)
	assert.Nil(t, cmd.PersistentPostRun)

	allowedCommands := []string{
		"agent",
		"auth",
		"clean",
		"cron",
		"gateway",
		"heartbeat",
		"launcher",
		"migrate",
		"model",
		"onboard",
		"replay",
		"owner",
		"risk",
		"sandbox",
		"skills",
		"start",
		"status",
		"trigger",
		"uninstall",
		"update",
		"version",
	}

	subcommands := cmd.Commands()
	assert.Len(t, subcommands, len(allowedCommands))

	for _, subcmd := range subcommands {
		found := slices.Contains(allowedCommands, subcmd.Name())
		assert.True(t, found, "unexpected subcommand %q", subcmd.Name())

		assert.False(t, subcmd.Hidden)
	}
}
