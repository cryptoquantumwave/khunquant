package faultops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func run(t *testing.T, g lab.Group, op, args string) (any, error) {
	t.Helper()
	for _, o := range g.Ops {
		if o.Name == op {
			return o.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no op %q", op)
	return nil, nil
}

func badArgs(t *testing.T, err error) {
	t.Helper()
	var le *lab.Error
	if !errors.As(err, &le) || le.Code != lab.CodeBadArgs {
		t.Fatalf("err = %v, want a bad_args error", err)
	}
}

func newGroup(t *testing.T) (lab.Group, *fault.Injector) {
	t.Helper()
	inj := fault.New(fault.Options{Now: time.Now, MaxHold: time.Second}) //simclock:wall a test clock
	return Group(inj), inj
}

// The group shape the registry needs: three named ops, each with a Run, and a name the
// registry accepts.
func TestGroup_IsRegistrable(t *testing.T) {
	g, _ := newGroup(t)
	l := &lab.Lab{}
	if err := l.AddGroup(g); err != nil || g.Name != "fault" {
		t.Fatalf("AddGroup = %v (name %q)", err, g.Name)
	}
}

func TestAdd_ListClear(t *testing.T) {
	g, inj := newGroup(t)
	out, err := run(t, g, "add", `{"op":"add","match":{"venue":"okx","path_prefix":"/api/v5/market/tickers"},"effect":{"error":"rate_limit"},"when":{"from":"+0m","to":"+2m"}}`)
	if err != nil {
		t.Fatal(err)
	}
	ids := out.(map[string]any)["ids"].([]string)
	if len(ids) != 1 || len(inj.List()) != 1 {
		t.Fatalf("ids = %v, rules = %d", ids, len(inj.List()))
	}
	// Several rules in one call; a bad one adds none, and an earlier table is left alone.
	_, err = run(t, g, "add", `{"op":"add","rules":[{"match":{"venue":"okx"},"effect":{"status":503}},{"match":{"venue":"okx"},"effect":{"error":"no_such"}}]}`)
	badArgs(t, err)
	if len(inj.List()) != 1 {
		t.Fatalf("a failed batch left %d rules, want the 1 from before", len(inj.List()))
	}
	out, err = run(t, g, "add", `{"rules":[{"match":{"venue":"okx"},"effect":{"status":503}},{"match":{"venue":"binance"},"effect":{"drop_connection":true}}]}`)
	if err != nil || len(out.(map[string]any)["ids"].([]string)) != 2 {
		t.Fatalf("batch add = %v, %v", out, err)
	}

	listed, _ := run(t, g, "list", `{}`)
	b, _ := json.Marshal(listed)
	if !strings.Contains(string(b), `"fired":0`) || !strings.Contains(string(b), `"active":true`) || !strings.Contains(string(b), `"error":"rate_limit"`) {
		t.Fatalf("list = %s", b)
	}

	cleared, err := run(t, g, "clear", `{"op":"clear","ids":["`+ids[0]+`"]}`)
	if err != nil || cleared.(map[string]any)["cleared"] != 1 || len(inj.List()) != 2 {
		t.Fatalf("clear by id = %v, %v; %d left", cleared, err, len(inj.List()))
	}
	cleared, _ = run(t, g, "clear", `{}`)
	if cleared.(map[string]any)["cleared"] != 2 || len(inj.List()) != 0 {
		t.Fatalf("clear all = %v; %d left", cleared, len(inj.List()))
	}
}

// Everything a model can put in the call is checked.
func TestAdd_RefusesBadInput(t *testing.T) {
	g, inj := newGroup(t)
	huge := `{"match":{"venue":"okx"},"effect":{"body":"` + strings.Repeat("x", maxArgs) + `"}}`
	many := `{"rules":[` + strings.TrimSuffix(strings.Repeat(`{"match":{"venue":"okx"},"effect":{"status":503}},`, maxPerCall+1), ",") + `]}`
	for name, args := range map[string]string{
		"misspelt match key":    `{"match":{"venue":"okx","pathprefix":"/x"},"effect":{"status":503}}`,
		"misspelt top key":      `{"match":{"venue":"okx"},"effect":{"status":503},"whn":{}}`,
		"unknown library name":  `{"match":{"venue":"okx"},"effect":{"error":"nope"}}`,
		"order kind not on yet": `{"match":{"venue":"binance"},"effect":{"order":"partial","fraction":0.5}}`,
		"malformed when":        `{"match":{"venue":"okx"},"effect":{"status":503},"when":{"from":"soon"}}`,
		"status out of range":   `{"match":{"venue":"okx"},"effect":{"status":1000}}`,
		"latency above cap":     `{"match":{"venue":"okx"},"effect":{"latency":"1h"}}`,
		"latency not a string":  `{"match":{"venue":"okx"},"effect":{"latency":5}}`,
		"one and many":          `{"match":{"venue":"okx"},"effect":{"status":503},"rules":[{"match":{"venue":"okx"},"effect":{"status":503}}]}`,
		"too many in a call":    many,
		"larger than the cap":   huge,
		"not an object":         `[1]`,
	} {
		if _, err := run(t, g, "add", args); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			badArgs(t, err)
		}
	}
	if n := len(inj.List()); n != 0 {
		t.Fatalf("refused input left %d rules", n)
	}
	if _, err := run(t, g, "clear", `{"idz":["f1"]}`); err == nil {
		t.Error("clear accepted a misspelt key and would have emptied the table")
	}
}
