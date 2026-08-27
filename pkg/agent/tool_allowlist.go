package agent

import (
	"strings"
)

// resolveAgentMCPServerAllowlist returns a case-insensitive set of allowed MCP server names.
// If no allowlist is configured, it returns nil (allowing all servers).
// Server names are normalized to lowercase for matching.
func resolveAgentMCPServerAllowlist(mcpServers []string) map[string]struct{} {
	if len(mcpServers) == 0 {
		return nil
	}

	allowlist := make(map[string]struct{}, len(mcpServers))
	for _, raw := range mcpServers {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		allowlist[trimmed] = struct{}{}
	}

	if len(allowlist) == 0 {
		return nil
	}
	return allowlist
}
