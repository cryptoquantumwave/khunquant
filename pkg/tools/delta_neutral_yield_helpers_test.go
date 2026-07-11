package tools

import (
	"bytes"
	"strings"
	"testing"
	"time"

	chart "github.com/wcharczuk/go-chart/v2"
	"github.com/wcharczuk/go-chart/v2/drawing"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

func TestRenderDeltaNeutralYieldChartTool_NameDescriptionParameters(t *testing.T) {
	store, err := deltaneutral.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	tool := NewRenderDeltaNeutralYieldChartTool(store)
	if tool.Name() != NameRenderDeltaNeutralYieldChart {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameRenderDeltaNeutralYieldChart)
	}
	if tool.Description() != DescRenderDeltaNeutralYieldChart {
		t.Errorf("Description() mismatch")
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"plan_id", "period", "columns"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

// --- yieldDigest index selection ---

func makePoints(n int) []deltaneutral.SnapshotSeriesPoint {
	pts := make([]deltaneutral.SnapshotSeriesPoint, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range pts {
		pts[i] = deltaneutral.SnapshotSeriesPoint{
			CheckedAt:          base.Add(time.Duration(i) * time.Hour),
			CurrentFundingRate: 0.0001 * float64(i+1),
			FundingAPYPct:      float64(i+1) * 10.95,
			EarnAPYPct:         3.2,
			CombinedAPYPct:     float64(i+1)*10.95 + 3.2,
		}
	}
	return pts
}

func pickDigest(pts []deltaneutral.SnapshotSeriesPoint) []deltaneutral.SnapshotSeriesPoint {
	if len(pts) == 0 {
		return nil
	}
	n := len(pts)
	seen := make(map[int]bool)
	var result []deltaneutral.SnapshotSeriesPoint
	for _, idx := range []int{0, n / 2, n - 1} {
		if !seen[idx] {
			seen[idx] = true
			result = append(result, pts[idx])
		}
	}
	return result
}

func TestYieldDigestIndexSelection(t *testing.T) {
	tests := []struct {
		name     string
		n        int
		wantLen  int
		wantIdxs []int // expected index into makePoints(n)
	}{
		{"0 points", 0, 0, nil},
		{"1 point", 1, 1, []int{0}},
		{"2 points", 2, 2, []int{0, 1}},
		{"3 points", 3, 3, []int{0, 1, 2}},
		{"4 points — mid deduped", 4, 3, []int{0, 2, 3}},
		{"10 points", 10, 3, []int{0, 5, 9}},
		{"51 points", 51, 3, []int{0, 25, 50}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.n == 0 {
				got := pickDigest(nil)
				if len(got) != 0 {
					t.Errorf("expected 0 points, got %d", len(got))
				}
				return
			}
			pts := makePoints(tc.n)
			got := pickDigest(pts)
			if len(got) != tc.wantLen {
				t.Errorf("len=%d want=%d", len(got), tc.wantLen)
			}
			for i, idx := range tc.wantIdxs {
				if i >= len(got) {
					break
				}
				if got[i] != pts[idx] {
					t.Errorf("result[%d]: got point at original index != %d", i, idx)
				}
			}
		})
	}
}

// --- formatYieldDigest output ---

func TestFormatYieldDigestEmpty(t *testing.T) {
	if got := formatYieldDigest(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestFormatYieldDigestContents(t *testing.T) {
	pts := makePoints(3)
	out := formatYieldDigest(pts)

	if !strings.Contains(out, "Yield History") {
		t.Error("missing header")
	}
	for _, lbl := range []string{"[first]", "[mid]", "[latest]"} {
		if !strings.Contains(out, lbl) {
			t.Errorf("missing label %s", lbl)
		}
	}
	if !strings.Contains(out, "fundAPY") || !strings.Contains(out, "combined") {
		t.Error("missing APY fields")
	}
}

// --- parsePeriodSince ---

func TestParsePeriodSince(t *testing.T) {
	tests := []struct {
		input string
		label string
		zero  bool // true = all time (zero time)
	}{
		{"7d", "7d", false},
		{"14d", "14d", false},
		{"30d", "30d", false},
		{"3m", "3m", false},
		{"6m", "6m", false},
		{"all", "all", true},
		{"", "7d", false},
		{"bogus", "7d", false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, lbl := parsePeriodSince(tc.input)
			if lbl != tc.label {
				t.Errorf("label=%q want=%q", lbl, tc.label)
			}
			if tc.zero && !got.IsZero() {
				t.Error("expected zero time for 'all'")
			}
			if !tc.zero && got.IsZero() {
				t.Error("expected non-zero time")
			}
		})
	}
}

// --- chart render smoke test ---

func TestChartRenderPNG(t *testing.T) {
	pts := makePoints(20)

	xVals := make([]time.Time, len(pts))
	frVals := make([]float64, len(pts))
	apyVals := make([]float64, len(pts))
	for i, p := range pts {
		xVals[i] = p.CheckedAt
		frVals[i] = p.CurrentFundingRate
		apyVals[i] = p.CombinedAPYPct
	}

	secondary := chart.TimeSeries{
		Name:    "Combined APY%",
		Style:   chart.Style{StrokeColor: drawing.ColorFromHex("ef4444")},
		XValues: xVals,
		YValues: apyVals,
	}
	secondary.YAxis = chart.YAxisSecondary

	g := chart.Chart{
		Width:  400,
		Height: 200,
		Series: []chart.Series{
			chart.TimeSeries{
				Name:    "Funding Rate",
				Style:   chart.Style{StrokeColor: drawing.ColorFromHex("6366f1")},
				XValues: xVals,
				YValues: frVals,
			},
			secondary,
		},
	}

	var buf bytes.Buffer
	if err := g.Render(chart.PNG, &buf); err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty PNG output")
	}
	// Check PNG magic bytes.
	magic := buf.Bytes()[:8]
	pngMagic := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	for i, b := range pngMagic {
		if magic[i] != b {
			t.Fatalf("not a valid PNG: magic bytes mismatch at %d", i)
		}
	}
}
