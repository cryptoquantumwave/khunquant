package heartbeat

import "strings"

// v1DefaultTask is the one task the v1 template shipped with. A file whose tasks
// are exactly this was never customised, so its upgrade takes the v2 default task
// instead of carrying a line written for the v1 file.
const v1DefaultTask = "- Run the full portfolio + market scan above and `message` me only on what is " +
	"genuinely material, following the guardrails, dedup ledger, and escalation rules."

// UpgradeOutcome says what UpgradeToV2 did with the tasks in the old file.
type UpgradeOutcome int

const (
	// TasksKept: what the owner wrote under the tasks marker is carried over unchanged.
	TasksKept UpgradeOutcome = iota
	// TasksNone: the old file has nothing to carry, so the new file has no tasks
	// either and runs nothing until one is added (as the old one did).
	TasksNone
	// TasksWereShippedDefault: the tasks were the v1 template's own default task,
	// so the new file has the v2 template's default task.
	TasksWereShippedDefault
	// TasksWholeFile: the old file has no tasks marker (it was heavily edited), so
	// all of its content is kept under the new file's tasks section.
	TasksWholeFile
)

// UpgradeToV2 returns the v2 file that replaces old, and what became of old's
// task list. The instructions come from the embedded template; what the owner
// wrote under the tasks marker is carried over. It does no I/O.
//
// An old file with no marker has its whole content kept under the new tasks
// section rather than dropped; it may then still hold v1 instructions, so the
// caller says so and points at the backup.
func UpgradeToV2(old string) (string, UpgradeOutcome) {
	idle := idleTemplateV2()
	old = strings.ReplaceAll(old, "\r\n", "\n")

	tasks, outcome := "", TasksKept
	if i := strings.Index(old, userTasksMarker); i >= 0 {
		tasks = strings.TrimSpace(old[i+len(userTasksMarker):])
	} else {
		tasks, outcome = strings.TrimSpace(old), TasksWholeFile
	}

	switch {
	case tasks == "":
		return idle, TasksNone
	case outcome == TasksKept && sameWords(tasks, v1DefaultTask):
		return templateV2, TasksWereShippedDefault
	}
	return idle + "\n" + tasks + "\n", outcome
}

// sameWords reports whether a and b are equal once runs of whitespace (including
// the line breaks of a wrapped task) are collapsed.
func sameWords(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}
