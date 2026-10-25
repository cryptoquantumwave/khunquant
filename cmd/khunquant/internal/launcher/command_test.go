package launcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

func TestNewLauncherCommand(t *testing.T) {
	cmd := NewLauncherCommand()

	require.NotNil(t, cmd)
	assert.Equal(t, "launcher", cmd.Use)
	assert.Nil(t, cmd.Run)
	assert.NotNil(t, cmd.RunE)
	assert.True(t, cmd.HasSubCommands())

	subcommands := cmd.Commands()
	require.Len(t, subcommands, 1)
	assert.Equal(t, "set-password", subcommands[0].Name())
	assert.NotNil(t, subcommands[0].RunE)
}

// TestResolveLauncherConfigPath_MatchesLauncherDerivation pins the derivation
// the launcher binary uses: writing to any other path produces a hash the
// launcher never reads, failing silently.
func TestResolveLauncherConfigPath_MatchesLauncherDerivation(t *testing.T) {
	dir := t.TempDir()
	appConfig := filepath.Join(dir, "config.json")

	got, err := resolveLauncherConfigPath(appConfig)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(dir, launcherconfig.FileName), got)
}

func TestResolveLauncherConfigPath_EmptyUsesDefault(t *testing.T) {
	got, err := resolveLauncherConfigPath("")
	require.NoError(t, err)

	assert.True(t, filepath.IsAbs(got))
	assert.Equal(t, launcherconfig.FileName, filepath.Base(got))
}

// TestSetPassword_WritesHashTheLauncherAccepts is the C-6 property: the written
// hash must satisfy the launcher's own verifier, or the gate still cannot be
// reached.
func TestSetPassword_WritesHashTheLauncherAccepts(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherconfig.FileName)

	// Mirrors what the command does after reading the password, without a TTY.
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery"), bcryptCost)
	require.NoError(t, err)

	cfg := launcherconfig.Default()
	cfg.DashboardPasswordHash = string(hash)
	require.NoError(t, launcherconfig.Save(launcherPath, cfg))

	loaded, err := launcherconfig.Load(launcherPath, launcherconfig.Default())
	require.NoError(t, err)

	assert.True(t, middleware.VerifyDashboardPassword(loaded.DashboardPasswordHash, "correct horse battery"))
	assert.False(t, middleware.VerifyDashboardPassword(loaded.DashboardPasswordHash, "wrong password"))
}

// TestSetPassword_DefaultFallbackSavesOnFreshInstall guards the trap that Save
// validates: a zero-value Config would be rejected with "port 0 is out of
// range" when no launcher-config.json exists yet.
func TestSetPassword_DefaultFallbackSavesOnFreshInstall(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherconfig.FileName)

	_, err := os.Stat(launcherPath)
	require.True(t, os.IsNotExist(err), "precondition: no launcher config yet")

	cfg, err := launcherconfig.Load(launcherPath, launcherconfig.Default())
	require.NoError(t, err)

	cfg.DashboardPasswordHash = "$2a$12$abcdefghijklmnopqrstuv"
	require.NoError(t, launcherconfig.Save(launcherPath, cfg))

	// The zero value is what must NOT be used as the fallback.
	assert.Error(t, launcherconfig.Save(launcherPath, launcherconfig.Config{}))
}

// TestSetPassword_ClearRoundTrips covers --clear: omitempty drops the key
// entirely, and Load must read that back as "no password configured" rather
// than failing.
func TestSetPassword_ClearRoundTrips(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, launcherconfig.FileName)

	cfg := launcherconfig.Default()
	cfg.DashboardPasswordHash = "$2a$12$abcdefghijklmnopqrstuv"
	require.NoError(t, launcherconfig.Save(launcherPath, cfg))

	cfg.DashboardPasswordHash = ""
	require.NoError(t, launcherconfig.Save(launcherPath, cfg))

	loaded, err := launcherconfig.Load(launcherPath, launcherconfig.Default())
	require.NoError(t, err)
	assert.Empty(t, loaded.DashboardPasswordHash)

	// An empty hash must not authenticate anything.
	assert.False(t, middleware.VerifyDashboardPassword(loaded.DashboardPasswordHash, ""))
}
