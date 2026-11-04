// Package faultops is the lab group "fault": add, list and clear fault rules. It is a package
// of its own because lab holds a *fault.Injector, so fault must not import lab.
package faultops

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

const (
	maxArgs     = 256 << 10 // a call is a handful of rules; anything bigger is refused unread
	maxPerCall  = 64
	ruleSchema  = `{"type":"object","description":"{id?, match:{venue (binance|binanceth|okx|bitkub|settrade|webull|web|* or llm), method?, path_prefix?, order?, market?, symbol?, side?, reduce_only?, turn?, caller?}, effect:{exactly one of: status+body, error, timeout, latency, drop_connection, order, llm_latency, llm_error}, when?:{from?, to?, times?, probability?, seed?}}"}`
	description = "Break the simulated exchanges and the simulated model the way production breaks them. A rule is match + one effect + when. " +
		"The first active matching rule fires, once per request. status/body/error answer in the venue's own error shape (error: a library name " +
		"such as rate_limit, backend_timeout, invalid_order, order_not_found, bad_symbol, auth, insufficient_funds, server_error, bad_request); " +
		"latency and timeout hold the request (wall time, capped); drop_connection closes it; order: written_then_timeout creates the order and loses the " +
		"answer, order: reject with code answers an order operation with a definite refusal; llm_latency and llm_error break model calls (venue llm). " +
		"when.from/to are \"+10m\" from now (sim time) or RFC 3339. Every firing is journaled as fault.fired."
)

// Group lets an agent add, list and clear fault rules.
func Group(inj *fault.Injector) lab.Group {
	return lab.Group{
		Name:        "fault",
		Description: description,
		Ops: []lab.Op{
			{
				Name:        "add",
				Description: "add one rule (the rule's own fields) or several (rules: [...]); all are checked before any is added. Answers the ids.",
				Args: json.RawMessage(`{"id":{"type":"string"},"match":{"type":"object"},"effect":{"type":"object"},"when":{"type":"object"},` +
					`"rules":{"type":"array","items":` + ruleSchema + `,"description":"several rules instead of one"}}`),
				Run: addOp(inj),
			},
			{Name: "list", Description: "every rule with how often it fired and whether it can fire now", Run: listOp(inj)},
			{
				Name:        "clear",
				Description: "remove the rules with these ids, or every rule when none is given",
				Args:        json.RawMessage(`{"ids":{"type":"array","items":{"type":"string"}}}`),
				Run:         clearOp(inj),
			},
		},
	}
}

// decode reads the call's object strictly: a misspelt match key must not turn into a rule
// that matches everything.
func decode(args json.RawMessage, into any) error {
	if len(args) > maxArgs {
		return lab.Errorf(lab.CodeBadArgs, "the call is larger than %d bytes", maxArgs)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	return nil
}

func addOp(inj *fault.Injector) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Op    string       `json:"op"`
			Rules []fault.Rule `json:"rules"`
			fault.Rule
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		rules := a.Rules
		if len(rules) == 0 {
			rules = []fault.Rule{a.Rule}
		} else if a.Match != (fault.Match{}) || a.ID != "" {
			return nil, lab.Errorf(lab.CodeBadArgs, "give one rule or rules[], not both")
		}
		if len(rules) > maxPerCall {
			return nil, lab.Errorf(lab.CodeBadArgs, "at most %d rules per call", maxPerCall)
		}
		ids := make([]string, 0, len(rules))
		for n, r := range rules {
			id, err := inj.Add(r)
			if err != nil {
				if len(ids) > 0 { // all or none (Clear with no id would empty the table)
					inj.Clear(ids...)
				}
				return nil, lab.Errorf(lab.CodeBadArgs, "rule %d: %v", n, err)
			}
			ids = append(ids, id)
		}
		return map[string]any{"ids": ids}, nil
	}
}

func listOp(inj *fault.Injector) lab.OpFunc {
	return func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"rules": inj.List()}, nil
	}
}

func clearOp(inj *fault.Injector) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Op  string   `json:"op"`
			IDs []string `json:"ids"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return map[string]any{"cleared": inj.Clear(a.IDs...)}, nil
	}
}
