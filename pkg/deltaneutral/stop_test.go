package deltaneutral

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

// dn.open before its first place step may be cancelled; once a place step has begun
// it is left to settle (finalize starts the recovery of a filled leg). dn.resize is
// protective and says what it does by direction.
func TestFlowOrders_DeltaNeutral(t *testing.T) {
	d := &Deps{}
	ctx := context.Background()
	in, _ := json.Marshal(OpenInput{PlanID: 4})
	open := journal.Workflow{ID: "wf1", Kind: KindOpen, Input: in}
	rec := func(name string, finished bool) journal.StepRecord {
		r := journal.StepRecord{Name: name, StartedAt: time.Unix(1, 0)}
		if finished {
			r.FinishedAt = time.Unix(2, 0)
		}
		return r
	}

	fo, err := d.FlowOrders(ctx, open, []journal.StepRecord{rec("validate", true), rec("set_leverage", true)})
	if err != nil || fo.Settle || fo.Protect {
		t.Errorf("before a place step: %+v, %v", fo, err)
	}
	fo, err = d.FlowOrders(ctx, open, []journal.StepRecord{rec("validate", true), rec("place_first_leg", false)})
	if err != nil || !fo.Settle || len(fo.Orders) != 0 {
		t.Errorf("a cut first leg must settle, not be cancelled: %+v, %v", fo, err)
	}

	for dir, want := range map[string]string{DirDecrease: "reduces exposure", DirIncrease: "refuses"} {
		rin, _ := json.Marshal(ResizeInput{PlanID: 4, Direction: dir})
		fo, err := d.FlowOrders(ctx, journal.Workflow{Kind: KindResize, Input: rin}, nil)
		if err != nil || !fo.Protect || !strings.Contains(fo.Note, want) {
			t.Errorf("resize %s: %+v, %v", dir, fo, err)
		}
	}
	if fo, _ := d.FlowOrders(ctx, journal.Workflow{Kind: KindUnwind}, nil); fo.Settle || len(fo.Orders) != 0 {
		t.Errorf("unwind: %+v", fo)
	}
}
