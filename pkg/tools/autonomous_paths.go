package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace paths an autonomous turn may not write, whatever restrict_to_workspace
// or the allow-paths say.
//
// The class ceiling (classification.go) keeps automation, trade and system tools
// away from a heartbeat or a scheduled job. Local-state tools are allowed there,
// and a file write is only "local" if the file is: several things the ceiling
// denies are driven by plain files in the workspace, so writing one of them
// from a background turn would be the denied action by another route. The list
// is the answer to "which workspace files act as state, code or prompt?" - when
// a new state directory or prompt file appears, add it here.
//
// Not listed on purpose: memory/*.md (including memory/MEMORY.md) and
// HEARTBEAT.md. The spec lets autonomous turns keep notes, and the heartbeat's
// alert ledger lives in MEMORY.md. They still influence later turns, which is
// an accepted residual (SECURITY.md) left to a later trust phase.
var autonomousProtectedPaths = []struct {
	// Path is relative to the workspace, slash-separated, lower case. A
	// directory protects everything under it.
	Path string
	// Why says what the path holds; shown in review, not to the model.
	Why string
}{
	{"cron", "the scheduler's store (jobs.json): a job with a command or a dca: name is automation and shell access"},
	{"state", "safety state such as daily_loss.json, which the loss brake reads"},
	{"state.json", "legacy runtime state file kept at the workspace root"},
	{"sessions", "user conversation histories and the context-engine database"},
	{"skills", "a file under skills/ is an installed skill, i.e. install_skill by another route"},
	{"memory/dca", "the DCA plan store (plans, schedules, execution history)"},
	{"memory/delta_neutral", "the delta-neutral plan store (plans, legs, history)"},
	{"memory/snapshots", "the portfolio snapshot store"},
	{"sandbox", "sandbox fixtures and the stored Webull session"},
	{"whatsapp", "the WhatsApp session store (credentials)"},
	{"pairing", "pending channel pairing requests (pairing/requests.json): access-control state, so a background turn must not plant or alter a pairing code"},
	{"agents.md", "system prompt of every user turn"},
	{"identity.md", "system prompt of every user turn"},
	{"soul.md", "system prompt of every user turn"},
	{"user.md", "system prompt of every user turn"},
}

// guardAutonomousWrite is the file-write gate for write_file, edit_file and
// append_file. For a turn a person started it returns the path unchanged and no
// refusal, so their behaviour is untouched. For an autonomous turn it
// returns the absolute path the tool must use - the path that was checked is the
// path that gets written - or a refusal.
//
// The target must resolve inside the workspace (restrict_to_workspace=false and
// allow-paths do not widen this) and must not be, or be under, a protected
// path. The path is cleaned and symlinks are resolved through the deepest
// existing ancestor before comparing, and the comparison ignores case, so a
// symlink into cron/ and CRON/jobs.json are both refused.
func guardAutonomousWrite(
	ctx context.Context,
	tool, workspace string,
	restrict bool,
	path string,
) (string, *ToolResult) {
	origin := ToolOrigin(ctx)
	if !IsAutonomousOrigin(origin) {
		return path, nil
	}
	outside := func() (string, *ToolResult) {
		return "", policyRefusal(tool, fmt.Sprintf(
			"autonomous (%s) turns may not write %q: it is outside the workspace", origin, path))
	}
	protected := func() (string, *ToolResult) {
		return "", policyRefusal(tool, fmt.Sprintf(
			"autonomous (%s) turns may not write %q: it holds scheduler / safety / prompt state", origin, path))
	}

	if workspace == "" || strings.TrimSpace(path) == "" {
		return outside()
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return outside()
	}
	absTarget := filepath.Clean(path)
	if !filepath.IsAbs(absTarget) {
		absTarget = filepath.Join(absWorkspace, absTarget)
	}

	realWorkspace, err := resolvePathAgainstExistingAncestor(absWorkspace)
	if err != nil {
		return outside()
	}
	realTarget, err := resolvePathAgainstExistingAncestor(absTarget)
	if err != nil && !os.IsNotExist(err) {
		return outside()
	}
	if err != nil || !isWithinWorkspace(realTarget, realWorkspace) {
		return outside()
	}

	rel, err := filepath.Rel(realWorkspace, realTarget)
	if err != nil {
		return outside()
	}
	rel = strings.ToLower(filepath.ToSlash(rel))
	for _, p := range autonomousProtectedPaths {
		if rel == p.Path || strings.HasPrefix(rel, p.Path+"/") {
			return protected()
		}
	}
	// A sandboxed filesystem over a relative workspace resolves paths against that
	// workspace and cannot take an absolute one; hand it the checked path in the
	// relative form. Everything else gets the absolute path, so a host
	// filesystem cannot read a relative path against the process's directory
	// instead of the workspace that was checked.
	if restrict && !filepath.IsAbs(workspace) {
		if rel, err := filepath.Rel(absWorkspace, absTarget); err == nil {
			return rel, nil
		}
		return outside()
	}
	return absTarget, nil
}
