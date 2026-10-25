package tools

import (
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// realExitPrefix leads the result of a risk-reducing exit that ran against the
// REAL exchange while paper_trading_mode is on.
//
// Paper mode refuses everything that opens or enlarges a position, but the
// exits (futures close/reduce/flatten, delta-neutral unwind) deliberately stay
// real: a real position opened before paper mode was switched on - or by a
// bug - must always be closable. Because everything else in paper mode is
// simulated or refused, an exit that quietly hit the exchange would surprise
// the user; the prefix makes it impossible to miss.
const realExitPrefix = "[REAL — closing a real position while paper mode is on] "

// markRealExit prefixes a successful or dry-run exit result with realExitPrefix
// when paper mode is on. Errors are left as they are (nothing was closed), and
// outside paper mode the result is returned untouched.
func markRealExit(cfg *config.Config, r *ToolResult) *ToolResult {
	if r == nil || r.IsError || !broker.IsPaperMode(cfg) {
		return r
	}
	if r.ForLLM != "" {
		r.ForLLM = realExitPrefix + r.ForLLM
	}
	if r.ForUser != "" {
		r.ForUser = realExitPrefix + r.ForUser
	}
	return r
}
