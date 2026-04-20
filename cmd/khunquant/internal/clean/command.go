package clean

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
)

func NewCleanCommand() *cobra.Command {
	var workspacePath string

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean agent state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := internal.LoadConfig()
			if err != nil {
				return fmt.Errorf("error loading config: %w", err)
			}
			workspacePath = cfg.WorkspacePath()
			return nil
		},
	}

	cmd.AddCommand(
		newSessionsCommand(func() string { return filepath.Join(workspacePath, "sessions") }),
	)

	return cmd
}
