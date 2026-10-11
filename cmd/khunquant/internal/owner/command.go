// Package owner implements `khunquant owner`, which sets the one person
// proactive output may go to (proactive.owner in config.json).
//
// Strictness lives here rather than in the config loaders: a typo found at
// start-up must not stop the gateway, but a human typing `owner set` gets an
// immediate refusal with the reason.
package owner

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// deps are the command's seams: where the config lives and how it is read and
// written. Production wiring uses the same load and save as the other
// config-writing commands, so credentials in .security.yml round-trip intact.
type deps struct {
	configPath func() string
	load       func(path string) (*config.Config, error)
	save       func(path string, cfg *config.Config) error
}

func defaultDeps() deps {
	return deps{
		configPath: internal.GetConfigPath,
		load:       config.LoadConfig,
		save:       config.SaveConfig,
	}
}

// NewOwnerCommand returns `khunquant owner` with its show, set and clear
// subcommands.
func NewOwnerCommand() *cobra.Command {
	return newOwnerCommand(defaultDeps())
}

func newOwnerCommand(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "owner",
		Short: "Show or set the owner that proactive messages go to",
		Long: "The owner is the one person proactive output may be sent to, in a direct chat. " +
			"Send /whoami to the bot to learn the values to use.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newShowCommand(d), newSetCommand(d), newClearCommand(d))
	return cmd
}

func loadConfig(d deps) (*config.Config, string, error) {
	path := d.configPath()
	cfg, err := d.load(path)
	if err != nil {
		return nil, "", fmt.Errorf("error loading config: %w", err)
	}
	return cfg, path, nil
}

func newShowCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the configured owner and the proactive modes actually in effect",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfig(d)
			if err != nil {
				return err
			}
			printOwner(cmd.OutOrStdout(), cfg.Proactive)
			return nil
		},
	}
}

func newSetCommand(d deps) *cobra.Command {
	var channel, chatID, sender string
	cmd := &cobra.Command{
		Use:   "set --channel telegram --chat-id 123 --sender telegram:123",
		Short: "Set the owner (a direct chat; the gateway picks the change up on reload)",
		Long: "Sets proactive.owner. The chat must be a direct chat, not a group, and the sender " +
			"must be the canonical \"platform:id\" of the same platform as --channel. " +
			"Send /whoami to the bot in a private chat to see the values.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var missing []string
			for _, f := range []struct{ flag, v string }{
				{"--channel", channel}, {"--chat-id", chatID}, {"--sender", sender},
			} {
				if strings.TrimSpace(f.v) == "" {
					missing = append(missing, f.flag)
				}
			}
			if len(missing) > 0 {
				return fmt.Errorf("missing required flag(s): %s", strings.Join(missing, ", "))
			}

			// Judge the owner on its own: an unrelated typo elsewhere in the
			// section must not stop the operator from fixing the owner. The
			// launcher's owner form uses the same helper.
			owner, err := config.ValidateOwnerInput(channel, chatID, sender)
			if err != nil {
				return fmt.Errorf("owner not saved:\n%w", err)
			}

			cfg, path, err := loadConfig(d)
			if err != nil {
				return err
			}
			cfg.Proactive.Owner = owner
			if err := d.save(path, cfg); err != nil {
				return fmt.Errorf("error saving config: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Owner saved.")
			printOwner(cmd.OutOrStdout(), cfg.Proactive)
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "channel of the owner's direct chat, e.g. telegram (required)")
	cmd.Flags().StringVar(&chatID, "chat-id", "", "chat id of the owner's direct chat (required)")
	cmd.Flags().StringVar(&sender, "sender", "", `owner's canonical sender id, e.g. "telegram:123456" (required)`)
	return cmd
}

func newClearCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Remove the owner; delivering proactive modes fall back to shadow/legacy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, path, err := loadConfig(d)
			if err != nil {
				return err
			}
			cfg.Proactive.Owner = config.OwnerConfig{}
			if err := d.save(path, cfg); err != nil {
				return fmt.Errorf("error saving config: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Owner cleared.")
			printOwner(cmd.OutOrStdout(), cfg.Proactive)
			return nil
		},
	}
}

func orNotSet(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(not set)"
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// printOwner prints the owner and the modes that actually apply, with every
// downgrade, so the operator sees what the gateway will do rather than what
// the file says.
func printOwner(w io.Writer, p config.ProactiveConfig) {
	e := p.Effective()
	fmt.Fprintf(w, "owner.channel:   %s\n", orNotSet(p.Owner.Channel))
	fmt.Fprintf(w, "owner.chat_id:   %s\n", orNotSet(p.Owner.ChatID))
	fmt.Fprintf(w, "owner.sender_id: %s\n", orNotSet(p.Owner.SenderID))
	fmt.Fprintf(w, "owner configured: %s\n", yesNo(e.OwnerConfigured))
	fmt.Fprintf(w, "proactive.enabled: %s\n", onOff(e.Enabled))
	fmt.Fprintln(w, "effective modes:")
	fmt.Fprintf(w, "  heartbeat: %s\n  attention: %s\n  triggers:  %s\n  policy:    %s\n",
		e.HeartbeatMode, e.AttentionMode, e.TriggersMode, e.PolicyMode)
	fmt.Fprintf(w, "  approvals: %s  mandates: %s  intents: %s  news: %s\n",
		onOff(e.Approvals), onOff(e.Mandates), onOff(e.Intents), onOff(e.News))
	fmt.Fprintf(w, "  perception: %s  cognition: %s  reflection: %s  learning: %s\n",
		onOff(e.Perception), onOff(e.Cognition), onOff(e.Reflection), onOff(e.Learning))
	if err := config.ValidateProactive(p); err != nil {
		fmt.Fprintf(w, "config problems (the section is treated as having no owner until fixed):\n%v\n", err)
	}
	if !e.Enabled {
		fmt.Fprintln(w, "note: proactive.enabled is false, so nothing proactive is delivered yet.")
	}
	for _, d := range e.Downgrades {
		fmt.Fprintf(w, "downgrade: %s\n", d)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
