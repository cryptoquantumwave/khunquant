// Package sandboxlab is the `khunquant sandbox` command: it starts and stops locked
// sandbox instances of the gateway, each with its own HOME, config, fixtures and
// token, so a gateway that cannot reach a real exchange, chat service or LLM can be
// driven by an agent or a developer. The process work lives in pkg/sandbox/supervisor.
package sandboxlab

import "github.com/spf13/cobra"

// NewSandboxCommand returns the `khunquant sandbox` command with `up` and `down`.
func NewSandboxCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "sandbox", Short: "Start and stop locked sandbox instances of the gateway"}
	cmd.AddCommand(newUpCommand(), newDownCommand())
	return cmd
}
