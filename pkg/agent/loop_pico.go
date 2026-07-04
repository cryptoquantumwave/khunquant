package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// publishPicoReasoning sends a "thought" message over the pico channel so the
// web UI can render a collapsible reasoning box.  This is best-effort: context
// cancellation, timeouts, and bus closure are all swallowed silently.
func (al *AgentLoop) publishPicoReasoning(
	ctx context.Context,
	reasoningContent, chatID, _ /*sessionKey*/, modelName string,
) {
	if reasoningContent == "" || chatID == "" {
		return
	}
	if ctx.Err() != nil {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel:    "pico",
		ChatID:     chatID,
		Content:    reasoningContent,
		Kind:       "thought",
		ModelName:  strings.TrimSpace(modelName),
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Pico reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish pico reasoning (best-effort)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		}
	}
}

// publishPicoToolCalls serialises the given tool calls and publishes them as a
// "tool_calls" message over the pico channel.  Best-effort: errors are logged
// and swallowed.
func (al *AgentLoop) publishPicoToolCalls(
	ctx context.Context,
	chatID, _ /*sessionKey*/, modelName string,
	toolCalls []providers.ToolCall,
) {
	if chatID == "" || len(toolCalls) == 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}

	maxArgsLen := al.cfg.Agents.Defaults.GetToolFeedbackMaxArgsLength()
	visibleToolCalls := utils.BuildVisibleToolCalls(toolCalls, maxArgsLen)
	if len(visibleToolCalls) == 0 {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
	defer pubCancel()

	err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel:   "pico",
		ChatID:    chatID,
		Kind:      "tool_calls",
		ModelName: strings.TrimSpace(modelName),
		ToolCalls: visibleToolCalls,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Pico tool calls publish skipped (timeout/cancel)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish pico tool calls (best-effort)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		}
	}
}
