package heartbeat

import (
	"strings"
	"testing"
)

const v1Header = "# Heartbeat\n## How delivery actually works\nuse `message`\n\n---\n\n"

func TestUpgradeToV2(t *testing.T) {
	idle := idleTemplateV2()
	for name, tc := range map[string]struct {
		old     string
		want    string
		outcome UpgradeOutcome
	}{
		"owner's tasks are carried over": {
			old:     v1Header + userTasksMarker + "\n\n- check SOL\n- weekly review on Sunday\n",
			want:    idle + "\n- check SOL\n- weekly review on Sunday\n",
			outcome: TasksKept,
		},
		"surrounding blank lines are normalised": {
			old:     v1Header + userTasksMarker + "\n\n\n\n- check SOL\n\n\n",
			want:    idle + "\n- check SOL\n",
			outcome: TasksKept,
		},
		"CRLF files are accepted": {
			old:     v1Header + userTasksMarker + "\r\n\r\n- check SOL\r\n",
			want:    idle + "\n- check SOL\n",
			outcome: TasksKept,
		},
		"no tasks stays idle": {
			old:     v1Header + userTasksMarker + "\n",
			want:    idle,
			outcome: TasksNone,
		},
		"the shipped v1 default task becomes the v2 default": {
			old: v1Header + userTasksMarker + "\n\n- Run the full portfolio + market scan above and `message` me only on what is\n" +
				"  genuinely material, following the guardrails, dedup ledger, and escalation rules.\n",
			want:    TemplateV2(),
			outcome: TasksWereShippedDefault,
		},
		"no marker keeps the whole file under the tasks": {
			old:     "# My own heartbeat\n\n- check SOL\n- check BTC\n",
			want:    idle + "\n# My own heartbeat\n\n- check SOL\n- check BTC\n",
			outcome: TasksWholeFile,
		},
		"an empty file stays idle": {
			old:     "  \n",
			want:    idle,
			outcome: TasksNone,
		},
	} {
		got, outcome := UpgradeToV2(tc.old)
		if got != tc.want || outcome != tc.outcome {
			t.Errorf("%s:\noutcome %d, want %d\ngot  %q\nwant %q", name, outcome, tc.outcome, tail(got), tail(tc.want))
		}
		if !IsV2Template(got) || IsV1Template(got) && outcome != TasksWholeFile {
			t.Errorf("%s: the result is not a clean v2 file", name)
		}
		// Upgrading twice changes nothing more.
		if again, _ := UpgradeToV2(got); again != got && outcome != TasksWholeFile {
			t.Errorf("%s: a second upgrade changed the file", name)
		}
	}
}

// A task list the owner wrote must be the tasks of the result: the service runs
// the new file if and only if there is something to run.
func TestUpgradeToV2_ResultRunsExactlyWhenThereAreTasks(t *testing.T) {
	for old, wantRuns := range map[string]bool{
		v1Header + userTasksMarker + "\n- check SOL\n": true,
		v1Header + userTasksMarker + "\n":              false,
		"- only tasks, no marker\n":                    true,
	} {
		got, _ := UpgradeToV2(old)
		if heartbeatHasUserTasks(got) != wantRuns {
			t.Errorf("%q: the upgraded file runs=%v, want %v", old, heartbeatHasUserTasks(got), wantRuns)
		}
	}
}

func tail(s string) string {
	if len(s) > 120 {
		return "..." + s[len(s)-120:]
	}
	return strings.TrimSpace(s)
}
