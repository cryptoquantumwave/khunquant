package sandboxlab

import (
	"encoding/json"
	"io"
	"os"
)

// mcpServerName is the name a client lists the supervisor under; .mcp.json uses the same.
const mcpServerName = "khunquant-sandbox"

// mcpConfig is the MCP client entry that starts `<binary> sandbox mcp`: the shape of the
// repository's .mcp.json, for clients (Cursor, Claude Desktop) that need their own copy.
// It carries no environment and no credential; the supervisor locks every instance itself.
func mcpConfig(binary string) ([]byte, error) {
	type entry struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	b, err := json.MarshalIndent(map[string]map[string]entry{
		"mcpServers": {mcpServerName: {Command: binary, Args: []string{"sandbox", "mcp"}}},
	}, "", "  ")
	return append(b, '\n'), err
}

// printMCPConfig writes the entry for the running binary.
func printMCPConfig(w io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	b, err := mcpConfig(exe)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
