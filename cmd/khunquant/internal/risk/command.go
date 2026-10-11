// Package risk implements `khunquant risk`, the operator's view of and handle
// on the daily loss guard.
package risk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// deps are the command's seams: where config comes from and what time it is.
type deps struct {
	loadConfig func() (*config.Config, error)
	now        func() time.Time
}

func defaultDeps() deps {
	return deps{loadConfig: internal.LoadConfig, now: time.Now}
}

// NewRiskCommand returns `khunquant risk` with its status and reset-daily-loss
// subcommands.
func NewRiskCommand() *cobra.Command {
	return newRiskCommand(defaultDeps())
}

func newRiskCommand(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "risk",
		Short: "Inspect and manage trading risk controls",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newStatusCommand(d), newResetCommand(d))
	return cmd
}

// openTracker loads config and returns a tracker bound to the workspace's state
// file and loss-day time zone. It is a private instance, not the global one:
// the command works on the file, and a running gateway picks the change up from
// the file's modification time.
func openTracker(d deps) (*broker.DailyLossTracker, *config.Config, error) {
	cfg, err := d.loadConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("error loading config: %w", err)
	}
	loc := time.UTC
	if name := strings.TrimSpace(cfg.TradingRisk.LossDayTimezone); name != "" {
		if l, err := time.LoadLocation(name); err != nil {
			// Same fallback as the gateway, so a typo cannot lock the operator
			// out of the reset escape hatch.
			fmt.Fprintf(os.Stderr, "warning: invalid trading_risk.loss_day_timezone %q (%v); using UTC\n", name, err)
		} else {
			loc = l
		}
	}
	tr := &broker.DailyLossTracker{}
	tr.SetClock(d.now)
	tr.SetDayLocation(loc)
	tr.SetStorePath(broker.LossStatePath(cfg.WorkspacePath()))
	return tr, cfg, nil
}

func newStatusCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show today's loss against the daily loss limit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tr, cfg, err := openTracker(d)
			if err != nil {
				return err
			}
			printStatus(cmd.OutOrStdout(), tr.Snapshot(), cfg.TradingRisk.DailyLossLimitUSD, d.now())
			return nil
		},
	}
}

func newResetCommand(d deps) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reset-daily-loss",
		Short: "Start the loss day over (e.g. after a withdrawal) and lift a tripped limit",
		Long: "Re-baselines every account to its latest equity sample and zeroes the realised loss, " +
			"recording the reason. Use it after a withdrawal or outgoing transfer, which the equity " +
			"guard cannot tell from a loss. A running gateway picks the change up on its next check.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(reason) == "" {
				return errors.New(`--reason is required, e.g. --reason "withdrawal"`)
			}
			tr, _, err := openTracker(d)
			if err != nil {
				return err
			}
			before := tr.TodayLossUSD()
			if err := tr.Reset(reason); err != nil {
				return fmt.Errorf("reset daily loss: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Daily loss reset (was %.2f USD, now %.2f USD). Reason: %s\n",
				before, tr.TodayLossUSD(), strings.TrimSpace(reason))
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the loss is being reset (required)")
	return cmd
}

func printStatus(w io.Writer, s broker.LossSnapshot, limit float64, now time.Time) {
	fmt.Fprintf(w, "Loss day:      %s (%s)\n", s.Day, s.Location)
	fmt.Fprintf(w, "Realised loss: %.2f USD\n", s.RealizedUSD)

	if len(s.Accounts) == 0 {
		fmt.Fprintln(w, "Equity:        no samples yet today (the gateway samples equity while daily_loss_limit_usd > 0)")
	} else {
		keys := make([]string, 0, len(s.Accounts))
		for k := range s.Accounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Account\tBaseline\tLast\tDrawdown\tAge")
		for _, k := range keys {
			e := s.Accounts[k]
			fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%.2f\t%s\n",
				k, e.BaselineUSD, e.LastUSD, e.Drawdown(), now.Sub(e.LastSampleAt).Round(time.Second))
		}
		tw.Flush()
		for _, k := range keys {
			if c := s.Accounts[k].CarriedLossUSD; c > 0 {
				fmt.Fprintf(w, "  %s: +%.2f USD carried from earlier in the day (equity measured differently)\n", k, c)
			}
		}
	}

	fmt.Fprintf(w, "Total loss:    %.2f USD (larger of realised loss and the sum of drawdowns)\n", s.TotalUSD)
	switch {
	case limit <= 0:
		fmt.Fprintln(w, "Limit:         none (trading_risk.daily_loss_limit_usd is 0)")
		fmt.Fprintln(w, "Tripped:       no")
	case s.TotalUSD >= limit:
		fmt.Fprintf(w, "Limit:         %.2f USD\n", limit)
		fmt.Fprintln(w, "Tripped:       YES - risk-increasing orders are refused")
	default:
		fmt.Fprintf(w, "Limit:         %.2f USD\n", limit)
		fmt.Fprintln(w, "Tripped:       no")
	}
	if s.ResetAt != nil {
		fmt.Fprintf(w, "Last reset:    %s (%s)\n", s.ResetAt.Format(time.RFC3339), s.ResetReason)
	}
}
