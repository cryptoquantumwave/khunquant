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

// v2With is the v2 template's instructions followed by tasks.
func v2With(tasks string) string {
	v2 := hb.TemplateV2()
	return v2[:strings.Index(v2, userTasksLine)+len(userTasksLine)] + "\n\n" + tasks + "\n"
}

// v1Upgraded is v1File after the upgrade: the owner's one task survives.
var v1Upgraded = v2With("- my own task: check SOL")

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

	assert.Equal(t, v1Upgraded, r.read("HEARTBEAT.md"), "the owner's task survives")
	assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak"), "the backup is the old file, byte for byte")
	assert.Equal(t, []string{"HEARTBEAT.md", "HEARTBEAT.md.v1.bak"}, r.files(), "no temp file is left behind")
	assert.Contains(t, out, "Backed up")
	assert.Contains(t, out, "HEARTBEAT.md.v1.bak")
	assert.Contains(t, out, "Your tasks")
	assert.Contains(t, out, "kept")
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
	assert.Equal(t, v1Upgraded, r.read("HEARTBEAT.md"))
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
	assert.Equal(t, v2With("- my own task: check SOL\n\n- another edit"), r.read("HEARTBEAT.md"))
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
	assert.Equal(t, v1Upgraded, r.read("HEARTBEAT.md"))
}
