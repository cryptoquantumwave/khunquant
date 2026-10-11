package heartbeat

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	hb "github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
)

const v1File = "# Heartbeat\n\n## How delivery actually works\n\nCall `message`.\n\n" +
	"Add your heartbeat tasks below this line:\n\n- my own task: check SOL\n"

// rig points the command at a temp workspace.
type rig struct {
	t         *testing.T
	workspace string
	cfg       *config.Config
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := newLegacyRig(t)
	// The shipped default is legacy; the upgrade is meant for a governed install.
	r.cfg.Proactive.Enabled = true
	r.cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	r.cfg.Proactive.Attention.Mode = config.ProactiveModeLive
	r.cfg.Proactive.Heartbeat.Mode = config.HeartbeatModeGoverned
	return r
}

// newLegacyRig is a rig on the shipped default config: heartbeat mode legacy.
func newLegacyRig(t *testing.T) *rig {
	t.Helper()
	cfg := config.DefaultConfig()
	ws := t.TempDir()
	cfg.Agents.Defaults.Workspace = ws
	return &rig{t: t, workspace: ws, cfg: cfg}
}

func (r *rig) path(name string) string { return filepath.Join(r.workspace, name) }

func (r *rig) write(name, content string) {
	r.t.Helper()
	require.NoError(r.t, os.WriteFile(r.path(name), []byte(content), 0o644))
}

func (r *rig) read(name string) string {
	r.t.Helper()
	data, err := os.ReadFile(r.path(name))
	require.NoError(r.t, err)
	return string(data)
}

func (r *rig) files() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.workspace)
	require.NoError(r.t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func (r *rig) run(args ...string) (string, error) {
	r.t.Helper()
	cmd := newHeartbeatCommand(deps{
		configPath: func() string { return "unused" },
		load:       func(string) (*config.Config, error) { return r.cfg, nil },
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"upgrade-template"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestUpgradeTemplate_BacksUp(t *testing.T) {
	r := newRig(t)
	r.write("HEARTBEAT.md", v1File)

	out, err := r.run()
	require.NoError(t, err)

	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
	assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak"), "the backup is the old file, byte for byte")
	assert.Equal(t, []string{"HEARTBEAT.md", "HEARTBEAT.md.v1.bak"}, r.files(), "no temp file is left behind")
	assert.Contains(t, out, "Backed up")
	assert.Contains(t, out, "HEARTBEAT.md.v1.bak")
	assert.Contains(t, out, "copy your own tasks")
	assert.True(t, hb.IsV2Template(r.read("HEARTBEAT.md")))
	assert.False(t, hb.IsV1Template(r.read("HEARTBEAT.md")))
}

func TestUpgradeTemplate_IsIdempotent(t *testing.T) {
	r := newRig(t)
	r.write("HEARTBEAT.md", v1File)
	_, err := r.run()
	require.NoError(t, err)
	files := r.files()

	out, err := r.run()
	require.NoError(t, err)
	assert.Contains(t, out, "already the v2 template")
	assert.Equal(t, files, r.files(), "a second run must not add a backup")
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
	assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak"))

	// A v2 file the user has edited is still v2: it is not touched either.
	edited := r.read("HEARTBEAT.md") + "\n- one more task\n"
	r.write("HEARTBEAT.md", edited)
	out, err = r.run()
	require.NoError(t, err)
	assert.Contains(t, out, "already the v2 template")
	assert.Equal(t, edited, r.read("HEARTBEAT.md"))
}

func TestUpgradeTemplate_NeverOverwritesAnExistingBackup(t *testing.T) {
	r := newRig(t)
	r.write("HEARTBEAT.md.v1.bak", "an older backup, precious")
	r.write("HEARTBEAT.md", v1File)

	out, err := r.run()
	require.NoError(t, err)
	assert.Equal(t, "an older backup, precious", r.read("HEARTBEAT.md.v1.bak"))
	assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak.1"))
	assert.Contains(t, out, "HEARTBEAT.md.v1.bak.1")

	// Downgrade again by hand, upgrade again: the next free name.
	r.write("HEARTBEAT.md", v1File+"\n- another edit\n")
	_, err = r.run()
	require.NoError(t, err)
	assert.Equal(t, "an older backup, precious", r.read("HEARTBEAT.md.v1.bak"))
	assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak.1"))
	assert.Equal(t, v1File+"\n- another edit\n", r.read("HEARTBEAT.md.v1.bak.2"))
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
}

func TestWriteBackup_RefusesToReplace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "HEARTBEAT.md.v1.bak")
	require.NoError(t, os.WriteFile(p, []byte("keep"), 0o644))
	err := writeBackup(p, []byte("new"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrExist))
	data, _ := os.ReadFile(p)
	assert.Equal(t, "keep", string(data))
}

func TestUpgradeTemplate_DryRunWritesNothing(t *testing.T) {
	r := newRig(t)
	r.write("HEARTBEAT.md.v1.bak", "older")
	r.write("HEARTBEAT.md", v1File)
	before := r.files()

	out, err := r.run("--dry-run")
	require.NoError(t, err)

	assert.Equal(t, before, r.files(), "a dry run creates no file")
	assert.Equal(t, v1File, r.read("HEARTBEAT.md"))
	assert.Equal(t, "older", r.read("HEARTBEAT.md.v1.bak"))
	assert.Contains(t, out, "[dry run] Would back up")
	assert.Contains(t, out, "HEARTBEAT.md.v1.bak.1", "it names the backup it would pick")
	assert.Contains(t, out, "Would write the v2 template")
	assert.Contains(t, out, "Nothing was changed")

	// And a dry run on an already-v2 file says so and does nothing.
	r.write("HEARTBEAT.md", hb.TemplateV2())
	out, err = r.run("--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "already the v2 template")
	assert.Equal(t, before, r.files())
}

func TestUpgradeTemplate_MissingFileGetsV2WithoutABackup(t *testing.T) {
	r := newRig(t)
	out, err := r.run()
	require.NoError(t, err)
	assert.Equal(t, []string{"HEARTBEAT.md"}, r.files())
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
	assert.Contains(t, out, "nothing to back up")

	r2 := newRig(t)
	_, err = r2.run("--dry-run")
	require.NoError(t, err)
	assert.Empty(t, r2.files())
}

// A file the user wrote from scratch has neither marker. It is still replaced
// (they asked for the upgrade), and what they wrote survives in the backup.
func TestUpgradeTemplate_CustomFileIsBackedUp(t *testing.T) {
	r := newRig(t)
	const custom = "Every hour, tell me the BTC price.\n"
	r.write("HEARTBEAT.md", custom)
	out, err := r.run()
	require.NoError(t, err)
	assert.Equal(t, custom, r.read("HEARTBEAT.md.v1.bak"))
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
	assert.Contains(t, out, "any tasks you added")
}

const legacyWarning = "WARNING: the heartbeat is in legacy mode; with the v2 template it will deliver nothing " +
	"until proactive.heartbeat.mode is governed and attention is live"

// While the effective mode is not governed the v2 file would make heartbeats
// deliver nothing: the command warns before writing and refuses without --force.
func TestUpgradeTemplate_LegacyModeNeedsForce(t *testing.T) {
	r := newLegacyRig(t)
	r.write("HEARTBEAT.md", v1File)
	before := r.files()

	out, err := r.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")
	assert.Contains(t, out, legacyWarning)
	assert.Equal(t, before, r.files(), "a refused upgrade writes nothing")
	assert.Equal(t, v1File, r.read("HEARTBEAT.md"))

	// A governed config that attention cannot back (shadow) is not governed either.
	g := newRig(t)
	g.cfg.Proactive.Attention.Mode = config.ProactiveModeShadow
	g.write("HEARTBEAT.md", v1File)
	out, err = g.run()
	require.Error(t, err)
	assert.Contains(t, out, legacyWarning)
	assert.Equal(t, v1File, g.read("HEARTBEAT.md"))

	// --dry-run shows the warning and the refusal, and writes nothing.
	out, err = r.run("--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, legacyWarning)
	assert.Contains(t, out, "Without --force the upgrade would be refused")
	assert.Equal(t, before, r.files())

	// --force goes ahead, with the warning first.
	out, err = r.run("--force")
	require.NoError(t, err)
	assert.Less(t, strings.Index(out, legacyWarning), strings.Index(out, "Wrote the v2 template"),
		"the warning must come before anything is written")
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
	assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak"))

	// A v2 file in legacy mode is still just "already v2".
	out, err = r.run()
	require.NoError(t, err)
	assert.Contains(t, out, "already the v2 template")
}

func TestUpgradeTemplate_GovernedNeedsNoForce(t *testing.T) {
	r := newRig(t)
	r.write("HEARTBEAT.md", v1File)
	out, err := r.run()
	require.NoError(t, err)
	assert.NotContains(t, out, "WARNING")
	assert.Contains(t, out, "no restart is needed")
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
}

func TestUpgradeTemplate_ConfigErrorIsReported(t *testing.T) {
	cmd := newHeartbeatCommand(deps{
		configPath: func() string { return "x" },
		load:       func(string) (*config.Config, error) { return nil, errors.New("boom") },
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"upgrade-template"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// The command writes the file the heartbeat service reads, which is
// cfg.WorkspacePath(), not an agent-specific workspace.
func TestUpgradeTemplate_UsesTheWorkspaceTheHeartbeatReads(t *testing.T) {
	r := newRig(t)
	r.cfg.Agents.List = []config.AgentConfig{{ID: "main", Default: true, Workspace: t.TempDir()}}
	r.write("HEARTBEAT.md", v1File)
	_, err := r.run()
	require.NoError(t, err)
	assert.Equal(t, hb.TemplateV2(), r.read("HEARTBEAT.md"))
}
