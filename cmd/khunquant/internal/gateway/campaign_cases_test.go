package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// B9-5, through the production reload path (handleConfigReload): approvals switched off while
// a card could be pending, then on again. While off the dispatcher has no approval service, so an
// /approve is answered "not available" and decides nothing; after the re-enable it is wired again
// and an unknown code is still "no pending proposal" (a decision needs a pending card, and the
// card's survival without a placement is proved in pkg/approval: TestB95_*).
func TestB95_ReloadWithApprovalsOffAnswersNotAvailable(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals)
	if f.services.Control.disp.Deps().Approvals == nil {
		t.Fatal("approvals are on, but the dispatcher has no approval service")
	}

	if err := f.reload(withOwner("1001")); err != nil { // approvals off
		t.Fatal(err)
	}
	if f.services.Approval != nil || f.services.Control.disp.Deps().Approvals != nil {
		t.Fatalf("approvals off, but runtime=%v dispatcher=%v", f.services.Approval != nil, f.services.Control.disp.Deps().Approvals != nil)
	}
	rep, ok := f.services.Control.disp.TryHandle(context.Background(), ownerMsg("/approve 7F3K"))
	if !ok || !strings.Contains(rep.Text, "not available") {
		t.Fatalf("/approve while approvals are off: %+v, %v", rep, ok)
	}

	if err := f.reload(withApprovals); err != nil { // approvals on again
		t.Fatal(err)
	}
	if f.services.Control.disp.Deps().Approvals == nil {
		t.Fatal("approvals re-enabled, but the dispatcher has no approval service")
	}
	rep, ok = f.services.Control.disp.TryHandle(context.Background(), ownerMsg("/approve 7F3K"))
	if !ok || !strings.Contains(rep.Text, "No pending proposal 7F3K") {
		t.Fatalf("/approve of a code that was never proposed: %+v, %v", rep, ok)
	}
}

// B9-6, through the production reload path: the owner changed by a reload. The previous owner can
// no longer decide: the new generation's service and dispatcher both read the new configuration.
func TestB96_ReloadChangingTheOwnerRetiresTheOldOwnersDecisions(t *testing.T) {
	f := newAssemblyFixture(t, withApprovals) // owner 1001
	if err := f.reload(func(c *config.Config) { withApprovals(c); withOwner("2002")(c) }); err != nil {
		t.Fatal(err)
	}
	rep, ok := f.services.Control.disp.TryHandle(context.Background(), ownerMsg("/approve 7F3K")) // sender 1001
	if !ok || !strings.Contains(rep.Text, "Not authorized") {
		t.Fatalf("the old owner's /approve after the reload: %+v, handled=%v; want \"Not authorized\"", rep, ok)
	}
}
