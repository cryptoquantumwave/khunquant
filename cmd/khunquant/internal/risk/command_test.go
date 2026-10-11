package risk

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

var cliNow = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

// seededWorkspace writes a state file in which one account is 60 USD below its
// baseline, and returns the deps that point the command at it.
func seededWorkspace(t *testing.T, limit float64) (deps, string) {
	t.Helper()
	ws := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = ws
	cfg.TradingRisk.DailyLossLimitUSD = limit

	path := broker.LossStatePath(ws)
	seed := &broker.DailyLossTracker{}
	seed.SetClock(func() time.Time { return cliNow })
	seed.SetStorePath(path)
	seed.SampleAccount("binance/main", 1000, cliNow.Add(-time.Hour))
	seed.SampleAccount("binance/main", 940, cliNow.Add(-5*time.Minute))
	seed.RecordLoss(10)
	require.Equal(t, 60.0, seed.TodayLossUSD())

	return deps{
		loadConfig: func() (*config.Config, error) { return cfg, nil },
		now:        func() time.Time { return cliNow },
	}, path
}

func run(t *testing.T, d deps, args ...string) (string, error) {
	t.Helper()
	cmd := newRiskCommand(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestNewRiskCommand(t *testing.T) {
	cmd := NewRiskCommand()
	require.NotNil(t, cmd)
	assert.Equal(t, "risk", cmd.Name())
	names := []string{}
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	assert.ElementsMatch(t, []string{"status", "reset-daily-loss"}, names)
}

func TestRiskStatusCommand_Prints(t *testing.T) {
	d, _ := seededWorkspace(t, 50)
	out, err := run(t, d, "status")
	require.NoError(t, err)

	for _, want := range []string{
		"2026-10-07 (UTC)",
		"Realised loss: 10.00 USD",
		"binance/main", "1000.00", "940.00", "60.00", "5m0s",
		"Total loss:    60.00 USD",
		"Limit:         50.00 USD",
		"Tripped:       YES",
	} {
		assert.Contains(t, out, want)
	}
}

func TestRiskStatusCommand_NotTrippedWithoutLimit(t *testing.T) {
	d, _ := seededWorkspace(t, 0)
	out, err := run(t, d, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "none")
	assert.Contains(t, out, "Tripped:       no")
}

func TestRiskResetCommand_WritesState(t *testing.T) {
	d, path := seededWorkspace(t, 50)

	// A "running gateway": its own tracker on the same file, tripped.
	running := &broker.DailyLossTracker{}
	running.SetClock(func() time.Time { return cliNow })
	running.SetStorePath(path)
	require.Error(t, running.CheckDailyLossFor(50, "binance/main"), "setup: the limit should be tripped")

	out, err := run(t, d, "reset-daily-loss", "--reason", "withdrawal")
	require.NoError(t, err)
	assert.Contains(t, out, "was 60.00 USD, now 0.00 USD")

	// The running tracker picks the reset up on its next check.
	assert.NoError(t, running.CheckDailyLossFor(50, "binance/main"))

	// What was written: a fresh tracker on the same file sees a zeroed day,
	// a re-baselined account and the recorded reason.
	check := &broker.DailyLossTracker{}
	check.SetClock(func() time.Time { return cliNow })
	check.SetStorePath(path)
	snap := check.Snapshot()
	assert.Equal(t, 0.0, snap.TotalUSD)
	assert.Equal(t, 0.0, snap.RealizedUSD)
	assert.Equal(t, 940.0, snap.Accounts["binance/main"].BaselineUSD)
	assert.Equal(t, "withdrawal", snap.ResetReason)
	require.NotNil(t, snap.ResetAt)
	assert.NoError(t, check.CheckDailyLossFor(50, "binance/main"))

	status, err := run(t, d, "status")
	require.NoError(t, err)
	assert.Contains(t, status, "Last reset:")
	assert.Contains(t, status, "withdrawal")
}

func TestRiskResetCommand_RequiresReason(t *testing.T) {
	d, path := seededWorkspace(t, 50)
	for _, args := range [][]string{{"reset-daily-loss"}, {"reset-daily-loss", "--reason", "   "}} {
		_, err := run(t, d, args...)
		require.Error(t, err, "args %v", args)
		assert.Contains(t, err.Error(), "--reason")
	}
	// Nothing changed.
	check := &broker.DailyLossTracker{}
	check.SetClock(func() time.Time { return cliNow })
	check.SetStorePath(path)
	assert.Equal(t, 60.0, check.TodayLossUSD())
}

// An invalid zone must not lock the operator out of the reset escape hatch: the
// command falls back to UTC like the gateway does.
func TestRiskCommand_InvalidTimezoneFallsBackToUTC(t *testing.T) {
	d, _ := seededWorkspace(t, 50)
	inner := d.loadConfig
	d.loadConfig = func() (*config.Config, error) {
		cfg, err := inner()
		cfg.TradingRisk.LossDayTimezone = "Mars/Olympus_Mons"
		return cfg, err
	}
	out, err := run(t, d, "status")
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "(UTC)"))
	_, err = run(t, d, "reset-daily-loss", "--reason", "typo in tz")
	require.NoError(t, err)
}

func TestRiskStatusCommand_NoStateFile(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "empty")
	d := deps{loadConfig: func() (*config.Config, error) { return cfg, nil }, now: func() time.Time { return cliNow }}
	out, err := run(t, d, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "no samples yet today")
}
