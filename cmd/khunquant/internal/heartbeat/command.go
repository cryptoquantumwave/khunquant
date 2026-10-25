// Package heartbeat implements `khunquant heartbeat`, which maintains the
// workspace's HEARTBEAT.md.
package heartbeat

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	hb "github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
)

const (
	templateFile = "HEARTBEAT.md"
	// backupName is the first choice of backup file. It is never overwritten:
	// when it exists, ".1", ".2", ... are tried.
	backupName = "HEARTBEAT.md.v1.bak"
	// maxBackups bounds the search for a free backup name.
	maxBackups = 1000
)

// deps are the command's seams.
type deps struct {
	configPath func() string
	load       func(path string) (*config.Config, error)
}

func defaultDeps() deps {
	return deps{configPath: internal.GetConfigPath, load: config.LoadConfig}
}

// NewHeartbeatCommand returns `khunquant heartbeat` with its upgrade-template
// subcommand.
func NewHeartbeatCommand() *cobra.Command {
	return newHeartbeatCommand(defaultDeps())
}

func newHeartbeatCommand(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Maintain the heartbeat's HEARTBEAT.md",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newUpgradeTemplateCommand(d))
	return cmd
}

func newUpgradeTemplateCommand(d deps) *cobra.Command {
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "upgrade-template",
		Short: "Replace HEARTBEAT.md with the v2 template, keeping a backup",
		Long: "The v2 template is written for a governed heartbeat: the model reports what it finds " +
			"through respond_proactive, and the system decides whether, when and where you hear about it. " +
			"The current HEARTBEAT.md is copied to HEARTBEAT.md.v1.bak first (an existing backup is never " +
			"overwritten; HEARTBEAT.md.v1.bak.1, .2, ... are used instead), then the new file is written. " +
			"A file that is already v2 is left alone. While the effective heartbeat mode is not governed the v2 " +
			"template would make heartbeats deliver nothing, so the command refuses unless --force is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := d.load(d.configPath())
			if err != nil {
				return fmt.Errorf("error loading config: %w", err)
			}
			// The heartbeat service reads HEARTBEAT.md from this workspace.
			return upgradeTemplate(cmd.OutOrStdout(), cfg, dryRun, force)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be done and change nothing")
	cmd.Flags().BoolVar(&force, "force", false,
		"Upgrade even though the effective heartbeat mode is not governed (heartbeats then deliver nothing)")
	return cmd
}

// upgradeTemplate does the work of the command.
func upgradeTemplate(out io.Writer, cfg *config.Config, dryRun, force bool) error {
	workspace := cfg.WorkspacePath()
	if workspace == "" {
		return errors.New("the workspace is not configured")
	}
	path := filepath.Join(workspace, templateFile)

	current, err := os.ReadFile(path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	if !missing && hb.IsV2Template(string(current)) {
		fmt.Fprintf(out, "%s is already the v2 template. Nothing changed.\n", path)
		return nil
	}

	// A v2 file under a heartbeat that is not governed delivers nothing: say so
	// before anything is written, and go on only when asked to.
	notGoverned := cfg.Proactive.Effective().HeartbeatMode != config.HeartbeatModeGoverned
	if notGoverned {
		fmt.Fprintf(out, "WARNING: the heartbeat is in legacy mode; with the v2 template it will deliver nothing "+
			"until proactive.heartbeat.mode is governed and attention is live "+
			"(that needs proactive enabled, an owner and attention.mode live).\n")
		if !force && !dryRun {
			return errors.New("refusing to upgrade HEARTBEAT.md while the heartbeat is not governed; " +
				"nothing was changed (use --force to upgrade anyway)")
		}
		if !force {
			fmt.Fprintln(out, "[dry run] Without --force the upgrade would be refused.")
		}
	}

	var backup string
	if !missing {
		backup, err = freeBackupPath(workspace)
		if err != nil {
			return err
		}
	}

	if dryRun {
		if missing {
			fmt.Fprintf(out, "[dry run] %s does not exist; would write the v2 template to it.\n", path)
		} else {
			fmt.Fprintf(out, "[dry run] Would back up %s as %s\n", path, backup)
			fmt.Fprintf(out, "[dry run] Would write the v2 template to %s\n", path)
		}
		printAfter(out, backup, true)
		fmt.Fprintln(out, "[dry run] Nothing was changed.")
		return nil
	}

	if !missing {
		if err := writeBackup(backup, current); err != nil {
			return fmt.Errorf("writing the backup %s: %w (HEARTBEAT.md was not changed)", backup, err)
		}
		fmt.Fprintf(out, "Backed up %s as %s\n", path, backup)
	} else {
		fmt.Fprintf(out, "%s does not exist; there is nothing to back up.\n", path)
	}
	if err := fileutil.WriteFileAtomic(path, []byte(hb.TemplateV2()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(out, "Wrote the v2 template to %s\n", path)
	printAfter(out, backup, false)
	return nil
}

// printAfter says what the user should know once the file is (or, in a dry run,
// would be) replaced.
func printAfter(out io.Writer, backup string, dryRun bool) {
	if backup != "" {
		verb := "is"
		if dryRun {
			verb = "will be"
		}
		fmt.Fprintf(out, "Your previous file, with any tasks you added, %s in %s.\n", verb, backup)
		fmt.Fprintf(out, "The v2 template has its own task list: copy your own tasks below its %q line.\n",
			"Add your heartbeat tasks below this line:")
	}
	if !dryRun {
		fmt.Fprintln(out, "A running gateway reads the file at its next heartbeat; no restart is needed.")
	}
}

// freeBackupPath returns the first of HEARTBEAT.md.v1.bak, .v1.bak.1, .v1.bak.2,
// ... that does not exist.
func freeBackupPath(workspace string) (string, error) {
	for i := 0; i < maxBackups; i++ {
		name := backupName
		if i > 0 {
			name += "." + strconv.Itoa(i)
		}
		p := filepath.Join(workspace, name)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		} else if err != nil {
			return "", fmt.Errorf("checking %s: %w", p, err)
		}
	}
	return "", fmt.Errorf("%d backups of HEARTBEAT.md already exist in %s; remove some and try again",
		maxBackups, workspace)
}

// writeBackup creates path with data and syncs it. It fails, rather than
// replacing, when path appeared since freeBackupPath looked: a backup is never
// overwritten.
func writeBackup(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
