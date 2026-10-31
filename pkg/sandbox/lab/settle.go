package lab

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// KindSettle is the journal kind of one settle: {ok, wall_ms, polls, busy}.
const KindSettle = "settle"

// Wall caps of a settle (design 5.3: wall time). A step of an advance uses the default;
// the inspect op takes a cap up to the maximum, which stays under the 25 s in-instance
// call limit.
const (
	DefaultSettleCap = 10 * time.Second
	MaxSettleCap     = 25 * time.Second
)

// SettleWait settles the registry's components (see quiesce.Registry.Settle), journals
// one settle entry for the call, and returns how it went. A *quiesce.BusyError is the
// error of a settle that did not pass.
func (l *Lab) SettleWait(ctx context.Context, wallCap time.Duration) (quiesce.Report, error) {
	rep, err := l.Quiesce.SettleReport(ctx, wallCap)
	if l.Journal != nil {
		busy := rep.Busy
		if busy == nil {
			busy = []string{}
		}
		payload, _ := json.Marshal(struct {
			OK     bool     `json:"ok"`
			WallMS int64    `json:"wall_ms"`
			Polls  int      `json:"polls"`
			Busy   []string `json:"busy"`
		}{err == nil, rep.Wall.Milliseconds(), rep.Polls, busy})
		l.Journal.Append(labjournal.Entry{Kind: KindSettle, Source: "quiesce", Payload: payload})
	}
	return rep, err
}

// settleStep is what an advance waits on after each step: every registered component.
func (l *Lab) settleStep(ctx context.Context) error {
	_, err := l.SettleWait(ctx, DefaultSettleCap)
	return err
}
