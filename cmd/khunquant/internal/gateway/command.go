package gateway

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

func NewGatewayCommand() *cobra.Command {
	var debug bool
	var noTruncate bool
	var locked bool

	cmd := &cobra.Command{
		Use:     "gateway",
		Aliases: []string{"g"},
		Short:   "Start khunquant gateway",
		Args:    cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if noTruncate && !debug {
				return fmt.Errorf("the --no-truncate option can only be used in conjunction with --debug (-d)")
			}

			if noTruncate {
				utils.SetDisableTruncation(true)
				logger.Info("String truncation is globally disabled via 'no-truncate' flag")
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := gatewayCmd(debug, locked)
			// main prints a lock failure once and exits with its status (3).
			var fatal *lockFatalError
			if errors.As(err, &fatal) {
				cmd.SilenceErrors = true
				cmd.SilenceUsage = true
			}
			return err
		},
	}

	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug logging")
	cmd.Flags().BoolVar(&locked, "sandbox-locked", false,
		"Lock the process as a sandbox instance (also KQ_SANDBOX_LOCKED=1); needs KQ_SANDBOX_TOKEN and a sandbox KHUNQUANT_HOME")
	cmd.Flags().BoolVarP(&noTruncate, "no-truncate", "T", false, "Disable string truncation in debug logs")

	return cmd
}
