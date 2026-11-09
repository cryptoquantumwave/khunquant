package scenario

import (
	"fmt"
	"slices"

	"github.com/cryptoquantumwave/khunquant/pkg/replay"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// ReplayLabels turns the message expectations of f that have a closed within window into
// the labels of a replay case, from the messages res recorded for them (ExpectResult.Sent).
// A positive expectation yields one should-fire label per distinct dedup key of the
// messages it matched (a prefix match, never contains), critical when one of them was; a
// count-0 expectation with replay_match yields a should-not-fire label. Whatever cannot
// become a label is named in skipped, with the reason: nothing is dropped silently.
// The windows are sim time (UTC), offsets from clock.start.
func ReplayLabels(f *File, res *Result) (labels []replay.Label, skipped []string) {
	for i := range f.Expect {
		e := &f.Expect[i]
		if e.Kind != "message" {
			continue
		}
		id := expectID(i, e)
		from, to, hasTo, _ := e.Within.Bounds()
		_, hi, _ := (labjournal.Match{Count: e.Count}).Bounds()
		var r *ExpectResult
		for k := range res.Expect {
			if res.Expect[k].ID == id {
				r = &res.Expect[k]
			}
		}
		note := f.Scenario + "/" + id
		switch {
		case !hasTo:
			skipped = append(skipped, id+": no within window with an end, so no label")
		case r == nil || !r.OK:
			skipped = append(skipped, id+": the expectation did not pass")
		case hi == 0 && e.ReplayMatch == "":
			skipped = append(skipped, id+": count 0 without replay_match, so no should-not-fire label")
		case hi == 0:
			labels = append(labels, replay.Label{Match: e.ReplayMatch, From: f.StartTime().Add(from).UTC(), To: f.StartTime().Add(to).UTC(), Note: note})
		default:
			var keys []string
			critical := map[string]bool{}
			for _, m := range r.Sent {
				if m.DedupKey == "" {
					continue
				}
				if !slices.Contains(keys, m.DedupKey) {
					keys = append(keys, m.DedupKey)
				}
				critical[m.DedupKey] = critical[m.DedupKey] || m.Severity == "critical"
			}
			if len(keys) == 0 {
				skipped = append(skipped, fmt.Sprintf("%s: the %d matched message(s) have no dedup key (not proactive sends)", id, len(r.Sent)))
			}
			for _, k := range keys {
				labels = append(labels, replay.Label{Match: k, From: f.StartTime().Add(from).UTC(), To: f.StartTime().Add(to).UTC(),
					ShouldFire: true, Critical: critical[k], Note: note})
			}
		}
	}
	return labels, skipped
}
