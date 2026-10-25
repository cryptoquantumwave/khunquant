package gateway

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const approvalSweeperFrame = "approval.(*Service).Run("

func withApprovals(c *config.Config) {
	withOwner("1001")(c)
	c.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
}

func proposalsTables(t *testing.T, workspace string) int {
	t.Helper()
	db, err := sqlitex.Shared(workspace)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'proposals'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func hasProposeTool(f *assemblyFixture) bool {
	_, ok := f.al.GetRegistry().GetDefaultAgent().Tools.Get(tools.NameProposeOrder)
	return ok
}

// With approvals off (the default, with or without an owner) there is no tool, no
// table, no workflow definition and no goroutine.
func TestApproval_OffBuildsNothing(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"default config":       nil,
		"owner, approvals off": withOwner("1001"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mutate)
			if f.services.Approval != nil {
				t.Fatal("an approval runtime was built with approvals off")
			}
			if hasProposeTool(f) {
				t.Error("propose_order is registered with approvals off")
			}
			if n := proposalsTables(t, f.workspace); n != 0 {
				t.Errorf("%d proposals tables created", n)
			}
			if n := goroutinesIn(approvalSweeperFrame); n != 0 {
				t.Errorf("%d approval goroutines", n)
			}
			if f.services.journalLive.Load() != nil {
				t.Error("a workflow engine was built for no workflow")
			}
		})
	}
}

// With approvals on the service, its table, its sweeper and the tool exist, a
// reload rebuilds them, and stopping the services ends the sweeper.
func TestApproval_OnBuildsRebuildsAndStops(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals)
	if f.services.Approval == nil || f.services.journalLive.Load() == nil {
		t.Fatal("approvals on, but no runtime or no workflow engine")
	}
	if !hasProposeTool(f) {
		t.Error("propose_order is not registered with approvals on")
	}
	if n := proposalsTables(t, f.workspace); n != 1 {
		t.Errorf("%d proposals tables", n)
	}
	if n := goroutinesIn(approvalSweeperFrame); n != 1 {
		t.Fatalf("%d approval sweepers, want 1", n)
	}

	first := f.services.Approval
	if err := f.reload(withApprovals); err != nil {
		t.Fatal(err)
	}
	if f.services.Approval == nil || f.services.Approval == first {
		t.Fatal("a reload did not rebuild the approval runtime")
	}
	if n := goroutinesIn(approvalSweeperFrame); n != 1 {
		t.Fatalf("%d approval sweepers after a reload, want 1", n)
	}

	// A reload that switches approvals off removes the lot.
	if err := f.reload(withOwner("1001")); err != nil {
		t.Fatal(err)
	}
	if f.services.Approval != nil || hasProposeTool(f) || goroutinesIn(approvalSweeperFrame) != 0 {
		t.Errorf("approvals switched off, but runtime=%v tool=%v sweepers=%d",
			f.services.Approval != nil, hasProposeTool(f), goroutinesIn(approvalSweeperFrame))
	}
}
