package detect

import (
	"math"
	"time"
)

// ReturnSeries samples a price at fixed time buckets and feeds the log return
// between consecutive bucket closes to an EWMA.
//
// A bucket is the half-open interval [k*Bucket, (k+1)*Bucket) of the Unix
// timeline. The close of a bucket is the last price seen in it, and a bucket
// closes when the first price of a later bucket arrives. The return is
// ln(close / previous close). When up to MaxGap buckets passed between the two
// closes (the series had no price for a while) the return is divided by
// sqrt(gap), because the variance of a random walk grows with time; beyond MaxGap
// the series simply re-bases on the new close and records no return, so a long
// silence is never scored as one huge move.
type ReturnSeries struct {
	Bucket time.Duration `json:"bucket_ns"`
	MaxGap int           `json:"max_gap"`
	E      EWMA          `json:"ewma"`

	Open    bool    `json:"open"`
	Cur     int64   `json:"cur"`      // index of the open bucket
	Last    float64 `json:"last"`     // last price seen in the open bucket
	Prev    float64 `json:"prev"`     // close of the last closed bucket; 0 when none
	PrevIdx int64   `json:"prev_idx"` // that bucket's index
}

// BucketClose describes a bucket that has just closed.
type BucketClose struct {
	Index     int64
	Close     float64
	PrevClose float64 // 0 when there was no earlier close
	// HasReturn says Return was computed (the previous close was within MaxGap).
	HasReturn bool
	Return    float64 // scaled log return
	Z         float64 // score of Return against the EWMA before it was folded in
	ZValid    bool
}

// Observe folds a price seen at t into the series. closed is true when the price
// belongs to a later bucket than the open one, in which case c describes the
// bucket that just closed. A price that is not positive and finite, a time before
// the epoch and a price older than the open bucket are ignored.
func (r *ReturnSeries) Observe(t time.Time, price float64) (c BucketClose, closed bool) {
	if !finite(price) || price <= 0 || r.Bucket <= 0 {
		return c, false
	}
	ns := t.UnixNano()
	if ns < 0 {
		return c, false
	}
	idx := ns / int64(r.Bucket)
	switch {
	case !r.Open:
		r.Open, r.Cur, r.Last = true, idx, price
		return c, false
	case idx < r.Cur:
		return c, false
	case idx == r.Cur:
		r.Last = price
		return c, false
	}

	c = BucketClose{Index: r.Cur, Close: r.Last, PrevClose: r.Prev}
	if r.Prev > 0 {
		if gap := r.Cur - r.PrevIdx; gap >= 1 && gap <= int64(max(r.MaxGap, 1)) {
			c.HasReturn = true
			c.Return = math.Log(r.Last/r.Prev) / math.Sqrt(float64(gap))
			c.Z, c.ZValid = r.E.Update(c.Return)
		}
	}
	r.Prev, r.PrevIdx = r.Last, r.Cur
	r.Cur, r.Last = idx, price
	return c, true
}
