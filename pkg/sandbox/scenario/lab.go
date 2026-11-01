package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// SettleError is a settle that did not pass: the components that were still busy. A run
// that meets one is an infrastructure failure, never a pass.
type SettleError struct {
	Busy   []string
	Detail string
}

func (e *SettleError) Error() string {
	return fmt.Sprintf("not settled: %s (%s)", strings.Join(e.Busy, ", "), e.Detail)
}

// jsonable converts a decoded YAML value into one encoding/json accepts (a mapping with
// non-string keys decodes to map[any]any).
func jsonable(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = jsonable(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = jsonable(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jsonable(e)
		}
		return out
	}
	return v
}

// call runs one op of a lab group with args (op "" for a single-op group) and returns the
// JSON result.
func call(ctx context.Context, c *labclient.Client, group, op string, args map[string]any) (json.RawMessage, error) {
	body := map[string]any{}
	for k, v := range args {
		body[k] = jsonable(v)
	}
	if op != "" {
		body["op"] = op
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", group, op, err)
	}
	out, err := c.Call(ctx, group, raw)
	if err == nil && group == "call_tool" { // a tool that failed is still an answer to the lab
		var r struct {
			IsError bool   `json:"is_error"`
			ForLLM  string `json:"for_llm"`
		}
		if json.Unmarshal(out, &r) == nil && r.IsError {
			return out, fmt.Errorf("tool %v failed: %s", body["name"], r.ForLLM)
		}
	}
	return out, err
}

// clockNow reads the instance's sim time.
func clockNow(ctx context.Context, c *labclient.Client) (time.Time, error) {
	raw, err := call(ctx, c, "clock", "get", nil)
	if err != nil {
		return time.Time{}, err
	}
	var st struct {
		Now time.Time `json:"now"`
	}
	return st.Now, json.Unmarshal(raw, &st)
}

// failedSettle reports a settle after seq that did not pass (the lab journals every
// settle, the ones inside an advance as well) and the components it found busy.
func failedSettle(ctx context.Context, c *labclient.Client, since int64) (*SettleError, error) {
	res, err := c.WaitFor(ctx, labjournal.Match{Kind: "settle", Fields: map[string]any{"ok": false}, Count: ">= 1"}, since, time.Millisecond)
	if err != nil || !res.Matched {
		return nil, err
	}
	busy := map[string]bool{}
	var names []string
	for _, e := range res.Entries {
		var p struct {
			Busy   []string `json:"busy"`
			WallMS int64    `json:"wall_ms"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		for _, b := range p.Busy {
			if !busy[b] {
				busy[b] = true
				names = append(names, b)
			}
		}
	}
	return &SettleError{Busy: names, Detail: fmt.Sprintf("%d settle(s) failed", res.Count)}, nil
}

// journalHead is the newest journal seq (0 for an empty journal) and the drop count.
func journalHead(ctx context.Context, c *labclient.Client) (seq int64, drops uint64, err error) {
	raw, err := call(ctx, c, "inspect", "journal", map[string]any{"limit": 1})
	if err != nil {
		return 0, 0, err
	}
	var out struct {
		NextSeq int64  `json:"next_seq"`
		Drops   uint64 `json:"drops"`
	}
	return out.NextSeq, out.Drops, json.Unmarshal(raw, &out)
}

// advanceTo moves the clock to target in calls of at most wall each (until deadline, if set) (the instance
// answers done:false when the budget ends) and settles after every step inside them. A
// settle that did not pass comes back as a *SettleError.
func advanceTo(ctx context.Context, c *labclient.Client, target time.Time, wall time.Duration, deadline time.Time) error {
	since, _, err := journalHead(ctx, c)
	if err != nil {
		return err
	}
	for {
		if now, err := clockNow(ctx, c); err != nil || !now.Before(target) {
			return err
		}
		raw, err := call(ctx, c, "clock", "advance", map[string]any{"to": target.Format(time.RFC3339Nano), "wall_budget": wall.String()})
		if err != nil {
			var le *labclient.Error
			if errors.As(err, &le) {
				if se, ferr := failedSettle(ctx, c, since); ferr == nil && se != nil {
					return se
				}
			}
			return err
		}
		var sum struct {
			Done bool `json:"done"`
		}
		if err := json.Unmarshal(raw, &sum); err != nil {
			return err
		}
		if sum.Done {
			return nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return errMaxWall
		}
	}
}

// errMaxWall ends an advance whose caller's wall-time limit passed before the target.
var errMaxWall = errors.New("max_wall exceeded")

// settle waits for every component to be idle; a settle that fails is a *SettleError.
func settle(ctx context.Context, c *labclient.Client, wallCap time.Duration) error {
	raw, err := call(ctx, c, "inspect", "settle", map[string]any{"wall_cap": wallCap.String()})
	if err != nil {
		return err
	}
	var out struct {
		Settled bool     `json:"settled"`
		Busy    []string `json:"busy"`
		WallMS  int64    `json:"wall_ms"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if !out.Settled {
		return &SettleError{Busy: out.Busy, Detail: fmt.Sprintf("after %dms", out.WallMS)}
	}
	return nil
}
