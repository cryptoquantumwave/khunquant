package sandboxlab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// instanceOut is what `up` prints. The token is shown here and nowhere else.
type instanceOut struct {
	Name       string `json:"name"`
	Home       string `json:"home"`
	PID        int    `json:"pid"`
	GatewayURL string `json:"gateway_url"`
	Token      string `json:"token"`
	Log        string `json:"log"`
}

func newUpCommand() *cobra.Command {
	var (
		opts    supervisor.UpOptions
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start a locked sandbox gateway in its own HOME",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true // a failed start is not a usage error
			if runtime.GOOS == "windows" {
				return supervisor.ErrUnixOnly
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			sup, err := supervisor.New(supervisor.Config{})
			if err != nil {
				return err
			}
			info, err := sup.Up(ctx, opts)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			if !jsonOut {
				enc.SetIndent("", "  ")
			}
			// The gateway outlives this command (own process group); `down` stops it.
			return enc.Encode(instanceOut{info.Name, info.Home, info.PID, info.GatewayURL, info.RevealToken(), info.LogPath})
		},
	}
	upFlags(cmd.Flags(), &opts)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the instance as one line of JSON (default: indented)")
	return cmd
}

// upFlags defines one flag per option of supervisor.UpOptions, so a new option
// reaches the command line without touching this file.
func upFlags(fs *pflag.FlagSet, opts *supervisor.UpOptions) {
	v := reflect.ValueOf(opts).Elem()
	for _, f := range supervisor.UpOptionFields() {
		ptr := v.Field(f.Index).Addr().Interface()
		switch p := ptr.(type) {
		case *string:
			fs.StringVar(p, f.Flag, "", f.Desc)
		case *bool:
			fs.BoolVar(p, f.Flag, false, f.Desc)
		case *int:
			fs.IntVar(p, f.Flag, 0, f.Desc)
		default:
			panic(fmt.Sprintf("sandbox up: option %q has unsupported type %T", f.Flag, ptr))
		}
	}
}
