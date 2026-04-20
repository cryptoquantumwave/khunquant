package update

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/updater"
)

func NewUpdateCommand() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update khunquant to the latest version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(yes)
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	return cmd
}

func runUpdate(skipConfirm bool) error {
	currentVersion := config.GetVersion()

	fmt.Printf("%s Checking for updates (current: %s)…\n", internal.Logo, currentVersion)

	info, err := updater.CheckForUpdate(context.Background(), updater.DefaultOwner, updater.DefaultRepo, currentVersion)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	if info == nil || !info.IsOutdated {
		fmt.Printf("%s Already up to date (%s)\n", internal.Logo, currentVersion)
		return nil
	}

	fmt.Printf("%s New version available: %s\n", internal.Logo, info.LatestVersion)

	if runtime.GOOS == "windows" {
		fmt.Printf("   Automatic update is not supported on Windows.\n")
		fmt.Printf("   Download manually: %s\n", info.ReleaseURL)
		return nil
	}

	if !skipConfirm {
		fmt.Printf("   Update now? [y/N] ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if answer != "y" && answer != "yes" {
			fmt.Println("   Aborted.")
			return nil
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine binary path: %w", err)
	}
	// Resolve symlinks so we replace the real file.
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	binaryName := "khunquant"
	if runtime.GOOS == "windows" {
		binaryName = "khunquant.exe"
	}

	fmt.Printf("%s Downloading %s…\n", internal.Logo, info.LatestVersion)

	// Track download progress in-place.
	progress := func(downloaded, total int64) {
		if total > 0 {
			pct := float64(downloaded) / float64(total) * 100
			fmt.Fprintf(os.Stderr, "\r   Progress: %.0f%%  ", pct)
		}
	}

	// Pass the already-fetched info to avoid a redundant API call inside SelfUpdate.
	updated, err := updater.SelfUpdate(context.Background(), updater.DefaultOwner, updater.DefaultRepo, currentVersion, binaryName, exePath, info, progress)
	fmt.Fprintln(os.Stderr) // newline after progress output
	if err != nil {
		fmt.Printf("   Update failed: %v\n", err)
		fmt.Printf("   Download manually: %s\n", info.ReleaseURL)
		return nil
	}
	if updated == nil {
		fmt.Printf("%s Already up to date.\n", internal.Logo)
		return nil
	}

	fmt.Printf("%s Updated to %s successfully!\n", internal.Logo, updated.LatestVersion)
	fmt.Println("   Restart the gateway (or run 'khunquant gateway') to apply the update.")
	return nil
}
