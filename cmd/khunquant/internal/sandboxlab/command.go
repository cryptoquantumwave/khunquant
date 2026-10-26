// Package sandboxlab is the `khunquant sandbox` command: it starts and stops locked
// sandbox instances of the gateway, each with its own HOME, config, fixtures and
// token, so a gateway that cannot reach a real exchange, chat service or LLM can be
// driven by an agent or a developer.
package sandboxlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"
)

// errUnixOnly is returned on platforms without process groups and POSIX signals.
var errUnixOnly = errors.New("khunquant sandbox up/down need a unix system (process groups and signals)")

// NewSandboxCommand returns the `khunquant sandbox` command with `up` and `down`.
func NewSandboxCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "sandbox", Short: "Start and stop locked sandbox instances of the gateway"}
	cmd.AddCommand(newUpCommand(), newDownCommand())
	return cmd
}

func newUpCommand() *cobra.Command {
	var (
		opts    UpOptions
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start a locked sandbox gateway in its own HOME",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true // a failed start is not a usage error
			if runtime.GOOS == "windows" {
				return errUnixOnly
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			inst, err := Up(ctx, opts)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			if !jsonOut {
				enc.SetIndent("", "  ")
			}
			return enc.Encode(inst) // the only place the token is ever shown
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "Instance name (default: random)")
	cmd.Flags().StringVar(&opts.FixturesFrom, "fixtures-from", "",
		"Directory holding <venue>/fixtures.json (default: ./workspace/sandbox)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the instance as one line of JSON (default: indented)")
	return cmd
}

func newDownCommand() *cobra.Command {
	var opts DownOptions
	cmd := &cobra.Command{
		Use:   "down <name>",
		Short: "Stop a sandbox instance and remove its home",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runtime.GOOS == "windows" {
				return errUnixOnly
			}
			opts.Name = args[0]
			if err := Down(opts); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Sandbox %q is down\n", opts.Name)
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.KeepHome, "keep-home", false, "Keep the instance home (logs, config) after stopping")
	return cmd
}
