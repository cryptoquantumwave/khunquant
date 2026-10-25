package detect

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// TestEWMA_ZScore walks a hand-computed sequence. With HalfLife 1 alpha is 0.5:
// 10 starts the state; 12 is scored with sigma 0 (no z), then mean 11 and var 1;
// 13 is therefore 2 sigma above the mean.
func TestEWMA_ZScore(t *testing.T) {
	e := EWMA{HalfLife: 1, MinSamples: 1}
	if _, ok := e.Update(10); ok {
		t.Fatal("the first sample cannot be scored")
	}
	if z, ok := e.Update(12); ok || z != 0 {
		t.Fatalf("a zero-sigma state must not score: z=%v ok=%v", z, ok)
	}
	if !near(e.Mean, 11, 1e-12) || !near(e.Var, 1, 1e-12) {
		t.Fatalf("state after 10,12 = mean %v var %v, want 11 and 1", e.Mean, e.Var)
	}
	z, ok := e.Update(13)
	if !ok || !near(z, 2, 1e-12) {
		t.Fatalf("z of 13 = %v ok=%v, want 2", z, ok)
	}
}

func TestEWMA_ShockScoresAgainstStateBeforeIt(t *testing.T) {
	e := EWMA{HalfLife: 30, MinSamples: 10, MinSigma: 1e-6}
	for i := 0; i < 200; i++ {
		e.Update(math.Sin(float64(i)) * 0.001)
	}
	z, ok := e.Update(0.02)
	if !ok || z < 10 {
		t.Fatalf("a 20x shock scored z=%v ok=%v, want a large valid z", z, ok)
	}
}

func TestEWMA_WarmupAndFloor(t *testing.T) {
	e := EWMA{HalfLife: 5, MinSamples: 5, MinSigma: 0.01}
	for i := 0; i < 5; i++ {
		if _, ok := e.Update(1); ok {
			t.Fatalf("sample %d scored during warm-up", i)
		}
	}
	// A flat series has zero variance; the floor keeps z finite.
	z, ok := e.Update(1.05)
	if !ok || math.IsInf(z, 0) || math.IsNaN(z) || !near(z, 5, 1e-9) {
		t.Fatalf("z over the sigma floor = %v ok=%v, want 5", z, ok)
	}
}

func TestEWMA_NonFiniteIsIgnored(t *testing.T) {
	e := EWMA{HalfLife: 3, MinSamples: 1}
	e.Update(1)
	e.Update(2)
	before := e
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if z, ok := e.Update(x); z != 0 || ok {
			t.Fatalf("Update(%v) = %v, %v", x, z, ok)
		}
	}
	if e != before {
		t.Fatalf("a non-finite sample changed the state: %+v -> %+v", before, e)
	}
}

// TestCUSUM_DetectsShift: a sustained +1 sigma shift with K=0.5 adds 0.5 per
// sample, so Pos first exceeds H=5 on the 11th.
func TestCUSUM_DetectsShift(t *testing.T) {
	c := CUSUM{K: 0.5, H: 5}
	for i := 1; i <= 10; i++ {
		if fired, _, _ := c.Update(1); fired {
			t.Fatalf("fired early, at sample %d", i)
		}
	}
	fired, dir, stat := c.Update(1)
	if !fired || dir != 1 || !near(stat, 5.5, 1e-12) {
		t.Fatalf("11th sample: fired=%v dir=%d stat=%v, want true, +1, 5.5", fired, dir, stat)
	}
	if c.Pos != 0 || c.Neg != 0 {
		t.Fatalf("sums not reset after a fire: %+v", c)
	}
	// And the other direction.
	for i := 0; i < 10; i++ {
		c.Update(-1)
	}
	if fired, dir, _ := c.Update(-1); !fired || dir != -1 {
		t.Fatalf("downward shift: fired=%v dir=%d", fired, dir)
	}
}

func TestCUSUM_QuietNoiseNeverFires(t *testing.T) {
	c := CUSUM{K: 0.5, H: 5}
	for i := 0; i < 10000; i++ {
		z := 0.4
		if i%2 == 1 {
			z = -0.4
		}
		if fired, _, _ := c.Update(z); fired {
			t.Fatalf("fired on noise inside the allowance at %d", i)
		}
	}
	before := c
	if fired, _, _ := c.Update(math.NaN()); fired || c != before {
		t.Fatal("NaN changed the CUSUM")
	}
}

func TestHysteresis_CrossingNeedsAKnownSide(t *testing.T) {
	h := Hysteresis{Lower: 10, Upper: 20}
	steps := []struct {
		x       float64
		side    Side
		crossed bool
	}{
		{15, SideUnknown, false}, // inside the band: nothing known
		{25, SideAbove, false},   // first side is not a crossing
		{19, SideAbove, false},   // back inside the band: still above
		{21, SideAbove, false},   // jitter around Upper does not re-fire
		{9, SideBelow, true},     // through the band
		{12, SideBelow, false},
		{20, SideAbove, true}, // Upper itself counts as above
		{math.NaN(), SideAbove, false},
	}
	for i, s := range steps {
		side, crossed := h.Update(s.x)
		if side != s.side || crossed != s.crossed {
			t.Fatalf("step %d (x=%v): side=%d crossed=%v, want %d %v", i, s.x, side, crossed, s.side, s.crossed)
		}
	}
}

var t0 = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

func newRS() *ReturnSeries {
	return &ReturnSeries{Bucket: time.Minute, MaxGap: 3, E: EWMA{HalfLife: 10, MinSamples: 2, MinSigma: 1e-6}}
}

func TestReturnSeries_CloseOnlyWhenALaterBucketArrives(t *testing.T) {
	r := newRS()
	for i, p := range []float64{100, 101, 102} { // all inside minute 0
		if _, closed := r.Observe(t0.Add(time.Duration(i)*10*time.Second), p); closed {
			t.Fatalf("closed inside a bucket at tick %d", i)
		}
	}
	c, closed := r.Observe(t0.Add(time.Minute), 103)
	if !closed || c.Close != 102 || c.PrevClose != 0 || c.HasReturn {
		t.Fatalf("first close = %+v closed=%v, want close 102, no previous, no return", c, closed)
	}
	c, closed = r.Observe(t0.Add(2*time.Minute), 104.04)
	if !closed || !c.HasReturn || !near(c.Return, math.Log(103.0/102.0), 1e-12) {
		t.Fatalf("second close = %+v", c)
	}
}

func TestReturnSeries_GapScalesAndLongGapRebases(t *testing.T) {
	r := newRS()
	r.Observe(t0, 100)
	r.Observe(t0.Add(time.Minute), 100) // closes bucket 0 at 100
	// Two empty buckets, then a price: the close of bucket 1 (100) against bucket
	// 0's (100) has gap 1; make a real gap of 2 by skipping one bucket.
	c, _ := r.Observe(t0.Add(3*time.Minute), 110) // closes bucket 1 at 100
	if c.Close != 100 {
		t.Fatalf("close = %v", c.Close)
	}
	c, closed := r.Observe(t0.Add(5*time.Minute), 121) // closes bucket 3 at 110; previous close was bucket 1: gap 2
	if !closed || !c.HasReturn || !near(c.Return, math.Log(1.1)/math.Sqrt(2), 1e-12) {
		t.Fatalf("a gap of 2 buckets must scale by sqrt(2): %+v", c)
	}
	// A gap beyond MaxGap re-bases without a return.
	c, closed = r.Observe(t0.Add(30*time.Minute), 200) // closes bucket 5 at 121; previous bucket 3: gap 2 -> return
	if !closed || !c.HasReturn {
		t.Fatalf("%+v", c)
	}
	c, closed = r.Observe(t0.Add(60*time.Minute), 210) // closes bucket 30 at 200; previous bucket 5: gap 25 > 3
	if !closed || c.HasReturn {
		t.Fatalf("a long silence must not be scored as one move: %+v", c)
	}
}

func TestReturnSeries_IgnoresBadAndOldInput(t *testing.T) {
	r := newRS()
	r.Observe(t0.Add(time.Minute), 100)
	before := *r
	for _, p := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, closed := r.Observe(t0.Add(2*time.Minute), p); closed {
			t.Fatalf("closed on price %v", p)
		}
	}
	if _, closed := r.Observe(t0, 50); closed || *r != before { // an older bucket
		t.Fatalf("an older price changed the series: %+v", *r)
	}
	if _, closed := r.Observe(time.Unix(-5, 0), 50); closed || *r != before {
		t.Fatal("a time before the epoch changed the series")
	}
}

func TestDetectors_StateSurvivesJSON(t *testing.T) {
	r := newRS()
	for i := 0; i < 8; i++ {
		r.Observe(t0.Add(time.Duration(i)*time.Minute), 100+float64(i))
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back ReturnSeries
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != *r {
		t.Fatalf("round trip changed the state:\n%+v\n%+v", *r, back)
	}
}

// TestDetect_ImportBoundary pins the package to the standard library, apart from
// the test-only boundary helper.
func TestDetect_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/worldmodel/detect",
		Allowed: nil,
	})
}
