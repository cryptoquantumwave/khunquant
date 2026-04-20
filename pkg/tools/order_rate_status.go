package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// GetOrderRateStatusTool exposes the current rate-limiter token counts per provider.
type GetOrderRateStatusTool struct{}

func NewGetOrderRateStatusTool() *GetOrderRateStatusTool {
	return &GetOrderRateStatusTool{}
}

func (t *GetOrderRateStatusTool) Name() string { return NameGetOrderRateStatus }

func (t *GetOrderRateStatusTool) Description() string {
	return "Show the current order rate-limit status for all providers. Displays tokens remaining and the max allowed orders per minute."
}

func (t *GetOrderRateStatusTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
		"required":   []string{},
	}
}

func (t *GetOrderRateStatusTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	status := broker.DefaultLimiter.Status()
	if len(status) == 0 {
		return UserResult("No providers have been rate-limited yet.")
	}

	var sb strings.Builder
	sb.WriteString("Order Rate-Limit Status:\n\n")
	sb.WriteString(fmt.Sprintf("%-20s  %12s  %12s\n", "Provider", "Tokens Left", "Max/Minute"))
	sb.WriteString(fmt.Sprintf("%-20s  %12s  %12s\n", strings.Repeat("-", 20), strings.Repeat("-", 12), strings.Repeat("-", 12)))
	for _, s := range status {
		sb.WriteString(fmt.Sprintf("%-20s  %12d  %12d\n", s.ProviderID, s.TokensLeft, s.MaxTokens))
	}
	return UserResult(sb.String())
}
