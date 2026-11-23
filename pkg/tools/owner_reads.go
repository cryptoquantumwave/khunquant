package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ownerReadExempt names the protected entries a person who is not the owner may still
// read: they are public by design (load_skill is a public read tool and hands out the
// same files), so the owner-only read rule leaves them alone. Everything else on
// autonomousProtectedPaths - sessions, state, every memory store, the prompt files
// (which may hold the owner's profile), the scheduler, pairing and credential
// stores - is the owner's private state.
var ownerReadExempt = map[string]bool{"skills": true}

// guardOwnerPrivateRead is the one gate every tool that reads a model-chosen workspace
// path goes through (read_file, list_dir, send_file): a new path-reading tool calls it
// too (TestPathReadingToolsAreGuarded lists them).
//
// It is an allow-list, not a deny-list. While an owner is configured, a person whose
// turn is not the owner's (nonOwnerPersonTurn: any route, DM or group, sub-turns
// included) may read only a path that resolves INSIDE the workspace and is not on
// autonomousProtectedPaths (minus ownerReadExempt). A path outside the workspace is
// refused too: that is where config.json and the credential stores live, and the
// read tools' allow-read-paths are the owner's convenience, not a non-owner's.
// It returns nil when the read may go ahead.
//
// Unchanged: the owner's own turns, background and autonomous turns (heartbeat, cron,
// proactive: they run as the owner's agent, and checkOwnerTurn calls them
// ownerBackground, not ownerOther) and every turn when no owner is configured.
//
// restrict is how the tool resolves a relative path: against the workspace (a
// sandboxed tool) or against the process's directory (an unrestricted host
// filesystem, which is how a "workspace/state/..." form reaches the workspace when the
// process runs in its parent). The path that is checked is the path the tool opens.
func guardOwnerPrivateRead(ctx context.Context, tool, workspace string, restrict bool, path string) *ToolResult {
	if !nonOwnerPersonTurn(ctx) {
		return nil
	}
	if ownerPrivatePath(workspace, restrict, path) {
		return ownerReadRefusal(tool, path)
	}
	return nil
}

// filterOwnerPrivateEntries drops the owner-private entries from a listing of dir for a
// non-owner's turn, so a listing of the workspace root does not name sessions/ or
// memory/. For any other turn it returns the entries unchanged.
func filterOwnerPrivateEntries(
	ctx context.Context,
	workspace string,
	restrict bool,
	dir string,
	entries []os.DirEntry,
) []os.DirEntry {
	if !nonOwnerPersonTurn(ctx) {
		return entries
	}
	kept := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !ownerPrivatePath(workspace, restrict, filepath.Join(dir, e.Name())) {
			kept = append(kept, e)
		}
	}
	return kept
}

// ownerPrivatePath reports whether path, as the tool would open it, is not readable by a
// non-owner. It fails closed: an empty workspace, an unresolvable path or a path outside
// the workspace is private. A path that does not exist yet is judged by its deepest
// existing ancestor, so the answer does not reveal whether it exists.
func ownerPrivatePath(workspace string, restrict bool, path string) bool {
	if workspace == "" || strings.TrimSpace(path) == "" {
		return true
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return true
	}
	realWorkspace, err := resolvePathAgainstExistingAncestor(absWorkspace)
	if err != nil {
		return true
	}
	target := filepath.Clean(path)
	if !filepath.IsAbs(target) {
		base := absWorkspace
		if !restrict {
			if base, err = os.Getwd(); err != nil {
				return true
			}
		}
		target = filepath.Join(base, target)
	}
	realTarget, err := resolvePathAgainstExistingAncestor(target)
	if err != nil || !isWithinWorkspace(realTarget, realWorkspace) {
		return true
	}
	rel, err := filepath.Rel(realWorkspace, realTarget)
	if err != nil {
		return true
	}
	rel = strings.ToLower(filepath.ToSlash(rel))
	i := protectedPathIndex(rel)
	if i < 0 {
		return false
	}
	return !ownerReadExempt[autonomousProtectedPaths[i].Path]
}

// ownerReadRefusal is the text a refused read gets. It says the path is owner-only and
// repeats only the path the person asked for.
func ownerReadRefusal(tool, path string) *ToolResult {
	return policyRefusal(tool, fmt.Sprintf(
		"this turn may not read %q: an owner is configured, and the owner's conversations, memory, state and "+
			"settings are owner-only. Nothing was read. Tell the person to ask the owner.", path))
}
