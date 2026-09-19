package tools

// Child process environment for the exec tool.
//
// H-2: exec children used to inherit the gateway's entire environment, which is
// where this process keeps every LLM and exchange API key (see .env.example).
// Any command the model could run - `env`, `printenv`, `sh -c 'echo $OPENAI_API_KEY'`,
// or any program that reads credentials from the environment on its own - could
// therefore read secrets that no tool deliberately exposes. Nothing in the deny
// list stops that, and nothing can: the leak is in the process the shell starts,
// not in the text of the command.
//
// Children now start from an allowlist instead: the variables a shell needs to
// function, plus whatever the operator names in tools.exec.env_passthrough.

import (
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// The allowlist itself lives in utils.ChildEnvNameAllowed, shared with MCP
// stdio servers (M-5), so both kinds of child start from one definition.

// envNamesEqual compares environment variable names by the host's rule.
func envNamesEqual(a, b string) bool { return utils.EnvNamesEqual(a, b) }

// envNameAllowed reports whether a variable may be passed to a child.
func envNameAllowed(name string, passthrough []string) bool {
	return utils.ChildEnvNameAllowed(name, passthrough)
}

// buildChildEnv returns the environment for an exec child: the allowlisted
// subset of this process's environment, with HOME (and USERPROFILE on Windows)
// replaced by homeOverride when one is given.
//
// homeOverride is what keeps the workspace guard honest. When exec is
// restricted to the workspace, the guard resolves "~" to the workspace, so the
// shell has to resolve it the same way - otherwise "cat ~/x" means one path to
// the checker and another to the process, which is exactly the C-1 bypass.
// Setting HOME here makes the two agree by construction rather than by two
// implementations happening to match.
func buildChildEnv(passthrough []string, homeOverride string) []string {
	env := make([]string, 0, 16+len(passthrough))

	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if homeOverride != "" && isHomeEnvName(name) {
			continue // replaced below
		}
		if envNameAllowed(name, passthrough) {
			env = append(env, kv)
		}
	}

	if homeOverride != "" {
		env = append(env, "HOME="+homeOverride)
		if runtime.GOOS == "windows" {
			env = append(env, "USERPROFILE="+homeOverride)
		}
	}

	return env
}

// isHomeEnvName reports whether name is the variable buildChildEnv overrides
// with the workspace path.
func isHomeEnvName(name string) bool {
	if envNamesEqual(name, "HOME") {
		return true
	}
	return runtime.GOOS == "windows" && envNamesEqual(name, "USERPROFILE")
}

// C-1: the workspace guard inspects the command text, but the shell that runs
// it expands "~" and "$VAR" first. Anywhere the two disagree, the guard checks
// one path and the process opens another:
//
//	cat ~/.khunquant/config.json   # guard sees a relative-looking token
//	cat $HOME/.ssh/id_rsa          # guard sees no absolute path at all
//
// Both passed restrict_to_workspace, which is on by default. The expansion
// below ran on Windows only, while Unix - where commands go through `sh -c` -
// had none of it.
//
// The expansion deliberately uses the child's own environment (buildChildEnv),
// not this process's, so the value the guard resolves is the value the shell
// will see.

var (
	// tildeRefPattern matches "~" where a shell would treat it as a home
	// reference: at the start of a token. It captures the remainder of the
	// token so "~/x" and "~root/x" can be told apart. A trailing "~" inside a
	// word (backup~, a~b) is not a match and is left alone.
	tildeRefPattern = regexp.MustCompile(`(^|[\s"'=;&|:])~([^\s"';&|]*)`)

	// varRefPattern matches $NAME and ${NAME}. Note that ${NAME} is also
	// caught by a deny pattern, but deny patterns are disableable
	// (enable_deny_patterns: false) and the workspace boundary must hold
	// regardless, so the guard expands it on its own.
	varRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

	// expandableVars are the variables expanded to their value. HOME has to
	// be, because it names a directory the child may legitimately reach, so
	// "$HOME/../.." is a traversal the guard would otherwise not see.
	expandableVars = map[string]bool{
		"HOME":        true,
		"USERPROFILE": true,
	}
)

// expandShellRefs rewrites ~ and $VAR in command text the way the child shell
// would, using env (a KEY=VALUE slice as produced by buildChildEnv).
//
// It returns the expanded text and, when a reference cannot be resolved safely,
// a short reason the caller turns into a guard violation. An unset variable
// expands to "" exactly as the shell does, which turns "$NOPE/etc/passwd" into
// "/etc/passwd" - an absolute path the caller then checks, matching what the
// process would actually open.
func expandShellRefs(cmd string, env []string) (string, string) {
	lookup := func(name string) (string, bool) {
		prefix := name + "="
		for i := len(env) - 1; i >= 0; i-- {
			if value, ok := strings.CutPrefix(env[i], prefix); ok {
				return value, true
			}
		}
		return "", false
	}

	home, _ := lookup("HOME")

	var reason string
	expanded := tildeRefPattern.ReplaceAllStringFunc(cmd, func(match string) string {
		groups := tildeRefPattern.FindStringSubmatch(match)
		lead, rest := groups[1], groups[2]

		// "~otheruser/..." resolves through the password database to a home
		// directory this guard cannot predict. Refuse rather than guess.
		if rest != "" && !strings.HasPrefix(rest, "/") {
			reason = "unsupported home reference"
			return match
		}
		if home == "" {
			reason = "home directory reference with no HOME set"
			return match
		}
		return lead + home + rest
	})
	if reason != "" {
		return cmd, reason
	}

	expanded = varRefPattern.ReplaceAllStringFunc(expanded, func(match string) string {
		groups := varRefPattern.FindStringSubmatch(match)
		name := groups[1]
		if name == "" {
			name = groups[2]
		}
		value, isSet := lookup(name)
		switch {
		case expandableVars[name]:
			return value

		case !isSet:
			// The shell expands an unset variable to nothing, which can turn
			// a relative-looking token into an absolute path:
			//
			//	cat $NOT_SET/etc/passwd   ->   cat /etc/passwd
			//
			// The guard has to do the same or it checks a path inside the
			// workspace while the process opens one outside it. The model
			// chooses the variable name, so it can always pick an unset one.
			return ""

		default:
			// A variable that is set and is not HOME is left literal. Its
			// value comes from the scrubbed allowlist, so it holds no secret,
			// and expanding it would refuse ordinary commands - "echo $PATH"
			// resolves to a list of absolute system paths that all sit
			// outside the workspace.
			return match
		}
	})

	return expanded, ""
}

// checkDirectoryChange refuses directory changes whose target this guard cannot
// account for.
//
//   - "cd -" always: its target is OLDPWD, which the guard cannot see and which
//     may sit outside the workspace.
//   - bare "cd", "cd ~" and "cd $HOME" only when no workspace is configured.
//
// The second case is the subtle one. Those three all land in HOME, and when a
// workspace is set, a restricted child's HOME *is* the workspace
// (buildChildEnv), so they move to the workspace root - harmless, and refusing
// them would break ordinary use. With workingDir empty there is no override, so
// the child keeps the real home while the guard goes on resolving relative
// paths against cwd: "cd && cat .ssh/id_rsa" would check a path in cwd and read
// one in the user's home. The HOME override is what makes those commands safe,
// so they are allowed exactly when it is in effect.
//
// Relative escapes like "cd ../.." remain the traversal pattern's job.
func (t *ExecTool) checkDirectoryChange(cmd string) string {
	homeIsWorkspace := t.workingDir != ""

	for _, segment := range splitShellSegments(cmd) {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		if fields[0] != "cd" && fields[0] != "pushd" {
			continue
		}

		if len(fields) == 1 {
			if !homeIsWorkspace {
				return "directory change to the home directory"
			}
			continue
		}

		switch fields[1] {
		case "-":
			return "directory change to an unknown previous directory"
		case "~", "$HOME", "${HOME}":
			if !homeIsWorkspace {
				return "directory change to the home directory"
			}
		}
	}
	return ""
}

// splitShellSegments splits a command into the pieces a shell would run in
// sequence, so a guard can inspect each one's first word.
func splitShellSegments(cmd string) []string {
	return strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r':
			return true
		}
		return false
	})
}

// childEnv returns the environment for a command this tool is about to start.
// Both the one-shot path (executeRun) and the session path (runBackground) use
// it, so a session cannot be the way around the allowlist.
func (t *ExecTool) childEnv() []string {
	home := ""
	if t.restrictToWorkspace && t.workingDir != "" {
		home = t.workingDir
	}
	return buildChildEnv(t.envPassthrough, home)
}
