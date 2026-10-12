package heartbeat

import (
	_ "embed"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Contract is how a heartbeat turn reports what it found, and so which
// HEARTBEAT.md the model needs.
type Contract int

const (
	// ContractLegacy is the v1 contract: the model calls the `message` tool to
	// reach the user, to whichever chat was last active.
	ContractLegacy Contract = iota
	// ContractGoverned is the v2 contract: the model declares topics with
	// respond_proactive and the runtime decides and delivers to the owner.
	ContractGoverned
)

func (c Contract) String() string {
	if c == ContractGoverned {
		return "governed"
	}
	return "legacy"
}

// V1Marker is a phrase only the v1 template contains ("How delivery actually
// works"): the section that tells the model to call `message`.
const V1Marker = "How delivery actually works"

// V2Marker is the phrase of the marker line the v2 template starts with.
const V2Marker = "khunquant:heartbeat-template v2"

// templateV2 is the v2 HEARTBEAT.md: the only copy of its text. New installs are
// not given it (onboarding copies workspace/HEARTBEAT.md, the v1 file, because
// the default heartbeat mode is legacy); `khunquant heartbeat upgrade-template`
// writes it, and a governed service recreates a missing file from it.
//
//go:embed template_v2.md
var templateV2 string

// TemplateV2 returns the full v2 template, as upgrade-template writes it.
func TemplateV2() string { return templateV2 }

// IsV1Template reports whether content is (or still contains) the v1 template's
// delivery instructions.
func IsV1Template(content string) bool { return strings.Contains(content, V1Marker) }

// IsV2Template reports whether content carries the v2 marker line.
func IsV2Template(content string) bool { return strings.Contains(content, V2Marker) }

// idleTemplateV2 is the v2 template up to and including the line that separates
// instructions from the user's tasks, so that a missing file is recreated as a
// file that runs nothing until the user adds a task - the same as the legacy
// default. It is cut from the embedded template, so the text has one source.
func idleTemplateV2() string {
	i := strings.Index(templateV2, userTasksMarker)
	if i < 0 {
		return templateV2
	}
	return templateV2[:i+len(userTasksMarker)] + "\n"
}

// legacyDefaultTemplate is what a legacy-contract service writes when
// HEARTBEAT.md is missing.
const legacyDefaultTemplate = `# Heartbeat Check List

This file contains tasks for the heartbeat service to check periodically.

## Examples

- Check for unread messages
- Review upcoming calendar events
- Check device status (e.g., MaixCam)

## Instructions

- Execute ALL tasks listed below. Do NOT skip any task.
- For simple tasks (e.g., report current time), respond directly.
- For complex tasks that may take time, use the spawn tool to create a subagent.
- The spawn tool is async - subagent results will be sent to the user automatically.
- After spawning a subagent, CONTINUE to process remaining tasks.
- Only respond with HEARTBEAT_OK when ALL tasks are done AND nothing needs attention.

---

Add your heartbeat tasks below this line:
`

// defaultTemplate is the file written when HEARTBEAT.md is missing: the one that
// matches the contract, so a governed install never gets v1 instructions.
func defaultTemplate(c Contract) string {
	if c == ContractGoverned {
		return idleTemplateV2()
	}
	return legacyDefaultTemplate
}

// TemplateAdvisory returns the warning to give when content does not match the
// contract c, or "" when it does (or when there is nothing to say). It never
// changes the file: a user's HEARTBEAT.md is theirs.
//
//   - governed with a v1 file: the turn still works, because the proactive
//     contract the runtime puts ahead of the file tells the model to use
//     respond_proactive and `message` is not available; the file's own
//     instructions are wrong for it and cost tokens.
//   - legacy with a v2 file: the file tells the model to call respond_proactive,
//     which a legacy turn does not have, so nothing would reach the user.
func TemplateAdvisory(content string, c Contract) string {
	switch {
	case c == ContractGoverned && IsV1Template(content) && !IsV2Template(content):
		return "HEARTBEAT.md is the v1 template: it tells the model to call the `message` tool, " +
			"which a governed heartbeat does not have. The heartbeat still works and its decisions are " +
			"delivered through attention, but the file's instructions are out of date. " +
			"Run `khunquant heartbeat upgrade-template` to back it up as HEARTBEAT.md.v1.bak and switch to v2"
	case c == ContractLegacy && IsV2Template(content):
		return "HEARTBEAT.md is the v2 template but the heartbeat is in legacy mode: the file tells the model " +
			"to call `respond_proactive`, which a legacy heartbeat does not have, so until this is fixed " +
			"heartbeats run and deliver nothing. Restore HEARTBEAT.md.v1.bak if you have it, otherwise " +
			"delete HEARTBEAT.md to get the default back (idle until you add tasks), or set " +
			"proactive.heartbeat.mode to governed (needs proactive enabled, an owner and attention.mode live). " +
			"HEARTBEAT.md has not been changed"
	}
	return ""
}

// warnLog is the sink for TemplateAdvisory warnings; tests replace it.
var warnLog = func(msg string, fields map[string]any) { logger.WarnCF("heartbeat", msg, fields) }
