package control

import (
	"fmt"
	"sort"
	"strings"
)

// StopSummary is what /stop did, built by code from what each step reported. It is
// the reply to the command and the body of POST /api/control/kill: counts and names
// only (workflow kinds, "buy BTC/USDT on okx"), never an id, a price, a credential
// or a line from the model.
type StopSummary struct {
	// Engaged: the kill flag is set. AlreadyEngaged: it was set before this run.
	Engaged        bool `json:"engaged"`
	AlreadyEngaged bool `json:"already_engaged,omitempty"`
	// Busy: another stop was still running, so this call did nothing but say so.
	Busy bool `json:"busy,omitempty"`
	// Pending: some steps did not finish inside the reply budget and go on in the
	// background; their outcome follows as a second message.
	Pending bool `json:"pending,omitempty"`

	TurnsCancelled int `json:"turns_cancelled"`
	// WorkflowsCancelled are kinds of workflows cancelled at their next step boundary
	// (WorkflowsStopping: cancel requested, not yet at a boundary).
	WorkflowsCancelled []string `json:"workflows_cancelled,omitempty"`
	WorkflowsStopping  []string `json:"workflows_stopping,omitempty"`
	// WorkflowsProtective keep running because they only reduce risk;
	// WorkflowsFinishing because cutting them would skip the steps that record an
	// order or hedge a leg. Neither places anything more.
	WorkflowsProtective []string `json:"workflows_protective,omitempty"`
	WorkflowsFinishing  []string `json:"workflows_finishing,omitempty"`

	ProposalsCancelled int `json:"proposals_cancelled"`
	MandatesRevoked    int `json:"mandates_revoked"`

	OrdersCancelled []string `json:"orders_cancelled,omitempty"`
	// OrdersClosed were found already filled or closed: nothing to withdraw.
	OrdersClosed []string `json:"orders_closed,omitempty"`
	// ExitOrdersLeft are exits the agent placed (reduce-only): not withdrawn.
	ExitOrdersLeft []string `json:"exit_orders_left,omitempty"`
	// OrdersMayExist could not be shown to exist or not to: check the exchange.
	OrdersMayExist []string `json:"orders_may_exist,omitempty"`
	// Errors are steps that failed; later steps ran regardless.
	Errors []string `json:"errors,omitempty"`
}

func (s StopSummary) clone() StopSummary {
	c := s
	for _, p := range []*[]string{&c.WorkflowsCancelled, &c.WorkflowsStopping, &c.WorkflowsProtective, &c.WorkflowsFinishing,
		&c.OrdersCancelled, &c.OrdersClosed, &c.ExitOrdersLeft, &c.OrdersMayExist, &c.Errors} {
		*p = append([]string(nil), *p...)
	}
	return c
}

// Text is the owner's reply.
func (s StopSummary) Text() string {
	if s.Busy {
		if !s.Engaged {
			return "A stop is already running, but the kill flag could not be set again. Try /stop again."
		}
		return "A stop is already running; the kill flag is engaged and its summary arrives in this chat when it ends."
	}
	var b strings.Builder
	switch {
	case !s.Engaged:
		b.WriteString("WARNING: the kill flag could not be set, so automatic actions may continue. Try /stop again. ")
	case s.AlreadyEngaged:
		b.WriteString("Stopped again (it was already stopped). ")
	default:
		b.WriteString("Stopped. ")
	}
	fmt.Fprintf(&b, "Cancelled %s, %s%s, %s; revoked %s; cancelled %s.",
		count(s.TurnsCancelled, "turn", "turns"), count(len(s.WorkflowsCancelled), "workflow", "workflows"), kindList(s.WorkflowsCancelled),
		count(s.ProposalsCancelled, "proposal", "proposals"), count(s.MandatesRevoked, "mandate", "mandates"),
		count(len(s.OrdersCancelled), "open order", "open orders"))
	line := func(prefix string, items []string) {
		if len(items) > 0 {
			b.WriteString("\n" + prefix + strings.Join(items, "; "))
		}
	}
	line("Protective, still running: ", countKinds(s.WorkflowsProtective))
	line("Finishing without placing anything more: ", countKinds(s.WorkflowsFinishing))
	line("Still stopping at their next step: ", countKinds(s.WorkflowsStopping))
	line("Cancelled orders: ", s.OrdersCancelled)
	line("Already filled or closed, nothing to cancel: ", s.OrdersClosed)
	line("Exit orders left in place (they reduce risk): ", s.ExitOrdersLeft)
	line("May exist: check the exchange: ", s.OrdersMayExist)
	line("Did not complete: ", s.Errors)
	if s.Pending {
		b.WriteString("\nSome steps are still running; the rest follows in a second message.")
	}
	b.WriteString("\nMonitoring and notifications stay on. Use /resume to allow approvals again; mandates must be approved again.")
	return b.String()
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// kindList is " (dca.execute, dn.open x2)" for a non-empty list of kinds.
func kindList(kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	return " (" + strings.Join(countKinds(kinds), ", ") + ")"
}

func countKinds(kinds []string) []string {
	counts := map[string]int{}
	for _, k := range kinds {
		counts[k]++
	}
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, k := range names {
		if counts[k] > 1 {
			out = append(out, fmt.Sprintf("%s x%d", k, counts[k]))
		} else {
			out = append(out, k)
		}
	}
	return out
}
