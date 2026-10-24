package heartbeat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	hb "github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
)

const shippedV1Task = "- Run the full portfolio + market scan above and `message` me only on what is\n" +
	"  genuinely material, following the guardrails, dedup ledger, and escalation rules."

// A file the user wrote from scratch has no tasks marker. It is replaced (they
// asked for the upgrade), kept whole under the new tasks section, and the output
// says so.
func TestUpgradeTemplate_CustomFileIsKeptWholeUnderTheTasks(t *testing.T) {
	r := newRig(t)
	const custom = "Every hour, tell me the BTC price.\n"
	r.write("HEARTBEAT.md", custom)
	out, err := r.run()
	require.NoError(t, err)
	assert.Equal(t, custom, r.read("HEARTBEAT.md.v1.bak"))
	assert.Equal(t, v2With("Every hour, tell me the BTC price."), r.read("HEARTBEAT.md"))
	assert.Contains(t, out, "WHOLE content")
	assert.Contains(t, out, "HEARTBEAT.md.v1.bak")
}

func TestUpgradeTemplate_TaskListCases(t *testing.T) {
	const header = "# Heartbeat\n\n## How delivery actually works\n\nCall `message`.\n\n"
	v2 := hb.TemplateV2()
	idle := v2[:strings.Index(v2, userTasksLine)+len(userTasksLine)] + "\n"
	for name, tc := range map[string]struct {
		old  string
		want string
		say  string
	}{
		"no tasks":     {header + userTasksLine + "\n", idle, "no tasks"},
		"shipped task": {header + userTasksLine + "\n\n" + shippedV1Task + "\n", hb.TemplateV2(), "default one of the v1 template"},
		"two tasks":    {header + userTasksLine + "\n\n- a\n- b\n", v2With("- a\n- b"), "kept in the new file"},
	} {
		r := newLegacyRig(t)
		r.write("HEARTBEAT.md", tc.old)
		out, err := r.run()
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, r.read("HEARTBEAT.md"), name)
		assert.Contains(t, out, tc.say, name)
		assert.Equal(t, tc.old, r.read("HEARTBEAT.md.v1.bak"), name)
	}
}

// The upgrade works in every heartbeat mode, with the same result, and --force is
// accepted and changes nothing.
func TestUpgradeTemplate_WorksInEveryMode(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) *rig{
		"legacy":   newLegacyRig,
		"governed": newRig,
		"sweep": func(t *testing.T) *rig {
			r := newRig(t)
			r.cfg.Proactive.Heartbeat.Mode = config.HeartbeatModeSweep
			r.cfg.Proactive.Perception.Enabled = true
			return r
		},
		"governed but shadow attention": func(t *testing.T) *rig {
			r := newRig(t)
			r.cfg.Proactive.Attention.Mode = config.ProactiveModeShadow
			return r
		},
	} {
		for _, flags := range [][]string{nil, {"--force"}} {
			r := mk(t)
			r.write("HEARTBEAT.md", v1File)
			out, err := r.run(flags...)
			require.NoError(t, err, name)
			assert.Equal(t, v1Upgraded, r.read("HEARTBEAT.md"), name)
			assert.Equal(t, v1File, r.read("HEARTBEAT.md.v1.bak"), name)
			assert.NotContains(t, out, "WARNING", name)
			assert.NotContains(t, out, "refus", name)
			assert.Equal(t, len(flags) > 0, strings.Contains(out, "--force is no longer needed"), name)

			// A dry run changes nothing and says what would be kept.
			d := mk(t)
			d.write("HEARTBEAT.md", v1File)
			out, err = d.run("--dry-run")
			require.NoError(t, err, name)
			assert.Equal(t, []string{"HEARTBEAT.md"}, d.files(), name)
			assert.Equal(t, v1File, d.read("HEARTBEAT.md"), name)
			assert.Contains(t, out, "would be kept", name)
		}
	}
}

func TestStatus_ShowsModeAndTemplate(t *testing.T) {
	ptr := func(s string) *string { return &s }
	for name, tc := range map[string]struct {
		content *string
		want    string
	}{
		"v1":      {ptr(v1File), "v1 template"},
		"v2":      {ptr(hb.TemplateV2()), "v2 template"},
		"own":     {ptr("- just a task\n"), "your own file"},
		"missing": {nil, "missing"},
	} {
		r := newLegacyRig(t)
		if tc.content != nil {
			r.write("HEARTBEAT.md", *tc.content)
		}
		cmd := newHeartbeatCommand(deps{
			configPath: func() string { return "unused" },
			load:       func(string) (*config.Config, error) { return r.cfg, nil },
		})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"status"})
		require.NoError(t, cmd.Execute(), name)
		assert.Contains(t, out.String(), "heartbeat mode: legacy", name)
		assert.Contains(t, out.String(), tc.want, name)
		if tc.content == nil {
			assert.Empty(t, r.files(), "status writes nothing")
		}
	}
}
