package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

const (
	defaultWaitFor = 60 * time.Second
	maxWaitFor     = 10 * time.Minute
	closeTimeout   = 30 * time.Second
)

// mcpServer is the supervisor's MCP server: the tools that manage instances plus
// proxies of everything an instance offers.
type mcpServer struct {
	sup *supervisor.Supervisor
	srv *mcp.Server

	mu      sync.Mutex
	reports map[string]*scenario.Report // the last report of each scenario run through sandbox_scenario
}

func newMCPCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the sandbox supervisor as an MCP server on stdio",
		Long: "Serve one MCP server on stdin/stdout that starts, restarts and stops locked sandbox\n" +
			"instances of this binary and proxies their lab and dev-MCP tools. The instance\n" +
			"tokens never leave this process. Instances stop when the client disconnects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if runtime.GOOS == "windows" {
				return supervisor.ErrUnixOnly
			}
			logger.SetLevel(logger.FATAL) // stdout carries the protocol; nothing else may write to it
			sup, err := supervisor.New(supervisor.Config{})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			runErr := newMCPServer(sup).Run(ctx, &mcp.StdioTransport{})
			cctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			defer cancel()
			if cerr := sup.Close(cctx); cerr != nil {
				runErr = errors.Join(runErr, cerr)
			}
			if errors.Is(runErr, context.Canceled) {
				return nil
			}
			return runErr
		},
	}
}

// newMCPServer builds the server over sup; the tools exist before any instance does.
func newMCPServer(sup *supervisor.Supervisor) *mcp.Server {
	m := &mcpServer{sup: sup, srv: mcp.NewServer(&mcp.Implementation{Name: "khunquant-sandbox", Version: "1"}, nil)}
	m.srv.AddTool(&mcp.Tool{
		Name: "sandbox_instance",
		Description: "Start, stop, restart and inspect locked sandbox instances of this binary. " +
			"ops: up, down, list, restart (mode graceful|kill), logs (tail, contains).",
		InputSchema: instanceSchema(),
	}, m.instanceHandler)
	m.srv.AddTool(&mcp.Tool{
		Name: "sandbox_wait_for",
		Description: "Wait in wall time until journal entries match, across restarts. " +
			"match: {kind, subject, fields, count}; after_seq: only entries after it; timeout_wall default 60s, max 10m.",
		InputSchema: json.RawMessage(`{"type":"object","required":["match"],"additionalProperties":false,"properties":{
			"instance":` + instanceProp + `,
			"match":{"type":"object","description":"{kind (exact or prefix.*), subject, fields (dotted paths into the payload), count (\"1\", \">= 2\", \"0\" = absence)}"},
			"after_seq":{"type":"integer","description":"journal cursor; default 0"},
			"timeout_wall":{"type":"string","description":"Go duration, default 60s, at most 10m"}}}`),
	}, m.waitForHandler)
	m.registerProxies()
	m.registerScenarioTools()
	return m.srv
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult("%v", err)
	}
	return textResult(string(b))
}

func errResult(format string, a ...any) *mcp.CallToolResult {
	r := textResult(fmt.Sprintf(format, a...))
	r.IsError = true
	return r
}

// instanceSchema is generated from the UpOptions table, so every option of
// `sandbox up` is an argument of op=up.
func instanceSchema() json.RawMessage {
	props := map[string]any{
		"op":        map[string]any{"type": "string", "enum": []string{"up", "down", "list", "restart", "logs"}},
		"keep_home": map[string]any{"type": "boolean", "description": "down: keep the home (logs, config)"},
		"mode":      map[string]any{"type": "string", "enum": []string{"graceful", "kill"}, "description": "restart: SIGTERM then SIGKILL, or SIGKILL at once"},
		"tail":      map[string]any{"type": "integer", "description": "logs: last lines, default 200, at most 2000"},
		"contains":  map[string]any{"type": "string", "description": "logs: only lines containing this"},
	}
	for _, f := range supervisor.UpOptionFields() {
		props[f.JSON] = map[string]any{"type": jsonType(f.Kind), "description": f.Desc}
	}
	b, _ := json.Marshal(map[string]any{"type": "object", "required": []string{"op"}, "additionalProperties": false, "properties": props})
	return b
}

func jsonType(k reflect.Kind) string {
	switch k {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64:
		return "integer"
	}
	return "string"
}

// instanceArgs are the arguments of sandbox_instance. The embedded UpOptions brings
// the generated ones (name, fixtures_from, ...).
type instanceArgs struct {
	supervisor.UpOptions
	Op       string `json:"op"`
	KeepHome bool   `json:"keep_home"`
	Mode     string `json:"mode"`
	Tail     int    `json:"tail"`
	Contains string `json:"contains"`
}

func (m *mcpServer) instanceHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a instanceArgs
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return errResult("sandbox_instance: %v", err), nil
	}
	switch a.Op {
	case "up":
		if a.Name == "" {
			a.Name = "default"
		}
		info, err := m.sup.Up(ctx, a.UpOptions)
		if err != nil {
			return errResult("%v", err), nil
		}
		m.refine(ctx, info.Name)
		return jsonResult(info), nil
	case "down":
		if err := m.sup.Down(ctx, a.Name, a.KeepHome); err != nil {
			return errResult("%v", err), nil
		}
		return jsonResult(map[string]any{"down": true, "kept_home": a.KeepHome}), nil
	case "list":
		return jsonResult(map[string]any{"instances": m.sup.List()}), nil
	case "restart":
		info, err := m.sup.Restart(ctx, a.Name, supervisor.RestartMode(a.Mode))
		if err != nil {
			return errResult("%v", err), nil
		}
		return jsonResult(info), nil
	case "logs":
		lines, err := m.sup.Logs(a.Name, a.Tail, a.Contains)
		if err != nil {
			return errResult("%v", err), nil
		}
		return jsonResult(map[string]any{"lines": lines}), nil
	}
	return errResult("sandbox_instance: op must be one of up, down, list, restart, logs (got %q)", a.Op), nil
}

func (m *mcpServer) waitForHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Instance    string           `json:"instance"`
		Match       labjournal.Match `json:"match"`
		AfterSeq    int64            `json:"after_seq"`
		TimeoutWall string           `json:"timeout_wall"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return errResult("sandbox_wait_for: %v", err), nil
	}
	total := defaultWaitFor
	if a.TimeoutWall != "" {
		d, err := time.ParseDuration(a.TimeoutWall)
		if err != nil || d <= 0 || d > maxWaitFor {
			return errResult("sandbox_wait_for: timeout_wall must be a duration up to %s", maxWaitFor), nil
		}
		total = d
	}
	once := func(ctx context.Context, wall time.Duration) (labjournal.WaitResult, error) {
		c, err := m.sup.Client(a.Instance)
		if err != nil {
			return labjournal.WaitResult{}, err
		}
		return c.WaitFor(ctx, a.Match, a.AfterSeq, wall)
	}
	res, err := waitLoop(ctx, a.Match, total, once, m.recoverFrom(a.Instance))
	if err != nil {
		return errResult("sandbox_wait_for: %v", err), nil
	}
	return jsonResult(res), nil
}

// recoverFrom decides what a failed wait call means. An instance that is restarting
// is waited for, whatever the call said (a stopping gateway may answer with an error of
// its own); one that has exited is a failure. A running one that answered with an error
// is a failure too, and one that did not answer is retried after a moment.
func (m *mcpServer) recoverFrom(instance string) func(context.Context, error) error {
	return func(ctx context.Context, err error) error {
		info, gerr := m.sup.Get(instance)
		if gerr != nil {
			return gerr
		}
		var le *labclient.Error
		if info.State == supervisor.StateRunning && errors.As(err, &le) {
			return err
		}
		if info.State == supervisor.StateRunning {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		_, werr := m.sup.WaitRunning(ctx, instance)
		return werr
	}
}

// waitLoop turns the instance's bounded waits (at most labjournal.MaxWait) into one
// wall-time wait of total. The cursor stays at the caller's after_seq, so a count such
// as ">= 2" accumulates across calls. A presence check ends when it holds; an absence
// check ("0", "<= 2") only when the window is over, or at once when the bound is
// exceeded. once makes one call; recoverFn says whether a failed call is worth retrying.
func waitLoop(ctx context.Context, m labjournal.Match, total time.Duration,
	once func(ctx context.Context, wall time.Duration) (labjournal.WaitResult, error),
	recoverFn func(context.Context, error) error,
) (labjournal.WaitResult, error) {
	lo, hi, err := m.Bounds()
	if err != nil {
		return labjournal.WaitResult{}, err
	}
	absence := lo == 0 && hi >= 0
	end := time.Now().Add(total)
	var res labjournal.WaitResult
	for {
		began := time.Now()
		r, err := once(ctx, min(time.Until(end), labjournal.MaxWait))
		if err != nil {
			if rerr := recoverFn(ctx, err); rerr != nil {
				return res, rerr
			}
		} else {
			res = r
			if (hi >= 0 && r.Count > hi) || (r.Matched && !absence) {
				return res, nil
			}
		}
		if time.Until(end) <= 0 {
			return res, nil
		}
		if time.Since(began) < 100*time.Millisecond { // never spin on a call that returns at once
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}
