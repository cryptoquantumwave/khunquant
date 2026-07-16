package tools

import (
	"errors"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// reauthText returns the directing instruction text if err is (or wraps, via
// %w) exchanges.ErrNeedsReauth, or "" otherwise. This is the shared core
// used by both reauthHint (single-result tools) and callers that need a
// plain string for a per-row/per-exchange error slot (e.g. the "all
// exchanges" aggregate in exchange_total_value.go).
func reauthText(err error, exchange, account string) string {
	if err == nil || !errors.Is(err, exchanges.ErrNeedsReauth) {
		return ""
	}
	label := exchange
	if account != "" {
		label += " (" + account + ")"
	}
	return fmt.Sprintf(
		"%s needs re-authentication before this request can complete. Call the %s tool now (exchange=%q, account=%q) — "+
			"it will ask the user to approve the login in the Webull app and report back when connected. "+
			"Do not ask the user for an SMS/OTP code (there is no such API) and do not retry this request until reconnect reports success.",
		label, NameWebullReconnect, exchange, account)
}

// reauthHint returns a directing ToolResult if err is (or wraps, via %w)
// exchanges.ErrNeedsReauth, or nil otherwise. Call it at the top of an
// existing error branch, before falling back to a generic ErrorResult:
//
//	balances, err := we.GetWalletBalances(ctx, walletType)
//	if err != nil {
//	    if hint := reauthHint(err, exchangeName, accountName); hint != nil {
//	        return hint
//	    }
//	    return ErrorResult(fmt.Sprintf("get_assets_list: %v", err))
//	}
//
// This turns an opaque "needs re-authentication" error into an explicit
// instruction the LLM can act on immediately (call webull_reconnect) instead
// of guessing or flailing, which was the original failure mode this whole
// feature exists to fix.
func reauthHint(err error, exchange, account string) *ToolResult {
	msg := reauthText(err, exchange, account)
	if msg == "" {
		return nil
	}
	return ErrorResult(msg)
}

// reauthOrError is the one-line form of the reauthHint pattern for tools
// whose error branch is a single `return ErrorResult(...)`: it returns the
// reauth instruction when err signals interactive re-authentication, and the
// generic ErrorResult(msg) otherwise. Every tool error branch that surfaces
// an error from a provider/exchange METHOD call (market data, orders,
// balances — anything that can hit Webull's token check) must go through
// this or reauthHint; a raw ErrorResult there reproduces the original
// "agent flails on an opaque re-authentication error" failure mode.
func reauthOrError(err error, exchange, account, msg string) *ToolResult {
	if hint := reauthHint(err, exchange, account); hint != nil {
		return hint
	}
	return ErrorResult(msg)
}
