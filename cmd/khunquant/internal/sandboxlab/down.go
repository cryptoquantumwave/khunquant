package sandboxlab

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

func newDownCommand() *cobra.Command {
	var opts supervisor.DownOptions
	cmd := &cobra.Command{
		Use:   "down <name>",
		Short: "Stop a sandbox instance and remove its home",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runtime.GOOS == "windows" {
				return supervisor.ErrUnixOnly
			}
			opts.Name = args[0]
			if err := supervisor.Down(opts); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Sandbox %q is down\n", opts.Name)
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.KeepHome, "keep-home", false, "Keep the instance home (logs, config) after stopping")
	return cmd
}
