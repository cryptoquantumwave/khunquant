package utils

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// BuildVisibleToolCalls converts a slice of provider ToolCalls into the
// frontend-visible form. maxArgsLen limits argument JSON length (0 = unlimited).
func BuildVisibleToolCalls(
	toolCalls []providers.ToolCall,
	maxArgsLen int,
) []bus.VisibleToolCall {
	if len(toolCalls) == 0 {
		return nil
	}

	visible := make([]bus.VisibleToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		name, _ := VisibleToolCallNameAndArguments(tc)
		argsPreview := VisibleToolCallArgumentsPreview(tc, maxArgsLen)
		explanation := ""
		if tc.ExtraContent != nil {
			explanation = strings.TrimSpace(tc.ExtraContent.ToolFeedbackExplanation)
		}
		if name == "" && explanation == "" && argsPreview == "" {
			continue
		}

		visibleCall := bus.VisibleToolCall{
			ID:   strings.TrimSpace(tc.ID),
			Type: strings.TrimSpace(tc.Type),
		}
		if visibleCall.Type == "" {
			visibleCall.Type = "function"
		}
		if name != "" || argsPreview != "" {
			visibleCall.Function = &bus.VisibleToolCallFunction{
				Name:      name,
				Arguments: argsPreview,
			}
		}
		if explanation != "" {
			visibleCall.ExtraContent = &bus.VisibleToolCallExtraContent{
				ToolFeedbackExplanation: explanation,
			}
		}

		visible = append(visible, visibleCall)
	}

	if len(visible) == 0 {
		return nil
	}
	return visible
}

// VisibleToolCallNameAndArguments extracts the function name and raw JSON
// arguments from a ToolCall, normalising across the different provider shapes.
func VisibleToolCallNameAndArguments(tc providers.ToolCall) (string, string) {
	name := strings.TrimSpace(tc.Name)
	argsJSON := ""
	if tc.Function != nil {
		if name == "" {
			name = strings.TrimSpace(tc.Function.Name)
		}
		argsJSON = strings.TrimSpace(tc.Function.Arguments)
	}
	if argsJSON == "" && len(tc.Arguments) > 0 {
		if encodedArgs, err := json.Marshal(tc.Arguments); err == nil {
			argsJSON = string(encodedArgs)
		}
	}
	return name, strings.TrimSpace(argsJSON)
}

// VisibleToolCallArgumentsPreview returns a pretty-printed (and optionally
// truncated) JSON string of the tool-call arguments.
func VisibleToolCallArgumentsPreview(tc providers.ToolCall, maxLen int) string {
	_, argsJSON := VisibleToolCallNameAndArguments(tc)
	if argsJSON == "" {
		return ""
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(argsJSON), "", "  "); err == nil {
		argsJSON = pretty.String()
	}
	if maxLen > 0 {
		return Truncate(argsJSON, maxLen)
	}
	return argsJSON
}
