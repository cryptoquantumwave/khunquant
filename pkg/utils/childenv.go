package utils

// Child process environment allowlist.
//
// H-2 / M-5: child processes used to inherit this process's entire environment,
// which is where every LLM and exchange API key lives (see .env.example). A
// child - an exec command, or an MCP server that is third-party code - could
// read secrets that no tool deliberately exposes. Children start from this
// allowlist instead: what a program needs to run, plus what the operator names.

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

// EnvNamesEqual compares two environment variable names using the host's own
// rule: Windows environment variables are case-insensitive, Unix ones are not.
func EnvNamesEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ChildEnvNameAllowed reports whether a variable may be passed to a child
// process (an exec command or an MCP stdio server).
//
// An entry in passthrough may end in "*" to allow a prefix, which is how the
// LC_* locale family is expressed and how an operator can pass, say, a whole
// CI_* group without listing each one.
func ChildEnvNameAllowed(name string, passthrough []string) bool {
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
		if EnvNamesEqual(name, allowed) {
			return true
		}
	}

	if runtime.GOOS == "windows" {
		for _, allowed := range windowsEnvAllowlist {
			if EnvNamesEqual(name, allowed) {
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
		if EnvNamesEqual(name, entry) {
			return true
		}
	}

	return false
}

// FilterEnviron returns the entries of this process's environment that
// ChildEnvNameAllowed admits, in os.Environ order, and the names it withheld.
func FilterEnviron(passthrough []string) (kept []string, withheld []string) {
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if ChildEnvNameAllowed(name, passthrough) {
			kept = append(kept, kv)
		} else {
			withheld = append(withheld, name)
		}
	}
	return kept, withheld
}
