package sim

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// Rule answers the calls it matches. Rules are tried in order; the first whose every
// set field matches (and whose Times is not used up) answers.
type Rule struct {
	ID      string  `json:"id"`
	Match   Match   `json:"match"`
	Respond Respond `json:"respond"`
	Times   int     `json:"times,omitempty"` // 0 = unlimited
}

// Match is a conjunction: an unset field matches anything.
type Match struct {
	Turn           string   `json:"turn,omitempty"`   // a proactive.TurnKind value, or "complete"
	Origin         string   `json:"origin,omitempty"` // the turn's tool origin
	Complete       string   `json:"complete,omitempty"`
	PromptContains []string `json:"prompt_contains,omitempty"` // all must occur in the last user message
	SystemContains []string `json:"system_contains,omitempty"`
	ToolsInclude   []string `json:"tools_include,omitempty"`
	LastTool       string   `json:"last_tool,omitempty"` // the tool whose result the call answers
	LastToolResult []string `json:"last_tool_result_contains,omitempty"`
	Sender         string   `json:"sender,omitempty"`
}

// Respond is the answer. Tool/Args is one tool call, ToolCalls several in one response.
// Args are passed on as written, whatever the tool's schema says: a compromised model
// does not follow it.
type Respond struct {
	Text      string                   `json:"text,omitempty"`
	Tool      string                   `json:"tool,omitempty"`
	Args      map[string]any           `json:"args,omitempty"`
	ToolCalls []ToolCallSpec           `json:"tool_calls,omitempty"`
	ThenText  *string                  `json:"then_text,omitempty"` // the answer after our own tool calls; default "OK."
	Error     string                   `json:"error,omitempty"`     // returned as the provider's error
	Usage     *protocoltypes.UsageInfo `json:"usage,omitempty"`
}

// ToolCallSpec is one scripted tool call.
type ToolCallSpec struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

const (
	turnComplete = "complete" // CallInfo.Turn of a providers.Complete call

	callerNews     = "news"
	callerSeahorse = "seahorse"
	callerComplete = "complete"

	toolIDPrefix   = "sim_"
	maxToolRounds  = 8 // consecutive tool-call responses to one turn
	defaultThen    = "OK."
	noMatchText    = "(sim: no rule matched)"
	fallbackText   = "text"
	fallbackError  = "error"
	maxJournalText = 2048
)

var knownTurns = []string{
	turnComplete, string(proactive.TurnUser), string(proactive.TurnProactive),
	string(proactive.TurnHeartbeatLegacy), string(proactive.TurnCronLegacy),
	string(proactive.TurnApprovalFollowUp), string(proactive.TurnReflection), string(proactive.TurnSubTurn),
}

// validateRules checks a script before it is installed.
func validateRules(rules []Rule) error {
	seen := map[string]bool{}
	for i, r := range rules {
		at := fmt.Sprintf("rules[%d]", i)
		switch {
		case strings.TrimSpace(r.ID) == "":
			return fmt.Errorf("%s: id is required", at)
		case seen[r.ID]:
			return fmt.Errorf("%s: duplicate id %q", at, r.ID)
		case r.Times < 0:
			return fmt.Errorf("%s: times must not be negative", at)
		case r.Match.Turn != "" && !slices.Contains(knownTurns, r.Match.Turn):
			return fmt.Errorf("%s: unknown turn %q (known: %s)", at, r.Match.Turn, strings.Join(knownTurns, ", "))
		case r.Match.Complete != "" && !slices.Contains([]string{callerNews, callerSeahorse, callerComplete}, r.Match.Complete):
			return fmt.Errorf("%s: unknown complete caller %q (news, seahorse or complete)", at, r.Match.Complete)
		}
		seen[r.ID] = true
		p := r.Respond
		for _, c := range p.ToolCalls {
			if strings.TrimSpace(c.Name) == "" {
				return fmt.Errorf("%s: a tool call needs a name", at)
			}
		}
		switch {
		case p.Tool != "" && len(p.ToolCalls) > 0:
			return fmt.Errorf("%s: give tool or tool_calls, not both", at)
		case p.Error != "" && (p.Text != "" || p.Tool != "" || len(p.ToolCalls) > 0):
			return fmt.Errorf("%s: error cannot be combined with text or tool calls", at)
		case p.Text == "" && p.Tool == "" && len(p.ToolCalls) == 0 && p.Error == "":
			return fmt.Errorf("%s: respond needs text, tool, tool_calls or error", at)
		}
	}
	return nil
}

// matches reports whether every field m sets holds for the call.
func (m Match) matches(ci CallInfo, msgs []protocoltypes.Message) bool {
	if (m.Turn != "" && m.Turn != ci.Turn) || (m.Origin != "" && m.Origin != ci.Origin) ||
		(m.Sender != "" && m.Sender != ci.Sender) || (m.Complete != "" && m.Complete != ci.Caller) {
		return false
	}
	for _, t := range m.ToolsInclude {
		if !slices.Contains(ci.Tools, t) {
			return false
		}
	}
	if !containsAll(lastContent(msgs, "user"), m.PromptContains) || !containsAll(systemText(msgs), m.SystemContains) {
		return false
	}
	if m.LastTool != "" || len(m.LastToolResult) > 0 {
		name, result, ok := lastToolResult(msgs)
		return ok && (m.LastTool == "" || m.LastTool == name) && containsAll(result, m.LastToolResult)
	}
	return true
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func lastContent(msgs []protocoltypes.Message, role string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == role {
			return msgs[i].Content
		}
	}
	return ""
}

// systemText is every system message, structured parts included.
func systemText(msgs []protocoltypes.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		b.WriteString(m.Content)
		for _, p := range m.SystemParts {
			b.WriteString("\n" + p.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// lastToolResult is the tool and the text of the result that ends the history.
func lastToolResult(msgs []protocoltypes.Message) (name, result string, ok bool) {
	n := len(msgs)
	if n == 0 || msgs[n-1].Role != "tool" {
		return "", "", false
	}
	last := msgs[n-1]
	for i := n - 2; i >= 0; i-- {
		for _, tc := range msgs[i].ToolCalls {
			if tc.ID == last.ToolCallID {
				return toolName(tc), last.Content, true
			}
		}
	}
	return "", last.Content, true
}

func toolName(tc protocoltypes.ToolCall) string {
	if tc.Name == "" && tc.Function != nil {
		return tc.Function.Name
	}
	return tc.Name
}

// followUpOf says whether the history ends with the results of tool calls this engine
// scripted, and the rule that made them.
func followUpOf(msgs []protocoltypes.Message) (ruleID string, ok bool) {
	i := len(msgs) - 1
	for i >= 0 && msgs[i].Role == "tool" {
		i--
	}
	if i == len(msgs)-1 || i < 0 || msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
		return "", false
	}
	for _, tc := range msgs[i].ToolCalls {
		id, isOurs := ruleOfCallID(tc.ID)
		if !isOurs {
			return "", false
		}
		if ruleID == "" {
			ruleID = id
		}
	}
	return ruleID, true
}

// callID is "sim_<rule>_<n>", n being unique in the process.
func callID(rule string, n int64) string { return fmt.Sprintf("%s%s_%d", toolIDPrefix, rule, n) }

func ruleOfCallID(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, toolIDPrefix)
	if !ok {
		return "", false
	}
	i := strings.LastIndex(rest, "_") // rule ids may hold underscores
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

// argsCopy detaches the arguments from the script, so a tool that edits its map cannot
// change the rule.
func argsCopy(a map[string]any) map[string]any {
	out := map[string]any{}
	if b, err := json.Marshal(a); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (r *Rule) calls() []ToolCallSpec {
	if len(r.Respond.ToolCalls) > 0 {
		return r.Respond.ToolCalls
	}
	if r.Respond.Tool != "" {
		return []ToolCallSpec{{Name: r.Respond.Tool, Args: r.Respond.Args}}
	}
	return nil
}

// truncate cuts s to n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
