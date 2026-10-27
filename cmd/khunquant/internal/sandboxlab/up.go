package sandboxlab

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

func newUpCommand() *cobra.Command {
	var (
		opts    supervisor.UpOptions
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start a locked sandbox gateway in its own HOME",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true // a failed start is not a usage error
			if runtime.GOOS == "windows" {
				return supervisor.ErrUnixOnly
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			inst, err := supervisor.Up(ctx, opts)
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
