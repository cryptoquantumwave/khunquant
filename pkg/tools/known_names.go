package tools

import "github.com/cryptoquantumwave/khunquant/pkg/config"

func init() {
	config.SetToolNameChecker(IsKnownToolName)
}

// IsKnownToolName reports whether name is a tool this build can have: every
// tool has a row in the classification table (a test fails otherwise), so the
// table is the list of known names. An MCP tool is not in it; the ceiling of a
// proactive turn denies those anyway.
func IsKnownToolName(name string) bool {
	return ClassOf(name) != ClassUnknown
}
