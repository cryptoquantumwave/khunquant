package scenario

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// Caps is what a running instance offers, read from GET /api/sandbox/lab: its lab groups
// with their op names, and the journal kinds it declares. A nil Kinds means "not known":
// the kind checks are skipped; an empty non-nil list declares nothing.
type Caps struct {
	Groups map[string][]string
	Kinds  []string
}

var (
	idRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	matrixRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type validator struct {
	f      *File
	caps   *Caps
	issues []Issue
}

func (v *validator) addf(path, format string, a ...any) {
	v.issues = append(v.issues, Issue{path, fmt.Sprintf(format, a...)})
}

// need reports a feature the instance cannot serve: a group (and op) it lacks.
func (v *validator) need(path, what string, s stepSpec) {
	if v.caps == nil || s.Group == "" {
		return
	}
	ops, ok := v.caps.Groups[s.Group]
	switch {
	case !ok:
		v.addf(path, "`%s` needs group `%s` (%s)", what, s.Group, s.PR)
	case s.Op != "" && !slices.Contains(ops, s.Op):
		v.addf(path, "`%s` needs op `%s` of group `%s` (%s)", what, s.Op, s.Group, s.PR)
	}
}

// needKind reports a journal kind the instance does not declare.
func (v *validator) needKind(path, what, kind string) {
	if v.caps != nil && v.caps.Kinds != nil && !KindDeclared(v.caps.Kinds, kind) {
		v.addf(path, "`%s` needs journal kind `%s` (%s)", what, kind, ownerOf(kind))
	}
}

// Validate checks a scenario and returns every problem with a JSON pointer. With caps
// nil only what the file alone shows is checked; with caps, a feature the instance
// lacks is reported with the PR that adds it. A scenario that Validate accepts can run.
func Validate(f *File, caps *Caps) []Issue {
	v := &validator{f: f, caps: caps, issues: slices.Clone(f.keyIssues)}
	if f.raw != nil {
		v.scanRaw(f.raw, "")
	}
	v.header()
	v.instance()
	v.blocks()
	v.steps("/setup", f.Setup, false)
	v.steps("/timeline", f.Timeline, true)
	v.expects()
	return v.issues
}

func (v *validator) header() {
	f := v.f
	if f.Version != 1 {
		v.addf("/version", "must be 1 (got %d)", f.Version)
	}
	if !idRE.MatchString(f.Scenario) {
		v.addf("/scenario", "an id of lower-case letters, digits, . _ - is required")
	}
	switch f.Status {
	case "", StatusActive:
		if f.KnownBug != "" {
			v.addf("/known_bug", "only allowed with status: known_bug")
		}
	case StatusKnownBug:
		if f.KnownBug == "" {
			v.addf("/known_bug", "required when status is known_bug: name the defect")
		}
	default:
		v.addf("/status", "must be active or known_bug")
	}
	switch f.LLM {
	case "", "scripted", "replay", "live":
	default:
		v.addf("/llm", "must be scripted, replay or live")
	}
	switch {
	case f.Miss != "" && f.LLM != "replay":
		v.addf("/miss", "only allowed with llm: replay")
	case f.Miss != "" && f.Miss != "error" && f.Miss != "scripted":
		v.addf("/miss", "must be error or scripted")
	}
	if !slices.Contains([]string{"", "wire", "event"}, f.Level) {
		v.addf("/level", "must be wire or event")
	}
	if f.Runs < 0 {
		v.addf("/runs", "must not be negative")
	}
	if f.PassRate < 0 || f.PassRate > 1 {
		v.addf("/pass_rate", "must be between 0 and 1")
	}
	if !slices.Contains([]string{"", "main", "stack-head"}, f.Base) {
		v.addf("/base", "must be main or stack-head")
	}
	for k, vals := range f.Matrix {
		if !matrixRE.MatchString(k) {
			v.addf("/matrix/"+esc(k), "a matrix name is a plain identifier")
		}
		if len(vals) == 0 {
			v.addf("/matrix/"+esc(k), "needs at least one value")
		}
	}
	if _, err := time.Parse(time.RFC3339, f.Clock.Start); err != nil {
		v.addf("/clock/start", "required, RFC 3339 (such as 2026-10-20T09:30:00+07:00)")
	}
	if f.Clock.Mode != "" && f.Clock.Mode != "manual" {
		v.addf("/clock/mode", "must be manual: a scenario needs a clock it controls")
	}
	if d, err := time.ParseDuration(f.Clock.Resolution); f.Clock.Resolution != "" && (err != nil || d < 0) {
		v.addf("/clock/resolution", "must be a duration >= 0")
	}
}

func (v *validator) instance() {
	for _, p := range sortedKeys(v.f.Instance.Workspace) {
		if err := supervisor.CheckWorkspacePath(p); err != nil {
			v.addf("/instance/workspace/"+esc(p), "%v", err)
		}
	}
	v.patch("/instance/config", v.f.Instance.Config)
	if err := supervisor.CheckAccounts(v.f.Instance.Accounts); err != nil {
		v.addf("/instance/accounts", "%v", err)
	}
	if err := supervisor.CheckAccountPatch(v.f.Instance.Config, v.f.Instance.Accounts); err != nil {
		v.addf("/instance/config/exchanges", "%v", err)
	}
}

func (v *validator) patch(base string, cfg map[string]any) {
	for _, pi := range supervisor.CheckConfigPatch(cfg) {
		p := base
		for _, seg := range pi.Path {
			p += "/" + esc(seg)
		}
		v.addf(p, "%s", pi.Msg)
	}
}

// blocks checks the top-level blocks that are applied through lab groups.
func (v *validator) blocks() {
	f := v.f
	if len(f.Profile) > 0 {
		v.need("/profile", "profile", blockSpec["profile"])
	}
	for _, sym := range sortedKeys(f.Market) {
		m, p := f.Market[sym], "/market/"+esc(sym)
		model := m.Model
		if model == "" {
			model = "set"
		}
		if !slices.Contains([]string{"set", "walk", "replay", "recording"}, model) {
			v.addf(p+"/model", "must be set, walk, replay or recording")
			continue
		}
		spec := stepKinds["market."+model]
		args := map[string]any{"symbol": sym}
		for k, a := range m.Args {
			args[k] = a
		}
		v.required(p, spec, args)
		v.need(p, "market."+model, spec)
	}
	for _, key := range sortedKeys(f.Account) {
		v.need("/account/"+esc(key), "account", blockSpec["account"])
	}
	for name, n := range map[string]int{"faults": len(f.Faults), "web": len(f.Web), "llm_script": len(f.LLMScript)} {
		if n > 0 {
			v.need("/"+name, name, blockSpec[name])
		}
	}
}

// required reports each of spec's required arguments that args lacks.
func (v *validator) required(path string, spec stepSpec, args map[string]any) {
	for _, r := range spec.Req {
		if _, ok := args[r]; !ok {
			v.addf(path+"/"+r, "required")
		}
	}
}

func (v *validator) steps(base string, steps []Step, timeline bool) {
	var last time.Duration
	for i := range steps {
		s := &steps[i]
		p := fmt.Sprintf("%s/%d", base, i)
		switch {
		case s.bad != "":
			v.addf(p, "%s", s.bad)
			continue
		case len(s.kinds) == 0:
			v.addf(p, "a step needs one kind key (chat, market.path, advance, ...)")
			continue
		case len(s.kinds) > 1:
			v.addf(p, "a step has one kind key, not %d (%s): split it into steps", len(s.kinds), strings.Join(s.kinds, ", "))
			continue
		}
		if s.At != "" {
			d, err := Offset(s.At)
			switch {
			case !timeline:
				v.addf(p+"/at", "setup runs before the clock moves: no at")
			case err != nil:
				v.addf(p+"/at", "%v", err)
			case d < last:
				v.addf(p+"/at", "%s is before the previous step (%s): offsets must not decrease", s.At, last)
			default:
				last = d
			}
		}
		spec, ok := stepKinds[s.Kind]
		if !ok {
			v.addf(p+"/"+esc(s.Kind), "unknown step kind %q", s.Kind)
			continue
		}
		v.step(p+"/"+esc(s.Kind), s, spec)
	}
}

func (v *validator) step(p string, s *Step, spec stepSpec) {
	scalar, isScalar := s.Scalar()
	if isScalar && s.Args.Tag == "!!null" {
		isScalar = false
	}
	args, err := s.Map()
	switch s.Kind {
	case "advance":
		if d, err := time.ParseDuration(scalar); !isScalar || err != nil || d <= 0 {
			v.addf(p, "advance takes a duration such as 15m")
		}
	case "run_until":
		if isScalar {
			if _, err := ParseTarget(scalar); err != nil {
				v.addf(p, "%v", err)
			}
		} else if err != nil || args["until"] == nil {
			v.addf(p, "run_until takes a target (+2h, an RFC 3339 time, next HH:MM) or {until: <expectation>, max_sim}")
		}
	case "restart":
		switch {
		case isScalar && (scalar == "graceful" || scalar == "kill"):
		case err == nil && args["kill_at"] != nil:
			if _, kerr := KillAtOf(args); kerr != nil {
				v.addf(p+"/kill_at", "%v", kerr)
			}
		default:
			v.addf(p, "restart takes graceful, kill or {kill_at: {workflow, step, point}}")
		}
	case "config.patch":
		if err != nil || len(args) == 0 {
			v.addf(p, "config.patch takes a partial config")
		}
		v.patch(p, args)
		if err := supervisor.CheckAccountPatch(args, v.f.Instance.Accounts); err != nil {
			v.addf(p+"/exchanges", "%v", err)
		}
	default:
		if err != nil {
			v.addf(p, "%v", err)
			return
		}
		v.required(p, spec, args)
		if s.Kind == "call_tool" && args["as"] == "cli" {
			v.addf(p+"/as", "cli is the local console: it passes every owner gate whatever proactive.owner says, and no chat turn can carry it; use owner or a persona")
		}
		for _, k := range []string{"wall_cap", "timeout_wall"} {
			if str, ok := args[k].(string); ok {
				if d, err := time.ParseDuration(str); err != nil || d <= 0 {
					v.addf(p+"/"+k, "must be a duration such as 5s")
				}
			}
		}
	}
	v.need(p, s.Kind, spec)
}

func (v *validator) expects() {
	if len(v.f.Expect) == 0 { // the implicit checks alone would pass any run that ends settled
		v.addf("/expect", "at least one expectation is required: a scenario that expects nothing passes whatever happens")
	}
	seen := map[string]bool{}
	for i := range v.f.Expect {
		e := &v.f.Expect[i]
		p := fmt.Sprintf("/expect/%d", i)
		switch {
		case e.bad != "":
			v.addf(p, "%s", e.bad)
			continue
		case len(e.kinds) != 1:
			v.addf(p, "an expectation has one kind key (message, event, decision, ...), got %d", len(e.kinds))
			continue
		}
		if e.ID != "" && seen[e.ID] {
			v.addf(p+"/id", "duplicate id %q", e.ID)
		}
		seen[e.ID] = true
		spec, ok := expectSpec[e.Kind]
		if !ok {
			v.addf(p+"/"+esc(e.Kind), "unknown expectation kind %q", e.Kind)
			continue
		}
		kp := p + "/" + e.Kind
		for _, k := range sortedKeys(e.Fields) {
			if !slices.Contains(spec.Fields, k) {
				v.addf(kp+"/"+esc(k), "unknown field for %s (one of: %s)", e.Kind, strings.Join(spec.Fields, ", "))
			}
		}
		for _, k := range e.dups {
			v.addf(p+"/"+k, "given both inside %s and beside it", e.Kind)
		}
		for _, mi := range e.modIssues {
			v.addf(p+mi.Path, "%s", mi.Msg)
		}
		v.modifiers(p, e)
		v.expectKind(kp, e, spec.Journal)
	}
}

func (v *validator) modifiers(p string, e *Expectation) {
	if lo, hi, err := (labjournal.Match{Count: e.Count}).Bounds(); err != nil {
		v.addf(p+"/count", "must be 1, \">= 1\", \"<= 2\", \"0\" ...: %v", err)
	} else if lo == 0 && hi < 0 {
		v.addf(p+"/count", "%q holds whatever the journal holds, so the expectation checks nothing", e.Count)
	}
	if from, to, hasTo, err := e.Within.Bounds(); err != nil {
		v.addf(p+"/within", "%v", err)
	} else if hasTo && to < from {
		v.addf(p+"/within", "from is after to")
	}
	if _, _, err := ParseDuring(e.During); e.During != "" && err != nil {
		v.addf(p+"/during", "%v", err)
	}
	if e.ReplayMatch != "" && e.Kind != "message" {
		v.addf(p+"/replay_match", "only a message expectation has replay_match")
	} else if e.ReplayMatch != "" {
		// A should-not-fire label: the message must not arrive inside a closed window.
		if _, hi, err := (labjournal.Match{Count: e.Count}).Bounds(); err != nil || hi != 0 {
			v.addf(p+"/replay_match", "labels a message that must not be sent: it needs count: 0 (a sent message is labelled by its dedup key)")
		}
		if _, _, hasTo, _ := e.Within.Bounds(); !hasTo {
			v.addf(p+"/replay_match", "needs within with from and to: a label covers a closed window")
		}
	}
}

// expectKind checks what one expectation kind needs from the instance.
func (v *validator) expectKind(kp string, e *Expectation, journal string) {
	switch e.Kind {
	case "event":
		k, _ := e.Fields["kind"].(string)
		if k == "" {
			v.addf(kp+"/kind", "required: the journal kind, such as trigger.candidate or tool.call")
			return
		}
		journal = EventJournalKind(k)
		if msg := EventKindIssue(journal); msg != "" {
			v.addf(kp+"/kind", "%s", msg)
			return
		}
		for _, f := range []string{"trigger", "severity", "disposition", "reason"} {
			if _, ok := e.Fields[f]; ok && PayloadPaths[journal][f] == "" {
				v.addf(kp+"/"+f, "is a shorthand for event.trigger.candidate and event.attention.decision only; use fields: with a payload path")
			}
		}
		if PayloadPaths[journal]["disposition"] != "" {
			v.oneOf(kp, e.Fields, "disposition", true, dispositions)
		}
		v.payloadFields(kp, e, journal)
	case "decision":
		v.oneOf(kp, e.Fields, "disposition", true, dispositions)
	case "order":
		journal = OrderJournalKind(e.Fields)
		for _, f := range []string{"symbol", "side", "reduce_only", "client_id", "status"} {
			if _, ok := e.Fields[f]; ok {
				v.needKind(kp+"/"+f, "order."+f, "exchange.order")
			}
		}
		v.orderValues(kp, e.Fields, journal)
	case "probe":
		n := 0
		for _, name := range sortedKeys(probeGroup) {
			if _, ok := e.Fields[name]; ok {
				n++
				v.need(kp+"/"+name, "probe."+name, stepSpec{Group: probeGroup[name].group, PR: probeGroup[name].pr})
			}
		}
		if _, ok := e.Fields["equals"]; n != 1 || !ok {
			v.addf(kp, "a probe takes one of mandate_status, dn_plan_status, alert_job_exists, and equals")
		}
	case "workflow":
		v.need(kp, "workflow", stepSpec{Group: "inspect", Op: "workflows", PR: "SBX-05"})
		v.oneOf(kp, e.Fields, "state", false, workflowStates)
	case "logs":
		if lvl, ok := e.Fields["level"].(string); ok && !slices.Contains([]string{"ERROR", "WARN", "INFO"}, lvl) {
			v.addf(kp+"/level", "must be ERROR, WARN or INFO")
		}
	}
	if journal != "" {
		v.needKind(kp, e.Kind, journal)
	}
}

// The checks below are on values. A value the journal never holds matches nothing, so
// with a count of 0 the expectation would pass whatever happened.

// oneOf reports field f of fields when it is given and is not one of values (a "!x"
// stands for x when neg).
func (v *validator) oneOf(kp string, fields map[string]any, f string, neg bool, values []string) {
	raw, ok := fields[f]
	if !ok {
		return
	}
	s, isStr := raw.(string)
	if neg {
		s = strings.TrimPrefix(s, "!")
	}
	if !isStr || !slices.Contains(values, s) {
		v.addf(kp+"/"+f, "must be one of %s", strings.Join(values, ", "))
	}
}

// orderValues checks an order expectation's values against what its journal holds: not
// closed for filled, short for sell, the wire form BTCUSDT or the account key binance.spot.
func (v *validator) orderValues(kp string, f map[string]any, journal string) {
	if journal == "exchange.req" { // venue alone: the request log's subject, compared whole and never negated
		v.oneOf(kp, f, "venue", false, sortedKeys(orderPaths))
		return
	}
	v.oneOf(kp, f, "venue", true, sandbox.AccountVenues)
	v.oneOf(kp, f, "side", true, orderSides)
	v.oneOf(kp, f, "status", true, orderStatuses)
	if raw, ok := f["symbol"]; ok {
		s, isStr := raw.(string)
		s = strings.TrimPrefix(s, "!")
		venue, _ := f["venue"].(string)
		equityWire := slices.Contains(equityVenues, venue) && equityWireRE.MatchString(s)
		if !isStr || !(unifiedSymbolRE.MatchString(s) || equityWire) {
			v.addf(kp+"/symbol", "must be a unified symbol such as BTC/USDT, BTC/USDT:USDT, PTT/THB or AAPL/USD, not the venue's own form")
		}
	}
	if raw, ok := f["client_id"]; ok {
		if s, isStr := raw.(string); !isStr || strings.TrimPrefix(s, "!") == "" {
			v.addf(kp+"/client_id", "must be a string (quote a numeric id): the journal holds it as one")
		}
	}
	if raw, ok := f["reduce_only"]; ok {
		if _, isBool := raw.(bool); !isBool {
			v.addf(kp+"/reduce_only", "must be true or false")
		}
	}
}

// payloadFields checks the paths under an event expectation's fields: against the payload
// the journal kind's producer writes. For a kind whose payload is not declared here, a
// path cannot be checked, so an absence count on it is refused.
func (v *validator) payloadFields(kp string, e *Expectation, journal string) {
	raw, ok := e.Fields["fields"]
	if !ok {
		return
	}
	fields, isMap := raw.(map[string]any)
	if !isMap {
		v.addf(kp+"/fields", "must be a mapping of payload path to wanted value")
		return
	}
	s, known := payloadShape(journal)
	lo, _, err := (labjournal.Match{Count: e.Count}).Bounds()
	for _, p := range sortedKeys(fields) {
		switch {
		case known && !s.has(p):
			v.addf(kp+"/fields/"+esc(p), "no %s entry has this path (its payload has: %s)", journal, s.keys())
		case !known && err == nil && lo == 0:
			v.addf(kp+"/fields/"+esc(p), "the payload of %s is not declared here, so a misspelt path would match nothing and an absence count would pass whatever happened", journal)
		}
	}
}

// realHosts are the registrable domains of the real services a scenario never names.
var realHosts = []string{"binance.com", "binance.th", "okx.com", "okex.com", "bitkub.com", "settrade.com", "settrade.or.th", "webull.com", "webull.co.th"}

// scanRaw walks the whole document: no key may be a credential field and no URL may
// name a real exchange. (instance.config has the stricter loopback rule of its own.)
func (v *validator) scanRaw(n *yaml.Node, ptr string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, p := n.Content[i].Value, ptr+"/"+esc(n.Content[i].Value)
			switch {
			case p == "/instance/config":
			case config.CredentialFieldNames[k]:
				v.addf(p, "a scenario cannot carry a credential")
			default:
				v.scanRaw(n.Content[i+1], p)
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			v.scanRaw(c, fmt.Sprintf("%s/%d", ptr, i))
		}
	case yaml.ScalarNode:
		if u, err := url.Parse(n.Value); err == nil && strings.Contains(n.Value, "://") {
			for _, d := range realHosts {
				if h := u.Hostname(); h == d || strings.HasSuffix(h, "."+d) {
					v.addf(ptr, "a scenario cannot name a real exchange host (%s)", h)
				}
			}
		}
	}
}
