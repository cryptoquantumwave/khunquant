package scenario

import (
	"context"
	"errors"
	"fmt"
)

// expectID names an expectation in reports: its id, else expect-<n>.
func expectID(i int, e *Expectation) string {
	if e.ID != "" {
		return e.ID
	}
	return fmt.Sprintf("expect-%d", i)
}

// expect evaluates one expectation.
func (s *runState) expect(_ context.Context, i int, e *Expectation) ExpectResult {
	return ExpectResult{ID: expectID(i, e), Kind: e.Kind, Want: e.Kind + " expectation", OK: true,
		Got: "not evaluated: expectation evaluation arrives with SBX-11b-3"}
}

// untilOptions reads the {until: <expectation>, step, max_sim, max_wall} form of run_until.
func (s *runState) untilOptions(*Step, *UntilOptions) error {
	return errors.New("run_until with until: arrives with SBX-11b-2")
}
