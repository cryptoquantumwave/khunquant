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

// templateV2 is the v2 HEARTBEAT.md: the only copy of its text, and the file every
// heartbeat mode runs on. It tells the model to report with respond_proactive when
// it has that tool (a governed or sweep heartbeat) and to send one short `message`
// when it does not (a legacy heartbeat). workspace/HEARTBEAT.md, which onboarding
// copies to new installs, is a byte-identical copy (a test holds them together);
// `khunquant heartbeat upgrade-template` moves an existing install to it, and a
// missing file is recreated from it in every mode.
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

// TemplateAdvisory returns what to tell the operator when content is the v1
// template, and whether it is a warning (true) or only a notice (false); it
// returns "" when there is nothing to say. It never changes the file: a user's
// HEARTBEAT.md is theirs.
//
//   - governed or sweep with a v1 file (a warning): the turn still works, because
//     the proactive contract the runtime puts ahead of the file tells the model to
//     use respond_proactive and `message` is not available; the file's own
//     instructions are wrong for it and cost tokens.
//   - legacy with a v1 file (a notice): it keeps working exactly as before; the v2
//     file is shorter and also works in legacy mode.
//
// A v2 file is right under every contract, so it has no advisory.
func TemplateAdvisory(content string, c Contract) (msg string, warn bool) {
	if !IsV1Template(content) || IsV2Template(content) {
		return "", false
	}
	if c == ContractGoverned {
		return "HEARTBEAT.md is the v1 template: it tells the model to call the `message` tool, " +
			"which a governed heartbeat does not have. The heartbeat still works and its decisions are " +
			"delivered through attention, but the file's instructions are out of date. " +
			"Run `khunquant heartbeat upgrade-template` to back it up as HEARTBEAT.md.v1.bak and switch to v2 " +
			"(your tasks are kept)", true
	}
	return "HEARTBEAT.md is the v1 template. It keeps working as it is, and has not been changed. " +
		"The v2 template is shorter and works in every heartbeat mode: run " +
		"`khunquant heartbeat upgrade-template` to back this file up as HEARTBEAT.md.v1.bak and switch to it " +
		"(your tasks are kept)", false
}

// warnLog and infoLog are the sinks for TemplateAdvisory; tests replace them.
var (
	warnLog = func(msg string, fields map[string]any) { logger.WarnCF("heartbeat", msg, fields) }
	infoLog = func(msg string, fields map[string]any) { logger.InfoCF("heartbeat", msg, fields) }
)
