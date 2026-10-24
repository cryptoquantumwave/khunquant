package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// reflectionTurnTools is the whole ceiling of the nightly reflection turn: two reads of
// the assistant's own state and the one tool that hands its suggestions to code. It is a
// list of names, not a class test, on purpose: the read class also holds the web, files,
// skills and past conversations, which carry text from outside the process, and a
// reflection turn reads none of them. There is no message, write, memory, order, alert,
// plan, MCP or system tool in it, and a tool added later is outside until it is added here.
var reflectionTurnTools = map[string]bool{
	NameGetProfile:        true,
	NameListIntents:       true,
	NameProposeReflection: true,
}

// ReflectionTurnTools returns the ceiling's tool names, for tests and for the turn.
func ReflectionTurnTools() []string {
	out := make([]string, 0, len(reflectionTurnTools))
	for n := range reflectionTurnTools {
		out = append(out, n)
	}
	return out
}

func reflectionTurnMayCall(name string) bool { return reflectionTurnTools[name] }

// ProposeReflectionTool is how the reflection turn declares what it suggests. It changes
// nothing and queues nothing: it records the raw items in the turn's collector, and
// pkg/reflection validates each one in code and turns the valid ones into proposals the
// owner decides. It is built per turn, registered on that turn's registry only, and
// refuses to run in any other turn.
type ProposeReflectionTool struct{}

// NewProposeReflectionTool builds the tool for one reflection turn.
func NewProposeReflectionTool() *ProposeReflectionTool { return &ProposeReflectionTool{} }

func (t *ProposeReflectionTool) Name() string { return NameProposeReflection }

func (t *ProposeReflectionTool) Description() string {
	return "Declare, once, the adjustments you suggest from today's review (an empty list if none). " +
		"Each is checked by the system against the data and its bounds, and the owner decides; nothing changes until they do."
}

func (t *ProposeReflectionTool) Parameters() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":     "array",
				"maxItems": proactive.MaxRawReflectionItems,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{
							"type": "string", "enum": []string{"threshold_change", "intent_cleanup", "memory_fact", "mute_trigger"},
						},
						"target":       str("threshold_change: the trigger id; intent_cleanup: the intent id; mute_trigger: the trigger kind; memory_fact: omit."),
						"change":       map[string]any{"type": "object", "description": "threshold_change: {\"<param>\": <number>}; intent_cleanup: {\"action\":\"pause|cancel\"}; mute_trigger: {\"days\": 1-30}; memory_fact: {\"fact\": \"...\", \"category\": \"preference|fact|plan_note\"}."},
						"rationale":    str("At most 300 characters. Shown to the owner under \"Why\"; never acted on."),
						"evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Decision ids from today's data that support it."},
					},
					"required": []string{"type"},
				},
			},
		},
		"required": []string{"items"},
	}
}

func (t *ProposeReflectionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	if ToolOrigin(ctx) != OriginProactive || ProactiveTurnKind(ctx) != proactive.TurnReflection {
		return ErrorResult(NameProposeReflection + " is only available in the nightly reflection turn")
	}
	c := proactive.ReflectionCollectorFrom(ctx)
	if c == nil {
		return ErrorResult(NameProposeReflection + " has nowhere to record: this turn collects none")
	}
	raw, ok := args["items"].([]any)
	if !ok {
		return ErrorResult("items must be a list (an empty one when you suggest nothing)")
	}
	items := make([]proactive.ReflectionItem, 0, len(raw))
	for i, r := range raw {
		b, err := json.Marshal(r)
		if err != nil {
			return ErrorResult(fmt.Sprintf("item %d is not valid JSON", i+1))
		}
		var it proactive.ReflectionItem
		if err := json.Unmarshal(b, &it); err != nil {
			return ErrorResult(fmt.Sprintf("item %d is malformed: type, target and rationale are strings, change an object, evidence_ids a list of strings", i+1))
		}
		items = append(items, it)
	}
	if !c.Record(items) {
		return ErrorResult(NameProposeReflection + " was already called in this turn; it can be called once")
	}
	return SilentResult(fmt.Sprintf("Recorded %d suggestion(s). They are checked by the system and the owner decides. Stop here.", len(items)))
}
