package tools

import "strings"

// guardSessionInput applies the exec deny patterns, and (T1-2) the same
// workspace path checks guardCommand applies to a command about to run, to
// text being written into a live session.
//
// Why this exists: guardCommand runs once, on the command that starts a
// session. A session is a running shell, so everything written into it
// afterwards is executed too — with none of those checks. Upstream picoclaw
// guards only the starting command, which means starting a session with a
// permitted interpreter and then writing into it bypasses every deny pattern
// the fork has accumulated (PowerShell encoding guards, rm -rf forms, the
// deny-always-applies fix in PR #61).
//
// T1-2: the same reasoning applies to restrictToWorkspace. A session started
// inside the workspace can be handed "cat ../../secret\n" or "cat
// /etc/hosts\n" afterwards with none of guardCommand's path-traversal or
// outside-workspace checks applied, because those only ran once, on the
// command that started the session. checkWorkspaceViolation (shell.go) is
// the single implementation both guards now call, evaluated here against
// t.workingDir since a running session has no cwd argument of its own.
//
// Deny-pattern matching deliberately checks deny patterns only, not the
// allowlist. An allowlist describes whole commands a user sanctioned; session
// input is a fragment — a bare "y", a filename, a line continuation — and
// matching fragments against whole-command patterns would reject ordinary
// interactive use while adding no safety. Deny patterns describe things that
// must not run at all, which is the property that has to hold for a fragment
// as much as for a command.
func (t *ExecTool) guardSessionInput(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}

	denyLower := strings.ToLower(normalizeForDenyMatching(trimmed))
	for _, pattern := range t.denyPatterns {
		if pattern.MatchString(denyLower) {
			return "Session input blocked by safety guard (dangerous pattern detected)"
		}
	}

	if t.restrictToWorkspace && t.workingDir != "" {
		if reason := t.checkWorkspaceViolation(trimmed, t.workingDir); reason != "" {
			return "Session input blocked by safety guard (" + reason + ")"
		}
	}

	return ""
}

// ptyControlKeys are the named keys send-keys understands. They encode to
// terminal control sequences rather than literal text, so they carry no command
// content and are excluded before the remainder is guarded.
var ptyControlKeys = map[string]bool{
	"enter": true, "return": true, "tab": true, "escape": true, "esc": true,
	"space": true, "backspace": true, "delete": true, "del": true,
	"up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pageup": true, "pagedown": true,
	"insert": true,
}

// literalSendKeysText returns the portion of a send-keys token list that is sent
// as literal text, with named control keys removed.
func literalSendKeysText(keys []string) string {
	literal := make([]string, 0, len(keys))
	for _, k := range keys {
		bare := strings.ToLower(strings.TrimSpace(k))
		if bare == "" || ptyControlKeys[bare] {
			continue
		}
		if strings.HasPrefix(bare, "c-") || strings.HasPrefix(bare, "ctrl-") ||
			strings.HasPrefix(bare, "m-") || strings.HasPrefix(bare, "alt-") ||
			strings.HasPrefix(bare, "f") && len(bare) <= 3 {
			continue // modifier and function keys are control sequences too
		}
		literal = append(literal, k)
	}
	return strings.Join(literal, " ")
}
