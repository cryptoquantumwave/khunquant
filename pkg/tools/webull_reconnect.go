package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// webullReconnectPollInterval and webullReconnectTimeout drive the
// background token/check polling loop. They mirror the documented Webull
// limits (exchanges/webull.TokenCheckPollInterval/MaxWait) but are kept as
// separate overridable package vars so tests can shrink them to
// microseconds without touching the exchanges/webull package.
var (
	webullReconnectPollInterval = 5 * time.Second
	webullReconnectTimeout      = 5 * time.Minute
)

// reauthStatus* alias the provider-neutral status constants from
// pkg/exchanges so this package's switch statements stay short.
const (
	reauthStatusNormal  = exchanges.ReauthStatusNormal
	reauthStatusPending = exchanges.ReauthStatusPending
	reauthStatusInvalid = exchanges.ReauthStatusInvalid
	reauthStatusExpired = exchanges.ReauthStatusExpired
)

// WebullReconnectTool re-establishes an approved Webull session when a call
// reports it needs re-authentication (see exchanges.ErrNeedsReauth). Webull
// requires the user to approve the login inside the Webull mobile app; there
// is no API to submit an SMS/OTP code, so this tool starts the login and
// then polls in the background for the user's in-app approval, notifying
// the outcome asynchronously instead of blocking the turn.
type WebullReconnectTool struct {
	cfg *config.Config
}

// Compile-time check: WebullReconnectTool implements AsyncExecutor.
var _ AsyncExecutor = (*WebullReconnectTool)(nil)

func NewWebullReconnectTool(cfg *config.Config) *WebullReconnectTool {
	return &WebullReconnectTool{cfg: cfg}
}

func (t *WebullReconnectTool) Name() string {
	return NameWebullReconnect
}

func (t *WebullReconnectTool) Description() string {
	return "Re-establish an approved Webull session when a Webull call reports it needs re-authentication. " +
		"Triggers a login request and asks the user to approve it in the Webull mobile app, then waits in " +
		"the background and reports back once the account is reconnected (or if approval times out). " +
		"Does NOT accept or ask for an SMS/OTP code — Webull has no API for that; approval only happens " +
		"by the user tapping \"approve\" in the Webull app."
}

func (t *WebullReconnectTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"exchange": map[string]any{
				"type":        "string",
				"description": "Exchange to reconnect (default: \"webull\")",
				"enum":        []string{"webull"},
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name to reconnect (e.g. \"main\"). Omit for default account.",
			},
		},
		"required": []string{},
	}
}

func (t *WebullReconnectTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.execute(ctx, args, nil)
}

// ExecuteAsync implements AsyncExecutor. The callback is invoked once the
// background approval poll resolves (approved, timed out, or invalid).
func (t *WebullReconnectTool) ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
	return t.execute(ctx, args, cb)
}

func (t *WebullReconnectTool) execute(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
	exchangeName := "webull"
	if v, ok := args["exchange"].(string); ok && v != "" {
		exchangeName = v
	}
	accountName := ""
	if v, ok := args["account"].(string); ok {
		accountName = strings.TrimSpace(v)
	}
	label := exchangeName
	if accountName != "" {
		label = fmt.Sprintf("%s (%s)", exchangeName, accountName)
	}

	ex, err := exchanges.CreateExchangeForAccount(exchangeName, accountName, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("webull_reconnect: %v", err))
	}
	re, ok := ex.(exchanges.ReauthExchange)
	if !ok {
		return ErrorResult(fmt.Sprintf("webull_reconnect: exchange %q does not support reconnect", exchangeName))
	}

	status, err := re.Reconnect(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("webull_reconnect: %v", err))
	}

	switch status {
	case reauthStatusNormal:
		return NewToolResult(fmt.Sprintf(
			"%s session is already active (status NORMAL) — no approval needed. Retry the user's original request now.", label))

	case reauthStatusInvalid, reauthStatusExpired:
		return ErrorResult(fmt.Sprintf(
			"%s login could not be started (status %s). This looks like an app-credential problem, not something "+
				"that in-app approval can fix — the user may need to re-provision API access in the Webull app settings.",
			label, status))

	case reauthStatusPending:
		if cb == nil {
			// Synchronous fallback (e.g. tests, or a caller with no async
			// support): run the poll loop inline and return the terminal result.
			return pollWebullReauth(context.Background(), re, label)
		}
		go func() {
			// Deliberately derived from context.Background(), not ctx: ctx
			// is canceled the moment this synchronous call returns
			// AsyncResult below, exactly like the agent loop's own async
			// callback plumbing (pkg/agent/loop.go) does for the same reason.
			pollCtx, cancel := context.WithTimeout(context.Background(), webullReconnectTimeout)
			defer cancel()
			result := pollWebullReauth(pollCtx, re, label)
			cb(pollCtx, result)
		}()
		return &ToolResult{
			ForUser: fmt.Sprintf(
				"Open the Webull app and approve the login request for %s. I'll keep checking in the background and let you know as soon as it's connected (this can take a few minutes).",
				label),
			ForLLM: fmt.Sprintf(
				"webull_reconnect started for %s; a background poll is waiting for the user to approve the login in the Webull app. "+
					"Do not retry the original request yet — a follow-up system message will report the outcome.",
				label),
			Async: true,
		}

	default:
		return ErrorResult(fmt.Sprintf("webull_reconnect: %s returned unrecognized token status %q", label, status))
	}
}

// pollWebullReauth waits for the pending login to resolve via the shared
// exchanges.PollReauth loop, then maps the terminal status to the tool's
// user/LLM messages.
func pollWebullReauth(ctx context.Context, re exchanges.ReauthExchange, label string) *ToolResult {
	status, err := exchanges.PollReauth(ctx, re, webullReconnectPollInterval)
	if err != nil {
		// ctx expired: the user never approved within the window.
		return &ToolResult{
			ForUser: fmt.Sprintf(
				"Still no approval after waiting — the Webull login for %s wasn't approved in time. Ask me to reconnect again when you're ready to approve it in the app.",
				label),
			ForLLM: fmt.Sprintf(
				"webull_reconnect for %s timed out awaiting in-app approval; do not retry the original request. Suggest the user call webull_reconnect again when ready.",
				label),
			IsError: true,
		}
	}
	if status == reauthStatusNormal {
		return &ToolResult{
			ForUser: fmt.Sprintf("%s is reconnected and ready.", label),
			ForLLM: fmt.Sprintf(
				"Webull session for %s is now NORMAL. Retry the user's original request that failed with the re-authentication error.",
				label),
		}
	}
	// INVALID / EXPIRED.
	return &ToolResult{
		ForUser: fmt.Sprintf(
			"The Webull login for %s couldn't be completed (status %s). The API access may need to be re-approved in the Webull app settings.",
			label, status),
		ForLLM: fmt.Sprintf(
			"webull_reconnect polling for %s returned %s; treat as a credential/app-access problem, not a transient error. Do not retry the original request.",
			label, status),
		IsError: true,
	}
}
