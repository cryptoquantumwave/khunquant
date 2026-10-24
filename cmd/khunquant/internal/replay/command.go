// Package replay is `khunquant replay`: it runs recorded events through the real
// world model, triggers and attention under a fake clock, and reports what the owner
// would have heard. See pkg/replay for what is replayed and how.
package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/replay"
)

// Exit codes. 1 is any other failure.
const (
	ExitUsage          = 2
	ExitNondeterminism = 3
	ExitCriticalRecall = 4
)

const maxRepeat = 100

// ExitError is the error a command returns to say how the process should end; the
// caller prints Error() (when it is not empty) on stderr and exits with Code.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// ExitCode is the process exit status.
func (e *ExitError) ExitCode() int { return e.Code }

func exitf(code int, format string, a ...any) error {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

type options struct {
	events, labels, policy, llm, out string
	repeat                           int
}

// NewReplayCommand returns `khunquant replay`.
func NewReplayCommand() *cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "replay --events <file|dir>",
		Short: "Replay recorded events through the proactive pipeline and report what you would have heard",
		Long: "Runs recorded events (proactive.record_events writes them to <workspace>/replay/) through the real world\n" +
			"model, trigger registry and attention rules, with a clock driven by the events, an in-memory database,\n" +
			"a notify that only collects and no model. It never opens your databases, never contacts a running gateway,\n" +
			"never places an order and writes only the file named by --out.\n\n" +
			"The report holds candidates and what attention decided per day, the messages you would have received, and,\n" +
			"with --labels, precision, recall, critical recall and detection latency. --policy current is this build's\n" +
			"defaults; a file replaces them (see pkg/replay.Policy), so a change under test is a file that differs.\n\n" +
			"Exit codes: 0 done, 1 failed, 2 usage error, 3 --repeat found the runs differ, 4 critical recall below 100%.",
		Example: "  khunquant replay --events ~/.khunquant/workspace/replay --labels labels.json --out report.json\n" +
			"  khunquant replay --events testdata/replay/flash_crash/events.jsonl --policy new-thresholds.json --repeat 3",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitf(ExitUsage, "usage: khunquant replay --events <file|dir> (unexpected argument %q)", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return exitf(ExitUsage, "%v", err) })
	f := cmd.Flags()
	f.StringVar(&o.events, "events", "", "a recording: one .jsonl file, or a directory of them (required)")
	f.StringVar(&o.labels, "labels", "", "a labels file: what you should have heard, and when")
	f.StringVar(&o.policy, "policy", replay.CurrentPolicy, "current, or a policy file")
	f.StringVar(&o.llm, "llm", "off", "off (the only mode: templates, no model)")
	f.StringVar(&o.out, "out", "", "write the report here (the only file written); default is standard output")
	f.IntVar(&o.repeat, "repeat", 1, "run this many times and fail if any report differs")
	return cmd
}

func run(ctx context.Context, o options, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch {
	case o.events == "":
		return exitf(ExitUsage, "--events is required")
	case o.llm == "on":
		return exitf(ExitUsage, "--llm on is not available: it would call the configured model provider, and a replay "+
			"never does. Use --llm off (templates, no model)")
	case o.llm != "off":
		return exitf(ExitUsage, "--llm must be off, got %q", o.llm)
	case o.repeat < 1 || o.repeat > maxRepeat:
		return exitf(ExitUsage, "--repeat must be from 1 to %d", maxRepeat)
	}
	if err := refuseOverwritingInput(o); err != nil {
		return exitf(ExitUsage, "%v", err)
	}

	in := replay.Input{}
	var err error
	if in.Lines, err = replay.LoadEvents(o.events); err != nil {
		return err
	}
	if o.labels != "" {
		if in.Labels, err = replay.LoadLabels(o.labels); err != nil {
			return err
		}
	}
	if in.Policy, err = replay.LoadPolicy(o.policy); err != nil {
		return err
	}

	rep, err := replay.Repeat(o.repeat, func() (*replay.Report, error) { return replay.Run(ctx, in) })
	var nd *replay.ErrNondeterministic
	if errors.As(err, &nd) {
		return exitf(ExitNondeterminism, "%v", err)
	}
	if err != nil {
		return err
	}

	b, err := rep.JSON()
	if err != nil {
		return err
	}
	if o.out != "" {
		if err := os.WriteFile(o.out, b, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", o.out, err)
		}
	} else if _, err := stdout.Write(b); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "replay: %d events, %d candidates, %d messages would have been sent, %d model wakes\n",
		rep.Events.Total, rep.Totals.Candidates, rep.Totals.WouldSend, rep.LLMWakes)
	if rep.Labels != nil {
		fmt.Fprintf(stderr, "replay: precision %.3f, recall %.3f, critical recall %.3f\n",
			rep.Labels.Precision, rep.Labels.Recall, rep.Labels.CriticalRecall)
	}
	if !rep.CriticalRecallOK() {
		return exitf(ExitCriticalRecall, "critical recall is %.1f%% (%d of %d critical labels fired); it must be 100%%",
			rep.Labels.CriticalRecall*100, rep.Labels.CriticalFired, rep.Labels.CriticalLabels)
	}
	return nil
}

// refuseOverwritingInput refuses an --out that is one of the inputs or is inside the
// events directory: the report is the only thing a replay writes, and it must never
// replace a recording.
func refuseOverwritingInput(o options) error {
	if o.out == "" {
		return nil
	}
	out, err := filepath.Abs(o.out)
	if err != nil {
		return err
	}
	if st, err := os.Stat(out); err == nil && st.IsDir() {
		return fmt.Errorf("--out %s is a directory", o.out)
	}
	for _, in := range []string{o.events, o.labels, o.policy} {
		if in == "" || in == replay.CurrentPolicy {
			continue
		}
		abs, err := filepath.Abs(in)
		if err != nil {
			return err
		}
		if abs == out || isUnder(abs, out) {
			return fmt.Errorf("--out %s would overwrite or sit inside an input (%s)", o.out, in)
		}
		if a, err1 := os.Stat(abs); err1 == nil {
			if b, err2 := os.Stat(out); err2 == nil && os.SameFile(a, b) {
				return fmt.Errorf("--out %s is the same file as an input (%s)", o.out, in)
			}
		}
	}
	return nil
}

// isUnder reports whether path is inside dir.
func isUnder(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
