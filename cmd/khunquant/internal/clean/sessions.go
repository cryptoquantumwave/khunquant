package clean

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newSessionsCommand(sessionsDir func() string) *cobra.Command {
	return &cobra.Command{
		Use:     "sessions",
		Short:   "Delete all saved session histories",
		Args:    cobra.NoArgs,
		Example: "khunquant clean sessions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return cleanSessions(sessionsDir())
		},
	}
}

func cleanSessions(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No sessions directory found, nothing to clean.")
			return nil
		}
		return fmt.Errorf("read sessions dir: %w", err)
	}

	removed := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
		removed++
	}

	if removed == 0 {
		fmt.Println("No sessions to clean.")
	} else {
		fmt.Printf("Cleaned %d session(s).\n", removed)
	}
	return nil
}
