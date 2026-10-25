package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Report is what a replay found. It holds no ID and no wall-clock time, and every map
// is encoded in key order, so two replays of the same events under the same policy
// encode to the same bytes (--repeat asserts it).
type Report struct {
	Events EventsReport `json:"events"`
	// Days are the candidates and what attention did with them, by UTC date.
	Days   []DayReport `json:"days"`
	Totals DayReport   `json:"totals"`
	// LLMWakes is how many messages would have woken the model to compose: a group
	// with an escalate decision and no protective Critical. Cognition is a fake that
	// answers "silent" and the templates go out as they are (--llm off).
	LLMWakes int `json:"llm_wakes"`
	// Sends are the messages the owner would have received, in order.
	Sends  []Send        `json:"sends"`
	Labels *LabelsReport `json:"labels,omitempty"`
	// Engine counts how the pipeline did: errors here mean events it could not use.
	Engine EngineReport `json:"engine"`
}

// EventsReport describes the input.
type EventsReport struct {
	Total int       `json:"total"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// DayReport counts the candidates of one day by what attention decided. WouldSend is
// counted on the day the message went out, which can be later than the decision's.
type DayReport struct {
	Date         string         `json:"date"`
	Candidates   int            `json:"candidates"`
	Dispositions map[string]int `json:"dispositions"`
	WouldSend    int            `json:"would_send"`
}

// Send is one message the owner would have received.
type Send struct {
	At          time.Time `json:"at"`
	DedupKey    string    `json:"dedup_key"`
	Subject     string    `json:"subject"`
	Severity    string    `json:"severity"`
	Disposition string    `json:"disposition"`
	Protective  bool      `json:"protective,omitempty"`
	Title       string    `json:"title"`
	// Message numbers the message the decision went out in: decisions of one event
	// that were delivered together share it.
	Message int `json:"message"`
}

// EngineReport are the pipeline's counters.
type EngineReport struct {
	Processed        uint64 `json:"processed"`
	StageErrors      uint64 `json:"stage_errors"`
	StagePanics      uint64 `json:"stage_panics"`
	DispatchFailures uint64 `json:"dispatch_failures"`
	TriggerEvaluated uint64 `json:"trigger_evaluated"`
	TriggerErrors    uint64 `json:"trigger_errors"`
	TriggerInvalid   uint64 `json:"trigger_invalid_candidates"`
	// DeliveryErrors counts decisions attention would not mark delivered.
	DeliveryErrors uint64 `json:"delivery_errors"`
}

// LabelsReport scores the sends against the labels. A send counts as correct when it
// matches a should-fire label and falls inside its window; any other send (outside
// every window, or inside a should-not-fire one) is a false positive.
type LabelsReport struct {
	Labels         int     `json:"labels"`
	ShouldFire     int     `json:"should_fire"`
	Fired          int     `json:"fired"`
	WrongSends     int     `json:"wrong_sends"`
	Precision      float64 `json:"precision"`
	Recall         float64 `json:"recall"`
	CriticalLabels int     `json:"critical_labels"`
	CriticalFired  int     `json:"critical_fired"`
	// CriticalRecall must be 1; the command exits non-zero otherwise.
	CriticalRecall float64 `json:"critical_recall"`
	// Latency is how long after the start of its window each fired should-fire label
	// was first sent, in seconds.
	Latency LatencyReport `json:"latency_seconds"`
	Results []LabelResult `json:"results"`
}

// LatencyReport summarises detection latency.
type LatencyReport struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean"`
	Max   float64 `json:"max"`
}

// LabelResult is the outcome of one label.
type LabelResult struct {
	Match      string `json:"match"`
	ShouldFire bool   `json:"should_fire"`
	Critical   bool   `json:"critical,omitempty"`
	Sends      int    `json:"sends"`
	OK         bool   `json:"ok"`
	// LatencySeconds is set for a should-fire label that fired.
	LatencySeconds *float64 `json:"latency_seconds,omitempty"`
	Note           string   `json:"note,omitempty"`
}

// CriticalRecallOK reports whether every critical label fired (vacuously true with
// none, or without labels).
func (r *Report) CriticalRecallOK() bool { return r.Labels == nil || r.Labels.CriticalRecall >= 1 }

// JSON encodes the report, indented, with a final newline.
func (r *Report) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// matches reports whether a send is about what a label names: its dedup key starts
// with the match, or its subject is the match.
func (l Label) matches(s Send) bool {
	return strings.HasPrefix(s.DedupKey, l.Match) || s.Subject == l.Match
}

func (l Label) holds(s Send) bool {
	return l.matches(s) && !s.At.Before(l.From) && !s.At.After(l.To)
}

// score fills in the labels part of the report.
func score(labels []Label, sends []Send) *LabelsReport {
	lr := &LabelsReport{Labels: len(labels), Results: make([]LabelResult, 0, len(labels))}
	var lat []float64
	for _, l := range labels {
		res := LabelResult{Match: l.Match, ShouldFire: l.ShouldFire, Critical: l.Critical, Note: l.Note}
		var first *Send
		for i := range sends {
			if l.holds(sends[i]) {
				res.Sends++
				if first == nil {
					first = &sends[i]
				}
			}
		}
		if l.ShouldFire {
			lr.ShouldFire++
			res.OK = first != nil
			if res.OK {
				lr.Fired++
				d := round3(first.At.Sub(l.From).Seconds())
				res.LatencySeconds = &d
				lat = append(lat, d)
			}
			if l.Critical {
				lr.CriticalLabels++
				if res.OK {
					lr.CriticalFired++
				}
			}
		} else {
			res.OK = first == nil
		}
		lr.Results = append(lr.Results, res)
	}
	correct := 0
	for _, s := range sends {
		for _, l := range labels {
			if l.ShouldFire && l.holds(s) {
				correct++
				break
			}
		}
	}
	lr.WrongSends = len(sends) - correct
	lr.Precision, lr.Recall, lr.CriticalRecall = 1, 1, 1
	if len(sends) > 0 {
		lr.Precision = round3(float64(correct) / float64(len(sends)))
	}
	if lr.ShouldFire > 0 {
		lr.Recall = round3(float64(lr.Fired) / float64(lr.ShouldFire))
	}
	if lr.CriticalLabels > 0 {
		lr.CriticalRecall = round3(float64(lr.CriticalFired) / float64(lr.CriticalLabels))
	}
	if n := len(lat); n > 0 {
		sum := 0.0
		for _, x := range lat {
			sum += x
			lr.Latency.Max = math.Max(lr.Latency.Max, x)
		}
		lr.Latency.Count, lr.Latency.Mean = n, round3(sum/float64(n))
	}
	return lr
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// dayAcc accumulates one day.
type dayAcc struct {
	cands, send int
	disp        map[string]int
}

func dayReports(days map[string]*dayAcc) (out []DayReport, total DayReport) {
	total = DayReport{Date: "total", Dispositions: map[string]int{}}
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := days[k]
		out = append(out, DayReport{Date: k, Candidates: a.cands, Dispositions: a.disp, WouldSend: a.send})
		total.Candidates += a.cands
		total.WouldSend += a.send
		for d, n := range a.disp {
			total.Dispositions[d] += n
		}
	}
	return out, total
}

func isSend(d proactive.Disposition) bool {
	return d == proactive.DispNotify || d == proactive.DispEscalate
}

// ErrNondeterministic is returned by Repeat when two runs differ.
type ErrNondeterministic struct {
	Run    int
	Detail string
}

func (e *ErrNondeterministic) Error() string {
	return fmt.Sprintf("replay: run %d differs from run 1 (%s)", e.Run, e.Detail)
}

// Repeat runs once per k and asserts the encoded reports are identical (pass^k). It
// returns the first run's report.
func Repeat(k int, run func() (*Report, error)) (*Report, error) {
	if k < 1 {
		k = 1
	}
	var first *Report
	var want []byte
	for i := 1; i <= k; i++ {
		rep, err := run()
		if err != nil {
			return nil, err
		}
		got, err := rep.JSON()
		if err != nil {
			return nil, err
		}
		if i == 1 {
			first, want = rep, got
			continue
		}
		if !bytes.Equal(got, want) {
			return first, &ErrNondeterministic{Run: i, Detail: firstDiff(want, got)}
		}
	}
	return first, nil
}

// firstDiff names the first line at which a and b differ.
func firstDiff(a, b []byte) string {
	la, lb := bytes.Split(a, []byte("\n")), bytes.Split(b, []byte("\n"))
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = strings.TrimSpace(string(la[i]))
		}
		if i < len(lb) {
			y = strings.TrimSpace(string(lb[i]))
		}
		if x != y {
			return fmt.Sprintf("line %d: %q vs %q", i+1, x, y)
		}
	}
	return "length"
}
