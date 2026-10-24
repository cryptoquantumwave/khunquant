package agent

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// reflectionContract is the standing instruction of the nightly reflection turn.
//
//go:embed prompts/reflection.md
var reflectionContract string

// ReflectionDeadline bounds a reflection turn, queue wait included.
const ReflectionDeadline = 3 * time.Minute

// ReflectionTurnResult is what a reflection turn declared. Nothing in it has been
// validated or applied: pkg/reflection does that, in code.
type ReflectionTurnResult struct {
	Items []proactive.ReflectionItem
	// Called is whether the model called propose_reflection (it may declare an empty list).
	Called       bool
	InputTokens  int
	OutputTokens int
}

// RunReflectionTurn asks the default agent's model for the day's suggestions, given the
// data pkg/reflection built. It is a budgeted cognition job at reflection priority with a
// three-minute deadline. It needs the scheduler: with none, or with the budget spent, it
// returns an error wrapping proactive.ErrTurnSkipped and makes no model call; it never
// runs outside the budget.
//
// The turn is read-only and delivers nothing. Its origin is tools.OriginProactive with the
// reflection kind, whose ceiling is the three tools of tools.ReflectionTurnTools.
func (al *AgentLoop) RunReflectionTurn(ctx context.Context, input string) (ReflectionTurnResult, error) {
	var res ReflectionTurnResult
	if al.scheduler() == nil || inScheduledJob(ctx) {
		return res, fmt.Errorf("%w (the reflection runs only as a cognition job)", proactive.ErrTurnSkipped)
	}
	spec := turnSpec{Kind: proactive.TurnReflection, Priority: cognition.PriReflection, Budgeted: true, Deadline: ReflectionDeadline}
	if agent := al.GetRegistry().GetDefaultAgent(); agent != nil {
		spec.Session = sessionKeyAgentPrefix + agent.ID + ":reflection"
	}
	waitCtx, cancel := context.WithTimeout(ctx, ReflectionDeadline)
	defer cancel()
	var box turnBox[ReflectionTurnResult]
	err, scheduled := al.submitTurn(waitCtx, spec, func(jctx context.Context) error {
		r, runErr := al.runReflectionTurn(jctx, input)
		box.put(r, runErr)
		return runErr
	})
	if !scheduled {
		return res, fmt.Errorf("%w (no scheduler took the job)", proactive.ErrTurnSkipped)
	}
	if err != nil {
		err = fmt.Errorf("reflection turn: %w", err)
	}
	return box.outcome(err)
}

func (al *AgentLoop) runReflectionTurn(ctx context.Context, input string) (ReflectionTurnResult, error) {
	var res ReflectionTurnResult
	if turnStateFromContext(ctx) != nil {
		return res, errors.New("reflection turn: cannot be started from inside another turn")
	}
	registry := al.GetRegistry()
	if registry == nil {
		return res, errors.New("reflection turn: no agent registry")
	}
	agent := registry.GetDefaultAgent()
	if agent == nil || agent.Sessions == nil {
		return res, errors.New("reflection turn: no default agent")
	}
	cfg := al.GetConfig()
	if cfg == nil {
		return res, errors.New("reflection turn: no config")
	}
	eff := cfg.Proactive.Effective()
	ctx, cancel := context.WithTimeout(ctx, ReflectionDeadline)
	defer cancel()

	// Where the turn runs: context only, as for a proactive turn. NoOutbound keeps anything
	// from using it, and the turn reads no history (its session is never the owner's).
	channel, chatID := proactiveFallbackChannel, proactiveFallbackChatID
	if eff.CanDeliver() && eff.Owner.Channel != "" {
		channel, chatID = eff.Owner.Channel, eff.Owner.ChatID
	}
	collector := proactive.NewReflectionCollector()
	ctx = proactive.WithReflectionCollector(ctx, collector)
	ctx = tools.WithProactiveTurn(ctx, proactive.TurnReflection)
	ctx = tools.WithToolSender(ctx, "")
	ctx = proactive.WithTaint(ctx, proactive.NewTaint(proactive.TrustUntrusted)) // it reads the owner's words and intent titles

	report := &turnReport{}
	sessionKey := sessionKeyAgentPrefix + agent.ID + ":reflection"
	_, err := al.runAgentLoop(ctx, agent, processOptions{
		SessionKey:           sessionKey,
		Channel:              channel,
		ChatID:               chatID,
		UserMessage:          strings.TrimSpace(reflectionContract) + "\n\n" + input,
		PromptMode:           PromptProactive,
		OfferLoadSkill:       false,
		DefaultResponse:      "",
		EnableSummary:        false,
		SendResponse:         false,
		SuppressToolFeedback: true,
		NoHistory:            true,
		ExtraTools:           []tools.Tool{tools.NewProposeReflectionTool()},
		ModelOverride:        modelPrimary,
		NoOutbound:           true,
		Report:               report,
		NoBackgroundSections: true,
	})
	res.InputTokens, res.OutputTokens, _ = report.snapshot()
	if ctx.Err() == nil {
		al.trimProactiveSession(ctx, agent, sessionKey)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("reflection turn exceeded its %s deadline: %w", ReflectionDeadline, context.DeadlineExceeded)
		}
		return res, fmt.Errorf("reflection turn: %w", err)
	}
	res.Items, res.Called, _ = collector.Result()
	logger.InfoCF("agent", "Reflection turn finished", map[string]any{
		"called": res.Called, "items": len(res.Items), "input_tokens": res.InputTokens, "output_tokens": res.OutputTokens,
	})
	return res, nil
}

// SessionLine is one message of a session's text: its role and what was said.
type SessionLine struct{ Role, Text string }

// SessionTail returns the last n user and assistant messages of the default agent's
// session sessionKey, oldest first, and a fingerprint that changes whenever the user
// says something new (their count and the last one's hash). Tool calls and tool results
// are left out: they can hold pages fetched from the web, and the reflection reads
// nothing from outside the process. A session that does not exist gives nothing.
func (al *AgentLoop) SessionTail(sessionKey string, n int) (lines []SessionLine, fingerprint string) {
	registry := al.GetRegistry()
	if registry == nil {
		return nil, ""
	}
	agent := registry.GetDefaultAgent()
	if agent == nil || agent.Sessions == nil {
		return nil, ""
	}
	users, last := 0, ""
	for _, m := range agent.Sessions.GetHistory(sessionKey) {
		if (m.Role != "user" && m.Role != "assistant") || len(m.ToolCalls) > 0 || m.ToolCallID != "" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		if m.Role == "user" {
			users++
			last = m.Content
		}
		lines = append(lines, SessionLine{Role: m.Role, Text: m.Content})
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if users > 0 {
		sum := sha256.Sum256([]byte(last))
		fingerprint = fmt.Sprintf("%d:%x", users, sum[:6])
	}
	return lines, fingerprint
}
