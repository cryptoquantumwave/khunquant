package lab

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// KindSettle is the journal kind of one settle: {ok, cut, wall_ms, polls, busy}. cut is
// true when the caller's context ended the wait (an advance's wall budget, a client that
// went away) before the wall cap: the settle did not pass, but nothing was found stuck
// either, so a reader counts it as interrupted, not as failed.
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
			Cut    bool     `json:"cut"`
			WallMS int64    `json:"wall_ms"`
			Polls  int      `json:"polls"`
			Busy   []string `json:"busy"`
		}{err == nil, IsCut(err), rep.Wall.Milliseconds(), rep.Polls, busy})
		l.Journal.Append(labjournal.Entry{Kind: KindSettle, Source: "quiesce", Payload: payload})
	}
	return rep, err
}

// IsCut says whether err is a settle that the caller's context ended before its wall cap.
func IsCut(err error) bool {
	var be *quiesce.BusyError
	return errors.As(err, &be) && be.Cause != nil
}

// settleStep is what an advance waits on after each step: every registered component.
func (l *Lab) settleStep(ctx context.Context) error {
	_, err := l.SettleWait(ctx, DefaultSettleCap)
	return err
}
