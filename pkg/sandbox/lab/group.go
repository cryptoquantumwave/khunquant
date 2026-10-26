package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// CallTimeout caps every Call: the HTTP server writes with a 30 s timeout. Long waits
// are loops of calls.
const CallTimeout = labjournal.MaxWait

// OpFunc runs one operation. args is the whole request object, "op" included.
type OpFunc func(ctx context.Context, args json.RawMessage) (any, error)

// Op is one operation of a group.
type Op struct {
	Name        string // "" only for a single-op group: op may then be omitted
	Description string
	Args        json.RawMessage // JSON Schema "properties" object for this op
	Required    []string
	Run         OpFunc
}

// Group is a named set of operations.
type Group struct {
	Name        string // ^[a-z][a-z_]*$; route /api/sandbox/lab/<Name>, tool sandbox_<Name>
	Description string
	Ops         []Op
}

const (
	CodeUnknownGroup = "unknown_group"
	CodeUnknownOp    = "unknown_op"
	CodeBadArgs      = "bad_args"
	CodeUnavailable  = "unavailable"
	CodeFailed       = "failed"
)

// Error is a failure with a stable code; any other error an op returns is CodeFailed.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// Errorf builds an *Error.
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

var nameRE = regexp.MustCompile(`^[a-z][a-z_]*$`)

// reservedTools are the dev-MCP server's own tool names (pkg/devmcp cannot be imported
// here; its test pins that every one of them is refused as a group name).
var reservedTools = map[string]bool{
	"service_status": true, "list_tools": true, "list_llm_calls": true, "read_llm_call": true,
	"list_sessions": true, "read_session_history": true, "search_sessions": true,
	"read_logs": true, "read_config": true,
	"sandbox_list_venues": true, "sandbox_read_fixtures": true, "sandbox_write_fixtures": true,
	"sandbox_upsert_fixture": true, "sandbox_reload_fixtures": true, "sandbox_reset_simulator": true,
}

// AddGroup registers g in the table both front-ends read (POST /api/sandbox/lab/<group>
// and the MCP tool sandbox_<group>). It fails on a bad or reserved name, a duplicate
// group or op, or an op with no Run.
func (l *Lab) AddGroup(g Group) error {
	if !nameRE.MatchString(g.Name) || reservedTools[g.Name] || reservedTools["sandbox_"+g.Name] {
		return fmt.Errorf("lab: group name %q is invalid or collides with a dev-MCP tool", g.Name)
	}
	seen := map[string]bool{}
	for _, o := range g.Ops {
		if o.Run == nil || seen[o.Name] || (o.Name == "" && len(g.Ops) > 1) || (o.Name != "" && !nameRE.MatchString(o.Name)) {
			return fmt.Errorf("lab: group %q op %q is invalid (no Run, duplicate, or bad name)", g.Name, o.Name)
		}
		seen[o.Name] = true
	}
	if len(g.Ops) == 0 {
		return fmt.Errorf("lab: group %q has no ops", g.Name)
	}
	l.gmu.Lock()
	defer l.gmu.Unlock()
	if _, dup := l.groups[g.Name]; dup {
		return fmt.Errorf("lab: group %q is already registered", g.Name)
	}
	if l.groups == nil {
		l.groups = map[string]Group{}
	}
	l.groups[g.Name] = g
	return nil
}

// Groups returns the registered groups sorted by name.
func (l *Lab) Groups() []Group {
	l.gmu.RLock()
	defer l.gmu.RUnlock()
	out := make([]Group, 0, len(l.groups))
	for _, g := range l.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OpOf reads the "op" field of a request object ("" when absent or unreadable).
func OpOf(args json.RawMessage) string {
	var o struct {
		Op string `json:"op"`
	}
	_ = json.Unmarshal(args, &o)
	return o.Op
}

// Call runs op of group with args under CallTimeout. The error is always an *Error:
// a panic in the op and any plain error it returns both become CodeFailed.
func (l *Lab) Call(ctx context.Context, group, op string, args json.RawMessage) (res any, err error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	l.gmu.RLock()
	g, ok := l.groups[group]
	l.gmu.RUnlock()
	if !ok {
		return nil, Errorf(CodeUnknownGroup, "no lab group %q", group)
	}
	var run OpFunc
	for _, o := range g.Ops {
		if o.Name == op {
			run = o.Run
		}
	}
	if run == nil {
		return nil, Errorf(CodeUnknownOp, "group %q has no op %q", group, op)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, Errorf(CodeFailed, "group %q op %q panicked: %v", group, op, r)
		}
	}()
	res, err = run(ctx, args)
	var le *Error
	if err != nil && !errors.As(err, &le) {
		err = &Error{Code: CodeFailed, Msg: err.Error()}
	}
	return res, err
}
