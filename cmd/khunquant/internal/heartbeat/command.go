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
	// userTasksLine is the line that ends the instructions of both templates.
	userTasksLine = "Add your heartbeat tasks below this line:"
)

// deps are the command's seams.
type deps struct {
	configPath func() string
	load       func(path string) (*config.Config, error)
}

func defaultDeps() deps {
	return deps{configPath: internal.GetConfigPath, load: config.LoadConfig}
}

// NewHeartbeatCommand returns `khunquant heartbeat` with its status and
// upgrade-template subcommands.
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
	cmd.AddCommand(newStatusCommand(d), newUpgradeTemplateCommand(d))
	return cmd
}

func newStatusCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the heartbeat mode and which HEARTBEAT.md template is in use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := d.load(d.configPath())
			if err != nil {
				return fmt.Errorf("error loading config: %w", err)
			}
			return printStatus(cmd.OutOrStdout(), cfg)
		},
	}
}

// printStatus says whether the heartbeat is enabled, the mode that actually
// applies, and what HEARTBEAT.md is.
func printStatus(out io.Writer, cfg *config.Config) error {
	workspace := cfg.WorkspacePath()
	if workspace == "" {
		return errors.New("the workspace is not configured")
	}
	path := filepath.Join(workspace, templateFile)
	e := cfg.Proactive.Effective()
	fmt.Fprintf(out, "heartbeat enabled: %t (every %d minutes)\n", cfg.Heartbeat.Enabled, cfg.Heartbeat.Interval)
	fmt.Fprintf(out, "heartbeat mode: %s\n", e.HeartbeatMode)

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(out, "%s: missing (the heartbeat writes the v2 template, with no tasks, when it next runs)\n", path)
	case err != nil:
		return fmt.Errorf("reading %s: %w", path, err)
	case hb.IsV2Template(string(data)):
		fmt.Fprintf(out, "%s: v2 template\n", path)
	case hb.IsV1Template(string(data)):
		fmt.Fprintf(out, "%s: v1 template (works; run `khunquant heartbeat upgrade-template` to move to v2)\n", path)
	default:
		fmt.Fprintf(out, "%s: your own file (neither template)\n", path)
	}
	return nil
}

func newUpgradeTemplateCommand(d deps) *cobra.Command {
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "upgrade-template",
		Short: "Replace HEARTBEAT.md with the v2 template, keeping a backup and your tasks",
		Long: "The v2 template is shorter than v1 and works in every heartbeat mode: the model reports " +
			"through respond_proactive when the heartbeat is governed or a sweep, and sends one short message " +
			"in the default legacy mode. The current HEARTBEAT.md is copied to HEARTBEAT.md.v1.bak first (an " +
			"existing backup is never overwritten; HEARTBEAT.md.v1.bak.1, .2, ... are used instead), then the " +
			"new file is written. The tasks you wrote below the \"Add your heartbeat tasks below this line:\" " +
			"line are carried into the new file; a file without that line is kept whole under the new tasks " +
			"section. A file that is already v2 is left alone.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := d.load(d.configPath())
			if err != nil {
				return fmt.Errorf("error loading config: %w", err)
			}
			if force {
				fmt.Fprintln(cmd.OutOrStdout(), "Note: --force is no longer needed; the v2 template works in every heartbeat mode.")
			}
			// The heartbeat service reads HEARTBEAT.md from this workspace.
			return upgradeTemplate(cmd.OutOrStdout(), cfg, dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be done and change nothing")
	// Kept so that a script written for the earlier command does not fail on an
	// unknown flag; it does nothing.
	cmd.Flags().BoolVar(&force, "force", false, "No longer needed; accepted and ignored")
	_ = cmd.Flags().MarkHidden("force")
	return cmd
}

// upgradeTemplate does the work of the command.
func upgradeTemplate(out io.Writer, cfg *config.Config, dryRun bool) error {
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

	next, outcome := hb.TemplateV2(), hb.TasksWereShippedDefault
	if !missing {
		next, outcome = hb.UpgradeToV2(string(current))
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
			printKept(out, outcome, backup, true)
		}
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
	if err := fileutil.WriteFileAtomic(path, []byte(next), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(out, "Wrote the v2 template to %s\n", path)
	if !missing {
		printKept(out, outcome, backup, false)
	}
	fmt.Fprintln(out, "A running gateway reads the file at its next heartbeat; no restart is needed.")
	return nil
}

// printKept says what became of the owner's tasks; backup is the file that holds
// the previous content whole.
func printKept(out io.Writer, outcome hb.UpgradeOutcome, backup string, dryRun bool) {
	verb := "kept"
	if dryRun {
		verb = "would be kept"
	}
	switch outcome {
	case hb.TasksKept:
		fmt.Fprintf(out, "Your tasks (everything below the %q line) %s in the new file.\n", userTasksLine, verb)
	case hb.TasksWereShippedDefault:
		fmt.Fprintf(out, "Your task list was the default one of the v1 template; the new file has the v2 default task.\n")
	case hb.TasksNone:
		fmt.Fprintf(out, "You had no tasks, so the new file has none either and runs nothing until you add one.\n")
	case hb.TasksWholeFile:
		fmt.Fprintf(out, "WARNING: the old file has no %q line, so its WHOLE content was %s under the new "+
			"tasks section, instructions included. Review it and delete what the v2 instructions already cover; "+
			"the original is in %s.\n", userTasksLine, verb, backup)
	}
	if outcome != hb.TasksNone {
		fmt.Fprintf(out, "Your previous file is in %s.\n", backup)
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
