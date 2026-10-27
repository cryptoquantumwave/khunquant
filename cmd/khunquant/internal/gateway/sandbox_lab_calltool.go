package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// labSetup is what the lab's setup shortcuts (call_tool, manage) use of the current
// generation. registerSandboxLab sets it before the generation is published
// (publishLabGeneration), and the groups read it through shared.labGen.
type labSetup struct {
	al    *agent.AgentLoop
	mux   http.Handler // manage's private mux: the real /api/proactive/* handlers, this generation's services
	token string       // the management token that mux's guards expect; never leaves the process
}

// labSetupOf is the current generation's setup, or nil between generations.
func labSetupOf(shared *sharedResources) *labSetup {
	if s := shared.labGen.Load(); s != nil {
		return s.labSetup
	}
	return nil
}

// simChatChannel is the channel and sender platform of a simulated person. It is the
// name simchat registers under; the gateway names it here rather than reach into the
// channel, so call_tool's `as` follows simchat's grammar by agreement (dm:<p>,
// group:<g>, sender simchat:<p>), and a test pins that.
const simChatChannel = "simchat"

var personaRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type callToolArgs struct {
	Op   string         `json:"op"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	As   string         `json:"as"`
	Chat string         `json:"chat"`
}

// callToolGroup runs a registered tool as a given person, through the same registry
// funnel a model's call takes: argument validation, the tool policy and approvals, the
// hooks (and with them the lab's tool-call tap).
func callToolGroup(shared *sharedResources) lab.Group {
	return lab.Group{
		Name: "call_tool",
		Description: "Run any registered tool as a person, skipping the model. The call goes through the real registry: " +
			"schema validation, tool policy, approvals and hooks all apply. Returns the tool's result.",
		Ops: []lab.Op{{
			Description: "call a tool",
			Args: json.RawMessage(`{"name":{"type":"string","description":"tool name, e.g. set_price_alert"},` +
				`"args":{"type":"object","description":"the tool's arguments"},` +
				`"as":{"type":"string","description":"owner (default: the configured owner route) | cli (the local console) | <persona> (simchat:<persona> in dm:<persona>) | <persona>@group:<g>"},` +
				`"chat":{"type":"string","description":"override the chat id"}}`),
			Required: []string{"name"},
			Run:      func(ctx context.Context, raw json.RawMessage) (any, error) { return callTool(ctx, shared, raw) },
		}},
	}
}

func callTool(ctx context.Context, shared *sharedResources, raw json.RawMessage) (any, error) {
	var a callToolArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	if a.Name == "" {
		return nil, lab.Errorf(lab.CodeBadArgs, "name is required")
	}
	setup := labSetupOf(shared)
	if setup == nil || setup.al == nil {
		return nil, lab.Errorf(lab.CodeUnavailable, "no generation of services is running (a reload is in progress?)")
	}
	al := setup.al
	channel, chatID, sender, err := resolveAs(al, a.As, a.Chat)
	if err != nil {
		return nil, err
	}
	var inst *agent.AgentInstance
	if reg := al.GetRegistry(); reg != nil {
		inst = reg.GetDefaultAgent()
	}
	if inst == nil || inst.Tools == nil {
		return nil, lab.Errorf(lab.CodeUnavailable, "the default agent has no tool registry")
	}

	// The tags runAgentLoop gives a root turn (pkg/agent/loop.go). Never WithWebAuth: a
	// web-authenticated turn is a fact about a pico connection, which this is not.
	turnID := tools.NewTurnID()
	ctx = tools.WithTurnID(ctx, turnID)
	ctx = agent.WithAgentLoop(ctx, al)
	ctx = tools.WithToolOrigin(ctx, tools.OriginForChannel(channel))
	ctx = tools.WithToolSender(ctx, sender)
	defer func() { // the turn is ours, so the message tool's note of it is too
		if t, ok := inst.Tools.Get(tools.NameMessage); ok {
			if mt, ok := t.(*tools.MessageTool); ok {
				mt.ClearTurn(turnID)
			}
		}
	}()

	res := inst.Tools.ExecuteWithContext(ctx, a.Name, a.Args, channel, chatID, nil)
	if res == nil {
		return nil, lab.Errorf(lab.CodeFailed, "tool %q returned no result", a.Name)
	}
	return map[string]any{"for_llm": res.ForLLM, "for_user": res.ForUser, "is_error": res.IsError, "silent": res.Silent}, nil
}

// resolveAs maps `as` (and `chat`) to the channel, chat id and sender the turn carries.
func resolveAs(al *agent.AgentLoop, as, chat string) (channel, chatID, sender string, err error) {
	switch as {
	case "", "owner":
		cfg := al.GetConfig()
		if cfg == nil {
			return "", "", "", lab.Errorf(lab.CodeUnavailable, "no configuration is loaded")
		}
		route, ok := cfg.Proactive.OwnerRoute()
		if !ok {
			return "", "", "", lab.Errorf(lab.CodeBadArgs, "as owner: no owner is configured (proactive.owner)")
		}
		channel, chatID, sender = route.Channel, route.ChatID, route.SenderID
	case "cli":
		channel = "cli"
	default:
		name, at, _ := strings.Cut(as, "@")
		if !personaRE.MatchString(name) || (at != "" && !strings.HasPrefix(at, "group:")) || at == "group:" {
			return "", "", "", lab.Errorf(lab.CodeBadArgs, "as %q: want owner, cli, <persona> or <persona>@group:<name>", as)
		}
		channel, sender = simChatChannel, simChatChannel+":"+name
		chatID = "dm:" + name
		if at != "" {
			chatID = at
		}
	}
	if chat != "" {
		chatID = chat
	}
	return channel, chatID, sender, nil
}
