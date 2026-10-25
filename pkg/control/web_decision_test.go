package control_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

// The launcher's decision reaches the approval service through the control path's one
// decision, marked as the web's, with a sender that is not the configured owner.
func TestDecideFromLauncher_WebOwnerDecidesAsTheWeb(t *testing.T) {
	r, m := webRig(t, true, nil)
	got := r.d.DecideFromLauncher(context.Background(), "7f3k", true)
	if got.Result != control.LauncherDecided {
		t.Fatalf("result = %q (%s)", got.Result, got.Text)
	}
	mustContain(t, "text", got.Text, "Approved proposal 7F3K")
	if len(m.web) != 1 || !m.web[0] {
		t.Fatalf("Decide marker = %v, want [true]", m.web)
	}
	calls := r.appr.calls()
	if len(calls) != 1 || calls[0].Code != "7F3K" || !calls[0].Approve || calls[0].Nonce != "" || calls[0].Sender != "pico:pico-user" {
		t.Errorf("Decide calls = %+v", calls)
	}
	if rej := r.d.DecideFromLauncher(context.Background(), "7F3K", false); rej.Result != control.LauncherDecided {
		t.Errorf("reject = %+v", rej)
	}
}

func TestDecideFromLauncher_NothingIsDecidedUnlessTheWebIsTheOwner(t *testing.T) {
	off := false
	cases := []struct {
		name   string
		gate   bool
		mutate func(*config.Config)
	}{
		{"launcher authentication not established", false, nil},
		{"web_is_owner false", true, func(c *config.Config) { c.Proactive.Owner.WebIsOwner = &off }},
		{"no owner configured", true, func(c *config.Config) { c.Proactive.Owner = config.OwnerConfig{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, m := webRig(t, tc.gate, tc.mutate)
			for _, approve := range []bool{true, false} {
				if got := r.d.DecideFromLauncher(context.Background(), "7F3K", approve); got.Result != control.LauncherNotOwner {
					t.Errorf("approve=%v: result = %q", approve, got.Result)
				}
			}
			if len(m.web) != 0 || len(r.appr.calls()) != 0 {
				t.Errorf("Decide was called: %v / %+v", m.web, r.appr.calls())
			}
		})
	}
}

func TestDecideFromLauncher_ServiceAnswersMapToResults(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want control.LauncherResult
	}{
		{"kill switch", approval.ErrKilled, control.LauncherHeld},
		{"gone or expired", approval.ErrNotFound, control.LauncherNotFound},
		{"decided already", &approval.StatusError{Code: "7F3K", Status: approval.StatusRejected}, control.LauncherSettled},
		{"service re-proof fails", approval.ErrNotOwner, control.LauncherNotOwner},
		{"cannot apply", approval.ErrUnavailable, control.LauncherUnavailable},
		{"other", errors.New("db down"), control.LauncherFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := webRig(t, true, nil)
			r.appr.decideFn = func(string, bool) (approval.Proposal, error) { return approval.Proposal{}, tc.err }
			if got := r.d.DecideFromLauncher(context.Background(), "7F3K", true); got.Result != tc.want {
				t.Errorf("result = %q (%s), want %q", got.Result, got.Text, tc.want)
			}
		})
	}
}

func TestDecideFromLauncher_BadCodeAndApprovalsOff(t *testing.T) {
	r, m := webRig(t, true, nil)
	if got := r.d.DecideFromLauncher(context.Background(), "../x", true); got.Result != control.LauncherBadCode {
		t.Errorf("bad code: %q", got.Result)
	}
	deps := r.d.Deps()
	deps.Approvals = nil
	r.d.SetDeps(deps)
	if got := r.d.DecideFromLauncher(context.Background(), "7F3K", true); got.Result != control.LauncherUnavailable {
		t.Errorf("approvals off: %q", got.Result)
	}
	if len(m.web) != 0 {
		t.Error("Decide was called")
	}
}
