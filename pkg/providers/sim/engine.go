package sim

import (
	"context"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// Engine answers the model calls of one sandbox instance.
type Engine struct{ mode string }

// Chat is replaced by the rule engine in the next commit.
func (e *Engine) Chat(context.Context, []protocoltypes.Message, []protocoltypes.ToolDefinition,
	string, map[string]any,
) (*protocoltypes.LLMResponse, error) {
	return nil, errors.New("sim: scripted rules are not implemented yet")
}
