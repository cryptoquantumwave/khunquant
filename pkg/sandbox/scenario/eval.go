package scenario

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// env is what an expectation is read against: the instance's journal (fetched once per
// kind set and extended on demand) and, for logs, the gateway's log lines.
type env struct {
	c     *labclient.Client
	start time.Time // clock.start: the origin of within offsets
	loc   *time.Location
	logs  func() ([]string, error) // nil: no log source
	cache map[string]*fetched
}

type fetched struct {
	entries []labjournal.Entry
	cursor  int64
}

func newEnv(c *labclient.Client, start time.Time, loc *time.Location) *env {
	return &env{c: c, start: start, loc: loc, cache: map[string]*fetched{}}
}

// entriesOf returns every journal entry of the kinds, oldest first, fetching only what
// is new since the last call.
func (v *env) entriesOf(ctx context.Context, kinds []string) ([]labjournal.Entry, error) {
	key := strings.Join(kinds, ",")
	f := v.cache[key]
	if f == nil {
		f = &fetched{}
		v.cache[key] = f
	}
	for {
		raw, err := call(ctx, v.c, "inspect", "journal", map[string]any{"kinds": kinds, "after_seq": f.cursor, "limit": labjournal.MaxLimit, "from_start": true})
		if err != nil {
			return nil, err
		}
		var out struct {
			Entries []labjournal.Entry `json:"entries"`
			NextSeq int64              `json:"next_seq"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		f.entries = append(f.entries, out.Entries...)
		f.cursor = max(f.cursor, out.NextSeq)
		if len(out.Entries) < labjournal.MaxLimit {
			return f.entries, nil
		}
	}
}

func expectID(i int, e *Expectation) string {
	if e.ID != "" {
		return e.ID
	}
	return fmt.Sprintf("expect-%d", i)
}

// picker decides, for one entry whose payload is doc, whether it counts.
type picker func(e *labjournal.Entry, doc map[string]any) bool

// plan is how one expectation reads the journal: the kinds, which entries count, and
// a description of the wish for the report.
type plan struct {
	kinds []string
	keep  picker
	want  string
}

// eval evaluates one expectation and returns the journal entries that matched it.
func (v *env) eval(ctx context.Context, i int, e *Expectation) (ExpectResult, []labjournal.Entry) {
	r := ExpectResult{ID: expectID(i, e), Kind: e.Kind}
	lo, hi, err := (labjournal.Match{Count: e.Count}).Bounds()
	from, to, hasTo, werr := e.Within.Bounds()
	dFrom, dTo, derr := 0, 0, error(nil)
	if e.During != "" {
		dFrom, dTo, derr = ParseDuring(e.During)
	}
	if err = firstErr(err, werr, derr); err != nil {
		r.Want, r.Got = e.Kind, err.Error()
		return r, nil
	}
	switch e.Kind {
	case "workflow", "probe", "logs":
		return v.evalOther(ctx, r, e, lo, hi)
	}
	p, err := v.planOf(e)
	if err != nil {
		r.Want, r.Got = e.Kind, err.Error()
		return r, nil
	}
	r.Want = p.want + countText(e.Count, lo, hi) + windowText(e)
	all, err := v.entriesOf(ctx, p.kinds)
	if err != nil {
		r.Got, r.err = "the journal could not be read: "+err.Error(), err
		return r, nil
	}
	var inWindow, matched []labjournal.Entry
	for k := range all {
		en := &all[k]
		at := en.SimTime
		if at.Before(v.start.Add(from)) || (hasTo && at.After(v.start.Add(to))) || (e.During != "" && !within(at.In(v.loc), dFrom, dTo)) {
			continue
		}
		inWindow = append(inWindow, *en)
		var doc map[string]any
		_ = json.Unmarshal(en.Payload, &doc)
		if p.keep(en, doc) {
			matched = append(matched, *en)
			r.MatchedSeqs = append(r.MatchedSeqs, en.Seq)
		}
	}
	n := len(matched)
	r.OK = n >= lo && (hi < 0 || n <= hi)
	r.Got = fmt.Sprintf("%d matched (of %d %s entries in the window)", n, len(inWindow), strings.Join(p.kinds, ", "))
	if !r.OK {
		hi := time.Time{}
		if hasTo {
			hi = v.start.Add(to)
		}
		r.Excerpt = v.excerpt(ctx, v.start.Add(from), hi)
	}
	return r, matched
}

// storyKinds are the journal kinds that tell what happened around a failed expectation:
// not the clock steps, settles and exchange requests that fill the journal.
var storyKinds = []string{"chat.*", "event.trigger.*", "event.attention.*", "tool.call", "proposal", "policy.decision", "config.reload"}

// excerpt is up to 20 entries of the story around [lo, hi] (hi zero: no end): the ones
// inside, and the nearest before and after when there are few.
func (v *env) excerpt(ctx context.Context, lo, hi time.Time) []labjournal.Entry {
	all, err := v.entriesOf(ctx, storyKinds)
	if err != nil {
		return nil
	}
	var before, inside, after []labjournal.Entry
	for _, e := range all {
		switch {
		case e.SimTime.Before(lo):
			before = append(before, e)
		case !hi.IsZero() && e.SimTime.After(hi):
			after = append(after, e)
		default:
			inside = append(inside, e)
		}
	}
	if len(inside) >= 20 {
		return inside[len(inside)-20:]
	}
	room := 20 - len(inside)
	nb := min(len(before), (room+1)/2)
	na := min(len(after), room-nb)
	nb = min(len(before), room-na)
	return append(append(slices.Clone(before[len(before)-nb:]), inside...), after[:na]...)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// within says whether t's wall-clock minute is inside [from, to) minutes of the day; a
// window whose end is not after its start wraps midnight.
func within(t time.Time, from, to int) bool {
	m := t.Hour()*60 + t.Minute()
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to
}

func countText(raw string, lo, hi int) string {
	switch {
	case raw != "":
		return ", count " + raw
	case lo == 1 && hi < 0:
		return ", at least 1"
	}
	return ""
}

func windowText(e *Expectation) string {
	s := ""
	if e.Within != nil {
		s += fmt.Sprintf(", within %s..%s", e.Within.From, e.Within.To)
	}
	if e.During != "" {
		s += ", during " + e.During
	}
	return s
}

func describe(kind string, fields map[string]any) string {
	b, _ := json.Marshal(fields)
	return kind + " " + string(b)
}

// strs reads a string or a list of strings.
func strs(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// matches compares a payload value with a wanted one. A wanted string is a substring
// test ("!x" negates it, and also holds when the path is missing); anything else is
// equal after JSON.
func matches(got any, ok bool, want any) bool {
	if s, isStr := want.(string); isStr {
		neg := strings.HasPrefix(s, "!")
		has := ok && strings.Contains(fmt.Sprint(got), strings.TrimPrefix(s, "!"))
		return has != neg
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return ok && string(a) == string(b)
}

// pathsKeep keeps entries whose payload satisfies every path -> wanted value.
func pathsKeep(paths map[string]any) picker {
	return func(_ *labjournal.Entry, doc map[string]any) bool {
		for p, want := range paths {
			got, ok := labjournal.Lookup(doc, p)
			if !matches(got, ok, want) {
				return false
			}
		}
		return true
	}
}

// shorthand maps the fields of e that name payload paths (PayloadPaths, or the given
// table) to those paths; the rest stay in rest.
func shorthand(fields map[string]any, table map[string]string) (paths, rest map[string]any) {
	paths, rest = map[string]any{}, map[string]any{}
	for k, v := range fields {
		if p, ok := table[k]; ok {
			paths[p] = v
		} else {
			rest[k] = v
		}
	}
	return paths, rest
}

// orderPaths are the order-create endpoints of a venue in the request log (SBX-14a's
// exchange.order replaces this, and adds symbol, side and the rest).
var orderPaths = map[string][]string{
	"binance": {"/fapi/v1/order", "/fapi/v3/order", "/api/v3/order"},
	"okx":     {"/api/v5/trade/order"},
	"bitkub":  {"/api/v3/market/place-bid", "/api/v3/market/place-ask"},
}

func (v *env) planOf(e *Expectation) (*plan, error) {
	f := e.Fields
	switch e.Kind {
	case "message":
		to, contains, buttons := strs(f["to"]), strs(f["contains"]), strs(f["buttons"])
		keep := func(en *labjournal.Entry, doc map[string]any) bool {
			if d, _ := doc["delivered"].(bool); !d { // a refused send is journaled too, but nobody got it
				return false
			}
			text, _ := doc["text"].(string)
			var have []string
			if bs, _ := doc["buttons"].([]any); bs != nil {
				for _, b := range bs {
					if m, _ := b.(map[string]any); m != nil {
						have = append(have, fmt.Sprint(m["action"]))
					}
				}
			}
			return (len(to) == 0 || slices.ContainsFunc(to, func(p string) bool { return chatMatch(p, en.Subject) })) &&
				!slices.ContainsFunc(contains, func(c string) bool { return !strings.Contains(text, c) }) &&
				(f["severity"] == nil || doc["severity"] == f["severity"]) &&
				(f["silent"] == nil || doc["silent"] == f["silent"]) &&
				!slices.ContainsFunc(buttons, func(b string) bool { return !slices.Contains(have, b) })
		}
		return &plan{[]string{"chat.out"}, keep, describe("message", f)}, nil
	case "event", "decision":
		kind := "event.attention.decision"
		if e.Kind == "event" {
			k, _ := f["kind"].(string)
			if kind = EventJournalKind(k); k == "" {
				return nil, fmt.Errorf("event needs a kind")
			}
		}
		paths, rest := shorthand(f, PayloadPaths[kind])
		fields, _ := rest["fields"].(map[string]any)
		subject, _ := rest["subject"].(string)
		norm, _ := json.Marshal(fields)
		var want map[string]any
		_ = json.Unmarshal(norm, &want)
		for k, v := range want { // fields: are payload paths too
			paths[k] = v
		}
		keep := pathsKeep(paths)
		return &plan{[]string{kind}, func(en *labjournal.Entry, doc map[string]any) bool {
			return (subject == "" || en.Subject == subject) && keep(en, doc)
		}, describe(e.Kind+" "+kind, f)}, nil
	case "order":
		venue, _ := f["venue"].(string)
		if OrderJournalKind(f) == "exchange.order" {
			rest := map[string]any{}
			for k, v := range f {
				rest[k] = v
			}
			return &plan{[]string{"exchange.order"}, pathsKeep(rest), describe("order", f)}, nil
		}
		return &plan{[]string{"exchange.req"}, func(en *labjournal.Entry, doc map[string]any) bool {
			status, _ := doc["status"].(float64)
			return (venue == "" || en.Subject == venue) && doc["method"] == "POST" && status >= 200 && status < 300 &&
				slices.ContainsFunc(orderPaths[en.Subject], func(p string) bool {
					path, _ := doc["path"].(string)
					return strings.HasPrefix(path, p)
				})
		}, describe("order (order-create requests)", f)}, nil
	case "llm":
		turn, _ := f["turn"].(string)
		return &plan{[]string{"llm.call", "llm.complete"}, func(en *labjournal.Entry, _ map[string]any) bool {
			return turn == "" || (turn == "complete" && en.Kind == "llm.complete") || (en.Kind == "llm.call" && en.Subject == turn)
		}, describe("llm", f)}, nil
	case "proposal":
		paths, _ := shorthand(f, map[string]string{"kind": "event.payload.kind", "status": "event.payload.status"})
		return &plan{[]string{"proposal"}, pathsKeep(paths), describe("proposal", f)}, nil
	case "policy":
		paths, _ := shorthand(f, map[string]string{"verdict": "event.attrs.mode", "gate": "event.attrs.tool"})
		return &plan{[]string{"policy.decision"}, pathsKeep(paths), describe("policy", f)}, nil
	case "web":
		host, _ := f["host"].(string)
		return &plan{[]string{"web.req"}, func(en *labjournal.Entry, doc map[string]any) bool {
			h, _ := doc["host"].(string)
			return host == "" || strings.Contains(h, host) || strings.Contains(en.Subject, host)
		}, describe("web", f)}, nil
	}
	return nil, fmt.Errorf("unknown expectation kind %q", e.Kind)
}

// chatMatch says whether a chat id satisfies a pattern from `to`: a persona name means
// dm:<persona>; a pattern with a colon (group:*) is a glob over the chat id.
func chatMatch(pattern, chat string) bool {
	if !strings.Contains(pattern, ":") {
		pattern = "dm:" + pattern
	}
	ok, _ := path.Match(pattern, chat)
	return ok
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// evalOther evaluates the kinds that are not a plain journal read.
func (v *env) evalOther(ctx context.Context, r ExpectResult, e *Expectation, lo, hi int) (ExpectResult, []labjournal.Entry) {
	f := e.Fields
	switch e.Kind {
	case "logs":
		level, _ := f["level"].(string)
		re := regexp.MustCompile(`\b(ERR|ERROR)\b`)
		if level == "WARN" {
			re = regexp.MustCompile(`\b(ERR|ERROR|WRN|WARN)\b`)
		}
		r.Want = "no " + cmp.Or(level, "ERROR") + " log lines"
		if f["allow"] != nil {
			r.Want += " beyond " + fmt.Sprint(f["allow"])
		}
		if v.logs == nil {
			r.Got = "no log source"
			return r, nil
		}
		lines, err := v.logs()
		if err != nil {
			r.Got = err.Error()
			return r, nil
		}
		var bad []string
		for _, l := range lines {
			l = ansi.ReplaceAllString(l, "")
			if re.MatchString(l) && !slices.ContainsFunc(strs(f["allow"]), func(a string) bool { return strings.Contains(l, a) }) {
				bad = append(bad, l)
			}
		}
		r.OK, r.Got = len(bad) == 0, fmt.Sprintf("%d such lines", len(bad))
		if len(bad) > 0 {
			r.Got += ": " + strings.Join(bad[:min(len(bad), 3)], " | ")
		}
	case "workflow":
		r.Want = describe("workflow", f) + countText(e.Count, lo, hi)
		raw, err := call(ctx, v.c, "inspect", "workflows", map[string]any{"kind": f["kind"]})
		if err != nil {
			r.Got = err.Error()
			return r, nil
		}
		var out struct {
			Workflows []struct{ Kind, Status string }
		}
		_ = json.Unmarshal(raw, &out)
		n := 0
		for _, w := range out.Workflows {
			if state, _ := f["state"].(string); state == "" || w.Status == state {
				n++
			}
		}
		r.OK, r.Got = n >= lo && (hi < 0 || n <= hi), fmt.Sprintf("%d workflows", n)
	case "probe":
		r.OK, r.Want, r.Got = v.probe(ctx, f)
	}
	return r, nil
}

// probe reads one value out of the instance through a tool or route the owner has and
// compares it with equals: alert_job_exists(id) is whether set_price_alert list names
// the id; dn_plan_status(id) and mandate_status(id) are substring tests of the tool's
// or route's answer, because their answers are text.
func (v *env) probe(ctx context.Context, f map[string]any) (ok bool, want, got string) {
	ask := func(group, op string, args map[string]any) string {
		raw, err := call(ctx, v.c, group, op, args)
		if err != nil {
			return "error: " + err.Error()
		}
		return string(raw)
	}
	switch {
	case f["alert_job_exists"] != nil:
		id := fmt.Sprint(f["alert_job_exists"])
		answer := ask("call_tool", "", map[string]any{"name": "set_price_alert", "args": map[string]any{"action": "list"}, "as": "owner"})
		has := strings.Contains(answer, id)
		return has == (f["equals"] != false), fmt.Sprintf("alert job %s exists: %v", id, f["equals"] != false), fmt.Sprintf("exists: %v", has)
	case f["dn_plan_status"] != nil:
		answer := ask("call_tool", "", map[string]any{"name": "get_delta_neutral_plan", "args": map[string]any{"plan_id": f["dn_plan_status"]}, "as": "owner"})
		return strings.Contains(answer, fmt.Sprint(f["equals"])), fmt.Sprintf("plan %v status contains %v", f["dn_plan_status"], f["equals"]), clip(answer)
	case f["mandate_status"] != nil:
		answer := ask("manage", "", map[string]any{"method": "GET", "route": "/api/proactive/mandates"})
		return strings.Contains(answer, fmt.Sprint(f["mandate_status"])) && strings.Contains(answer, fmt.Sprint(f["equals"])),
			fmt.Sprintf("mandate %v with %v", f["mandate_status"], f["equals"]), clip(answer)
	}
	return false, "a probe", "trigger_state has no state view in this build"
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// expectationOf reads an expectation from the mapping form a run_until step or the
// sandbox_run_until tool carries ({message: {...}, count: ">= 1"}).
func expectationOf(m map[string]any) (*Expectation, error) {
	b, err := yaml.Marshal(jsonable(m))
	if err != nil {
		return nil, err
	}
	var e Expectation
	if err := yaml.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	if _, ok := expectSpec[e.Kind]; !ok || len(e.kinds) != 1 {
		return nil, errors.New("until takes one expectation: a mapping with one kind key such as message")
	}
	if e.Kind == "event" {
		k, _ := e.Fields["kind"].(string)
		if msg := EventKindIssue(EventJournalKind(k)); msg != "" {
			return nil, errors.New("until: " + msg)
		}
	}
	return &e, nil
}

// untilCheck is the Check of a RunUntil that stops when e holds; the entry is the last
// one that matched, so entry.sim_time is the instant of the match.
func (v *env) untilCheck(e *Expectation) func(context.Context) (bool, *labjournal.Entry, error) {
	return func(ctx context.Context) (bool, *labjournal.Entry, error) {
		r, matched := v.eval(ctx, 0, e)
		if r.err != nil || !r.OK {
			return false, nil, r.err
		}
		if len(matched) == 0 {
			return true, nil, nil
		}
		return true, &matched[len(matched)-1], nil
	}
}

// UntilOptionsFrom reads {until: <expectation>, step, max_sim, max_wall} (the strings are
// durations) into options that run until the expectation holds. start is the origin of
// the expectation's within offsets.
func UntilOptionsFrom(c *labclient.Client, args map[string]any, start time.Time, loc *time.Location) (UntilOptions, error) {
	return newEnv(c, start, loc).untilOptions(args)
}

func (v *env) untilOptions(args map[string]any) (o UntilOptions, err error) {
	for k, dst := range map[string]*time.Duration{"step": &o.Step, "max_sim": &o.MaxSim, "max_wall": &o.MaxWall} {
		if s, ok := args[k].(string); ok {
			if *dst, err = time.ParseDuration(s); err != nil || *dst <= 0 {
				return o, fmt.Errorf("%s %q is not a positive duration", k, s)
			}
		}
	}
	m, _ := args["until"].(map[string]any)
	if m == nil {
		return o, errors.New("until must be an expectation mapping")
	}
	e, err := expectationOf(m)
	if err != nil {
		return o, err
	}
	o.Check = v.untilCheck(e)
	return o, nil
}
