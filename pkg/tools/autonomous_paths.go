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
// memory/ as a whole is on the list (below the stores that live in it, which are
// listed first so that their refusal says what they hold), and so is HEARTBEAT.md:
// MEMORY.md and the notes are what later turns trust, so a background turn that could
// write them could plant a "fact" a later trade would act on, and a heartbeat that
// could rewrite HEARTBEAT.md could rewrite its own instructions. A background turn may
// SUGGEST a memory with the remember tool; the owner confirms it (remember.go). The
// memory entry is also OwnerOnly: while an owner is configured, a turn a person started
// writes memory only if it is the owner's own conversation.
var autonomousProtectedPaths = []struct {
	// Path is relative to the workspace, slash-separated, lower case. A
	// directory protects everything under it.
	Path string
	// Why says what the path holds; shown in review, not to the model.
	Why string
	// OwnerOnly additionally refuses a turn a person started unless it is the
	// owner's own root conversation turn, while an owner is configured. With no
	// owner configured nothing is refused to a person's turn: every default install
	// writes as it always did.
	OwnerOnly bool
}{
	{Path: "cron", Why: "the scheduler's store (jobs.json): a job with a command or a dca: name is automation and shell access"},
	{Path: "state", Why: "safety state such as daily_loss.json, which the loss brake reads"},
	{Path: "state.json", Why: "legacy runtime state file kept at the workspace root"},
	{Path: "sessions", Why: "user conversation histories and the context-engine database"},
	{Path: "skills", Why: "a file under skills/ is an installed skill, i.e. install_skill by another route"},
	{Path: "memory/dca", Why: "the DCA plan store (plans, schedules, execution history)"},
	{Path: "memory/delta_neutral", Why: "the delta-neutral plan store (plans, legs, history)"},
	{Path: "memory/snapshots", Why: "the portfolio snapshot store"},
	{Path: "memory/proactive", Why: "the proactive-agent store (proactive.db): attention budgets, intents, proposals and the journal that gate what the agent says and does unprompted"},
	{Path: "memory", Why: "the agent's memory (MEMORY.md and notes): later turns trust it, so only the owner's confirmation may add to it", OwnerOnly: true},
	{Path: "heartbeat.md", Why: "the heartbeat's own instructions: a background turn must not rewrite them"},
	{Path: "sandbox", Why: "sandbox fixtures and the stored Webull session"},
	{Path: "whatsapp", Why: "the WhatsApp session store (credentials)"},
	{Path: "pairing", Why: "pending channel pairing requests (pairing/requests.json): access-control state, so a background turn must not plant or alter a pairing code"},
	{Path: "calendar", Why: "market holiday lists (set_holidays.json, us_holidays.json): the clock source gates market evaluation on them, so a background turn that could write one could switch evaluation off on a trading day"},
	{Path: "agents.md", Why: "system prompt of every user turn"},
	{Path: "identity.md", Why: "system prompt of every user turn"},
	{Path: "soul.md", Why: "system prompt of every user turn"},
	{Path: "user.md", Why: "system prompt of every user turn"},
}

// The phrases of the two heartbeat templates (pkg/heartbeat: V1Marker and V2Marker,
// which a test there holds equal to these; tools cannot import heartbeat). A
// HEARTBEAT.md that has the first and not the second is the v1 template, which keeps
// an "alert ledger" in memory/MEMORY.md.
const (
	HeartbeatV1Marker = "How delivery actually works"
	HeartbeatV2Marker = "khunquant:heartbeat-template v2"
)

// rememberHint is the end of every refusal of a memory write.
const rememberHint = "Use `remember` to suggest a memory; the owner confirms it."

// heartbeatUpgradeHint is added to a legacy heartbeat's refusal when its HEARTBEAT.md is
// the v1 template: that template tells the model to keep its ledger in MEMORY.md.
const heartbeatUpgradeHint = " This heartbeat's HEARTBEAT.md is the v1 template, which keeps an alert ledger in " +
	"memory/MEMORY.md; run `khunquant heartbeat upgrade-template` to switch to the v2 template, which does not."

// isV1HeartbeatFile reports whether the workspace's HEARTBEAT.md is the v1 template.
func isV1HeartbeatFile(workspace string) bool {
	b, err := os.ReadFile(filepath.Join(workspace, "HEARTBEAT.md"))
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, HeartbeatV1Marker) && !strings.Contains(s, HeartbeatV2Marker)
}

// guardAutonomousWrite is the file-write gate for write_file, edit_file and
// append_file. For a turn a person started it returns the path unchanged and no
// refusal, except for a path under an OwnerOnly entry (memory/) while an owner is
// configured and the turn is not the owner's own root turn. For an autonomous turn it
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
	autonomous := IsAutonomousOrigin(origin)
	if !autonomous && !memoryOwnerOnly(ctx) {
		return path, nil
	}
	outside := func() (string, *ToolResult) {
		if !autonomous {
			return path, nil // not the business of the owner-only rule: the tool's own checks apply
		}
		return "", policyRefusal(tool, fmt.Sprintf(
			"autonomous (%s) turns may not write %q: it is outside the workspace", origin, path))
	}
	protected := func(why string, isMemory bool) (string, *ToolResult) {
		switch {
		case !autonomous:
			return "", policyRefusal(tool, fmt.Sprintf(
				"this turn may not write %q: an owner is configured, and memory is written only in the owner's own "+
					"conversation. %s", path, rememberHint))
		case isMemory:
			hint := ""
			if origin == OriginHeartbeat && isV1HeartbeatFile(workspace) {
				hint = heartbeatUpgradeHint
			}
			return "", policyRefusal(tool, fmt.Sprintf(
				"autonomous (%s) turns may not write %q: it is the agent's memory, which later turns trust. %s%s",
				origin, path, rememberHint, hint))
		}
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
			if !autonomous && !p.OwnerOnly {
				continue
			}
			return protected(p.Why, p.OwnerOnly)
		}
	}
	if !autonomous {
		return path, nil
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

// memoryOwnerOnly reports whether the OwnerOnly rule applies to this person's turn:
// an owner is configured and the turn is not the owner's own root conversation turn.
// An untagged turn is not the owner's (fail closed). With no config armed, or no owner
// configured, it is false: nothing changes for an install that never set an owner.
func memoryOwnerOnly(ctx context.Context) bool {
	cfg := armedConfig.Load()
	if cfg == nil {
		return false
	}
	if _, ok := cfg.Proactive.OwnerRoute(); !ok {
		return false
	}
	return !isOwnerRootTurn(ctx, cfg)
}
