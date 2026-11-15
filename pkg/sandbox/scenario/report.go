package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// JSON is the report as indented JSON.
func (r *Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Verdict is "pass", "FAIL", "skipped" or "infra", for a one-line summary.
func (r *Report) Verdict() string {
	switch {
	case r.Skipped != "":
		return "skipped"
	case r.ExitCode() == ExitInfra:
		return "infra"
	case r.ExitCode() == ExitInvalid:
		return "invalid"
	case r.Pass:
		return "pass"
	}
	return "FAIL"
}

// RowLines is one line per matrix row ("[model=a]: pass (flaky, 8 of 10)"), and for a
// scenario with no matrix a line only when its runs were flaky (the scenario's own verdict
// says when it failed).
func (r *Report) RowLines() []string {
	var out []string
	for _, w := range r.Rows {
		if len(w.Matrix) > 0 || (w.Pass && w.Passed < w.Runs) {
			line := w.Label()
			if len(w.Matrix) > 0 {
				line = strings.TrimSpace(matrixLabel(w.Matrix)) + ": " + line
			}
			out = append(out, line)
		}
	}
	return out
}

// Markdown is the report a person or an agent acts on: per run, what failed with want,
// got, the journal entries around it and the model's calls.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Scenario %s: %s\n\n", strings.TrimSpace(r.Scenario+" "+r.Tag()), r.Verdict())
	if r.Skipped != "" {
		fmt.Fprintf(&b, "Skipped: %s.\n", r.Skipped)
		return b.String()
	}
	fmt.Fprintf(&b, "%d of %d runs passed.\n", r.Passed, len(r.Runs))
	for _, line := range r.RowLines() {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	for _, res := range r.Runs {
		verdict := map[bool]string{true: "pass", false: "FAIL"}[res.Pass]
		fmt.Fprintf(&b, "\n## Run %d%s: %s (%s of wall time, sim start %s)\n\n", res.Run, matrixLabel(res.Matrix), verdict,
			res.Wall.Round(time.Millisecond), res.StartSim.Format(time.RFC3339))
		for _, i := range res.Issues {
			fmt.Fprintf(&b, "- Not runnable here: %s\n", i)
		}
		if res.Infra != "" {
			fmt.Fprintf(&b, "- Infrastructure: %s\n", res.Infra)
		}
		if len(res.Settle) > 0 {
			fmt.Fprintf(&b, "- Still busy when the settle gave up: %s\n", strings.Join(res.Settle, ", "))
		}
		if res.Failure != "" {
			fmt.Fprintf(&b, "- Step failed: %s\n", res.Failure)
		}
		if res.Home != "" {
			fmt.Fprintf(&b, "- Instance home kept at %s\n", res.Home)
		}
		for _, e := range res.Expect {
			mark := map[bool]string{true: "ok", false: "FAILED"}[e.OK]
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", e.ID, e.Kind, mark)
			if e.OK {
				continue
			}
			fmt.Fprintf(&b, "  - want: %s\n  - got: %s\n", e.Want, e.Got)
			if len(e.Excerpt) > 0 {
				b.WriteString("\n  Journal around it:\n\n```\n")
				for _, en := range e.Excerpt {
					fmt.Fprintf(&b, "%s\n", excerptLine(en))
				}
				b.WriteString("```\n")
			}
		}
		if len(res.LLMCalls) > 0 && !res.Pass {
			b.WriteString("\nModel calls (newest last):\n\n```\n")
			for _, c := range res.LLMCalls {
				fmt.Fprintf(&b, "%s\n", clip(string(c)))
			}
			b.WriteString("```\n")
		}
	}
	return b.String()
}

func excerptLine(e labjournal.Entry) string {
	return fmt.Sprintf("#%d %s %s %s %s", e.Seq, e.SimTime.Format("15:04:05"), e.Kind, e.Subject, clip(string(e.Payload)))
}
