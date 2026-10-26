package lab

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func okOp(res any) OpFunc {
	return func(context.Context, json.RawMessage) (any, error) { return res, nil }
}

func TestAddGroupRejectsDuplicateAndReserved(t *testing.T) {
	l := &Lab{}
	g := Group{Name: "inspect", Ops: []Op{{Name: "journal", Run: okOp(1)}}}
	if err := l.AddGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := l.AddGroup(g); err == nil {
		t.Error("a duplicate group was accepted")
	}
	dupOp := Group{Name: "dupop", Ops: []Op{{Name: "a", Run: okOp(1)}, {Name: "a", Run: okOp(1)}}}
	if err := l.AddGroup(dupOp); err == nil {
		t.Error("a duplicate op was accepted")
	}
	for _, name := range []string{"list_venues", "sandbox_list_venues", "read_logs", "service_status", "Bad", "", "1x", "a-b"} {
		if err := l.AddGroup(Group{Name: name, Ops: []Op{{Name: "x", Run: okOp(1)}}}); err == nil {
			t.Errorf("group name %q was accepted", name)
		}
	}
	for _, g := range []Group{
		{Name: "noops"},
		{Name: "norun", Ops: []Op{{Name: "x"}}},
		{Name: "nameless", Ops: []Op{{Run: okOp(1)}, {Name: "b", Run: okOp(1)}}},
		{Name: "badop", Ops: []Op{{Name: "Bad", Run: okOp(1)}}},
	} {
		if err := l.AddGroup(g); err == nil {
			t.Errorf("group %+v was accepted", g.Name)
		}
	}
	if got := l.Groups(); len(got) != 1 || got[0].Name != "inspect" {
		t.Errorf("Groups() = %+v, want only inspect", got)
	}
}

func TestGroupsSortedByName(t *testing.T) {
	l := &Lab{}
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := l.AddGroup(Group{Name: n, Ops: []Op{{Run: okOp(n)}}}); err != nil {
			t.Fatal(err)
		}
	}
	got := l.Groups()
	if len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "mid" || got[2].Name != "zeta" {
		t.Fatalf("Groups() = %+v", got)
	}
}

func TestCallSingleOpGroupWithoutOp(t *testing.T) {
	l := &Lab{}
	if err := l.AddGroup(Group{Name: "call_tool", Ops: []Op{{Run: okOp("done")}}}); err != nil {
		t.Fatal(err)
	}
	res, err := l.Call(context.Background(), "call_tool", "", nil)
	if err != nil || res != "done" {
		t.Fatalf("Call = %v, %v", res, err)
	}
	if OpOf(json.RawMessage(`{"op":"x","a":1}`)) != "x" || OpOf(json.RawMessage(`nonsense`)) != "" {
		t.Error("OpOf misreads the request object")
	}
}

func TestCallErrorsCarryCodes(t *testing.T) {
	l := &Lab{}
	boom := errors.New("boom")
	err := l.AddGroup(Group{Name: "g", Ops: []Op{
		{Name: "plain", Run: func(context.Context, json.RawMessage) (any, error) { return nil, boom }},
		{Name: "coded", Run: func(context.Context, json.RawMessage) (any, error) {
			return nil, Errorf(CodeBadArgs, "need x")
		}},
		{Name: "panics", Run: func(context.Context, json.RawMessage) (any, error) { panic("kaboom") }},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ group, op, code string }{
		{"nope", "plain", CodeUnknownGroup},
		{"g", "nope", CodeUnknownOp},
		{"g", "plain", CodeFailed},
		{"g", "coded", CodeBadArgs},
		{"g", "panics", CodeFailed},
	} {
		_, err := l.Call(context.Background(), tc.group, tc.op, nil)
		var le *Error
		if !errors.As(err, &le) || le.Code != tc.code {
			t.Errorf("Call(%s, %s) error = %v, want code %s", tc.group, tc.op, err, tc.code)
		}
	}
}
