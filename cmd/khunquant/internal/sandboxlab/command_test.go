package sandboxlab

import (
	"testing"

	"github.com/spf13/pflag"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// The flags of `sandbox up` are generated from supervisor.UpOptions, plus --json.
func TestUpCommand_FlagsFollowUpOptions(t *testing.T) {
	cmd := newUpCommand()
	for _, f := range supervisor.UpOptionFields() {
		if cmd.Flags().Lookup(f.Flag) == nil {
			t.Errorf("sandbox up has no --%s", f.Flag)
		}
	}
	want := len(supervisor.UpOptionFields()) + 1 // --json
	n := 0
	cmd.Flags().VisitAll(func(*pflag.Flag) { n++ })
	if n != want {
		t.Errorf("sandbox up has %d flags, want %d (one per option and --json)", n, want)
	}
	for _, name := range []string{"name", "fixtures-from", "json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("the flag --%s was lost", name)
		}
	}
	if newDownCommand().Flags().Lookup("keep-home") == nil {
		t.Error("sandbox down lost --keep-home")
	}
}
