package scenario

import (
	"encoding/json"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// ExpectResult is the outcome of one expectation (or of an implicit one: settled,
// journal_drops).
type ExpectResult struct {
	ID          string
	Kind        string
	OK          bool
	Want, Got   string
	MatchedSeqs []int64            // journal seqs that matched (SBX-21 reads them)
	Excerpt     []labjournal.Entry // entries around the window, at most 20

	err error // the journal could not be read (OK is false then)
}

// StepMark is where a step ran: the sim time after it and the newest journal seq.
type StepMark struct {
	Phase string // setup | timeline
	Index int
	Kind  string
	Sim   time.Time
	Seq   int64
}

// Result is one run of one matrix row.
type Result struct {
	Scenario string
	Matrix   map[string]string
	Run      int
	Pass     bool
	StartSim time.Time
	Expect   []ExpectResult
	Steps    []StepMark
	LLMCalls []json.RawMessage
	Drops    uint64
	Wall     time.Duration
	Home     string // the instance home, kept when a failed run asked for it

	// Why the run did not pass besides an expectation: a step that failed (Failure), a
	// settle that did not pass (Settle names the busy components), the instance (Infra),
	// or a feature the instance lacks (Issues).
	Failure string
	Settle  []string
	Infra   string
	Issues  []Issue
}

// Report is the outcome of a scenario: every run of every matrix row.
type Report struct {
	Scenario string
	Status   Status
	Skipped  string
	Runs     []*Result
	Passed   int
	Pass     bool // Passed/len(Runs) >= pass_rate
}

// Process exit codes of a run, as `khunquant sandbox run` reports them.
const (
	ExitFailed  = 1 // an expectation failed
	ExitInvalid = 2 // an invalid scenario
	ExitInfra   = 3 // the instance would not start, a settle cap, a lost instance
)

// ExitCode is the status a run of this scenario ends the process with: 0 when it
// passed or was skipped; 2 for a feature the instance lacks, 3 for infrastructure
// (which wins over a failed expectation: the run says nothing about the code), else 1.
func (r *Report) ExitCode() int {
	code := 0
	for _, res := range r.Runs {
		switch {
		case len(res.Issues) > 0:
			return ExitInvalid
		case res.Infra != "" || len(res.Settle) > 0:
			code = ExitInfra
		case !res.Pass && code == 0:
			code = ExitFailed
		}
	}
	if code == ExitFailed && r.Pass {
		return 0 // a failed run inside pass_rate
	}
	return code
}
