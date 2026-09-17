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
	"runtime"
	"strings"
)

// baseEnvAllowlist is the set every child gets: enough for a shell to find
// programs, write temporary files, and render text correctly. Deliberately
// excludes anything that carries credentials.
var baseEnvAllowlist = []string{
	"PATH",
	"HOME",
	"TMPDIR",
	"TMP",
	"TEMP",
	"LANG",
	"TZ",
	"USER",
	"LOGNAME",
	"SHELL",
	"TERM",
}

// windowsEnvAllowlist holds the variables a Windows child additionally needs;
// PowerShell will not start without SystemRoot, and many tools resolve paths
// through the profile directories.
var windowsEnvAllowlist = []string{
	"SystemRoot",
	"SystemDrive",
	"ComSpec",
	"PATHEXT",
	"USERPROFILE",
	"APPDATA",
	"LOCALAPPDATA",
	"ProgramData",
	"ProgramFiles",
	"ProgramFiles(x86)",
	"windir",
	"NUMBER_OF_PROCESSORS",
	"PROCESSOR_ARCHITECTURE",
	"OS",
}

// envNamesEqual compares two environment variable names using the host's own
// rule: Windows environment variables are case-insensitive, Unix ones are not.
func envNamesEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// envNameAllowed reports whether a variable may be passed to a child.
//
// An entry in passthrough may end in "*" to allow a prefix, which is how the
// LC_* locale family is expressed and how an operator can pass, say, a whole
// CI_* group without listing each one.
func envNameAllowed(name string, passthrough []string) bool {
	if name == "" {
		return false
	}

	// Locale variables are a family rather than a fixed list (LC_ALL,
	// LC_CTYPE, LC_NUMERIC, ...); all of them are safe and omitting them
	// makes tools emit mojibake or sort inconsistently.
	if strings.HasPrefix(name, "LC_") {
		return true
	}

	for _, allowed := range baseEnvAllowlist {
		if envNamesEqual(name, allowed) {
			return true
		}
	}

	if runtime.GOOS == "windows" {
		for _, allowed := range windowsEnvAllowlist {
			if envNamesEqual(name, allowed) {
				return true
			}
		}
	}

	for _, entry := range passthrough {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, ok := strings.CutSuffix(entry, "*"); ok {
			if prefix == "" {
				// A bare "*" would restore the full inherited environment and
				// with it the leak this allowlist exists to close. Ignored on
				// purpose: an operator who truly wants a variable can name it.
				continue
			}
			if runtime.GOOS == "windows" {
				if strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
					return true
				}
			} else if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if envNamesEqual(name, entry) {
			return true
		}
	}

	return false
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
	env := make([]string, 0, len(baseEnvAllowlist)+len(passthrough))

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
