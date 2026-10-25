package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/cryptoquantumwave/khunquant/pkg/credential"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
	"github.com/cryptoquantumwave/khunquant/web/backend/utils"
)

// bcryptCost is deliberately above bcrypt.DefaultCost (10). The dashboard
// password guards local trading controls and is verified once per login, so the
// extra work is unnoticeable to a user and meaningful against an offline crack
// of a stolen launcher-config.json.
const bcryptCost = 12

// NewLauncherCommand groups launcher administration subcommands.
func NewLauncherCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launcher",
		Short: "Manage web launcher settings (dashboard password)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newSetPasswordCommand())

	return cmd
}

// newSetPasswordCommand sets or clears the dashboard password.
//
// C-6: the launcher auto-issues a session to any loopback caller, and that rule
// is disabled only when a dashboard password is configured (SessionAuth in
// web/backend/middleware/auth.go). Until now nothing in the shipped binaries
// could write launcherconfig.DashboardPasswordHash, so no deployment could
// reach that gate and every one accepted loopback callers unauthenticated.
// A same-host reverse proxy or tunnel makes every remote user look like
// loopback, so this is not only a local-console concern.
func newSetPasswordCommand() *cobra.Command {
	var appConfigPath string
	var clear bool

	cmd := &cobra.Command{
		Use:   "set-password",
		Short: "Set (or clear) the web dashboard password",
		Long: "Sets the web launcher's dashboard password.\n\n" +
			"Until a password is set, the launcher grants any caller reaching it over\n" +
			"loopback a session without authentication - including remote users arriving\n" +
			"through a same-host reverse proxy or tunnel.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			launcherPath, err := resolveLauncherConfigPath(appConfigPath)
			if err != nil {
				return err
			}

			// Default() rather than a zero Config: Load returns the fallback
			// verbatim when the file is absent, and Save validates before
			// writing, so a zero value would fail with "port 0 is out of range"
			// on a fresh install.
			cfg, err := launcherconfig.Load(launcherPath, launcherconfig.Default())
			if err != nil {
				return fmt.Errorf("read %s: %w", launcherPath, err)
			}

			if clear {
				cfg.DashboardPasswordHash = ""
				if err := launcherconfig.Save(launcherPath, cfg); err != nil {
					return fmt.Errorf("write %s: %w", launcherPath, err)
				}
				cmd.Printf("Dashboard password cleared in %s\n", launcherPath)
				cmd.Println("The launcher will again issue a session to any loopback caller without")
				cmd.Println("authentication. Restart the launcher for this to take effect.")
				return nil
			}

			// term.ReadPassword on a pipe fails with an obscure ioctl error.
			// Refuse explicitly instead: a password read from a pipe would be
			// written into a config file from a shell history or CI log.
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("set-password requires an interactive terminal; run it from a console")
			}

			plain, err := credential.PromptPassphraseWithLabel("Enter new dashboard password", true)
			if err != nil {
				return err
			}

			hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
			if err != nil {
				return fmt.Errorf("hash password: %w", err)
			}

			cfg.DashboardPasswordHash = string(hash)
			if err := launcherconfig.Save(launcherPath, cfg); err != nil {
				return fmt.Errorf("write %s: %w", launcherPath, err)
			}

			cmd.Printf("Dashboard password set in %s\n", launcherPath)
			// The launcher holds its sessions in memory in its own process, so
			// this command cannot revoke them. Say so rather than implying it.
			cmd.Println("Restart the launcher for it to take effect and to drop existing sessions.")
			return nil
		},
	}

	cmd.Flags().StringVar(&appConfigPath, "config", "", "Path to the app config file (default: the launcher's own default)")
	cmd.Flags().BoolVar(&clear, "clear", false, "Remove the dashboard password instead of setting one")

	return cmd
}

// resolveLauncherConfigPath derives the launcher-config path the same way the
// launcher binary does (web/backend/main.go): from the *app* config path, via
// launcherconfig.PathForAppConfig. Diverging here would write a hash the
// launcher never reads, which fails silently and looks exactly like the gate
// not working.
func resolveLauncherConfigPath(appConfigPath string) (string, error) {
	path := strings.TrimSpace(appConfigPath)
	if path == "" {
		path = utils.GetDefaultConfigPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", path, err)
	}
	return launcherconfig.PathForAppConfig(abs), nil
}
