package worldmodel

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The expectation language: a boolean expression over a read-only view of the world
// model ("weight('crypto') < 30", "plan_funding_apy(12) > 10"), written by the owner as
// the assumption behind a goal and evaluated by the goal_discrepancy trigger. It is the
// expr-lang library the DCA triggers use, with three differences that matter for an
// expression an owner or a model wrote:
//
//   - the language is a WHITELIST (exprVet): literals, arithmetic, comparison, and/or/not,
//     the conditional operator and calls of the functions below. No builtin, no
//     member access, no closure, no range, no regex operator: nothing can loop, allocate
//     or reach anything but the functions in ExprEnv. The size is bounded (MaxExprBytes,
//     maxExprNodes) and so is the work of one evaluation (maxExprCalls calls, and the
//     caller's context, which every call checks).
//   - a function whose input is unknown makes the whole evaluation UNKNOWN, which is
//     neither true nor false: the scope records it and the caller must not count it.
//   - the functions read the View and nothing else: no network, no exchange, no
//     database, no write, no clock but the View's. A function that finds its fact
//     missing, stale, restored, below exchange trust or not finite is unknown.
//
// The functions (numbers are float64 unless said; percentages are 0..100):
//
//	price("BTC/USDT")        last price of the symbol on the first provider that has a fresh one
//	weight("crypto")         share of the portfolio in the class, in percent; classes: crypto,
//	                         th_equity, us_equity, cash
//	drawdown_day()           percent the portfolio is below its start-of-day value (negative: above)
//	plan_funding_apy(12)     the delta-neutral plan's current funding rate as an APY, in percent
//	plan_health(12)          the plan's health score, 0..100
//	position_value("SOL")    USD value of the holding (0 when it is not held)
//	now()                    unix seconds, by the View's clock
//	hour_local()             0..23, the hour in the owner's zone
const (
	// MaxExprBytes bounds an expression's source.
	MaxExprBytes = 500
	// ExprTimeBound is how long one evaluation may take; the trigger gives its context
	// this deadline and every function call checks the context.
	ExprTimeBound = 50 * time.Millisecond

	maxExprNodes = 128
	maxExprCalls = 16
	maxExprRefs  = 8

	exprPriceMaxAge     = 10 * time.Minute
	exprPortfolioMaxAge = 45 * time.Minute // three snapshot periods
	exprBaselineMaxAge  = 36 * time.Hour
	exprPlanMinAge      = 5 * time.Minute
	exprPlanMaxAge      = 6 * time.Hour
)

// The keys of a delta-neutral plan's health in the world model (pkg/deltaneutral/
// planhealth owns them; the world model may not import it, and a test in pkg/triggers
// pins that these stay equal).
const (
	ExprPlanKind        = "dn"
	ExprPlanKeyAPY      = "funding_apy_pct"
	ExprPlanKeyScore    = "score"
	ExprPlanKeyInterval = "interval_s"
	ExprPlanKeyDataOK   = "data_ok"
)

// ErrExprUnknown is what a function returns when its input is unknown.
var ErrExprUnknown = errors.New("worldmodel: unknown value")

var (
	exprSymbolPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/:._-]{0,31}$`)
	exprClasses       = []string{string(perception.ClassCrypto), string(perception.ClassTHEquity), string(perception.ClassUSEquity), string(perception.ClassCash)}
	exprFuncs         = []string{"price", "weight", "drawdown_day", "plan_funding_apy", "plan_health", "position_value", "now", "hour_local"}
)

// ExprFunctions lists the names the expectation language offers.
func ExprFunctions() []string { return append([]string(nil), exprFuncs...) }

// ExprRef is one value an evaluation read: the call, written by code from a validated
// argument, and the number it returned.
type ExprRef struct {
	Name  string
	Value float64
}

// ExprOption configures ExprEnv.
type ExprOption func(*ExprScope)

// WithExprProviders names the providers price() looks a symbol up on, in order. Without
// it price() is unknown.
func WithExprProviders(f func() []string) ExprOption { return func(s *ExprScope) { s.providers = f } }

// WithExprLocation gives hour_local() the owner's zone (called only when the
// expression uses it). Without it the zone is UTC.
func WithExprLocation(f func() *time.Location) ExprOption {
	return func(s *ExprScope) { s.location = f }
}

// ExprScope is the environment of ONE evaluation: the functions, over the View, and the
// record of what they read. It is not safe for concurrent use.
type ExprScope struct {
	v         View
	providers func() []string
	location  func() *time.Location

	ctx     context.Context
	calls   int
	unknown bool
	trust   proactive.Trust
	refs    map[string]float64
}

// ExprEnv returns the read-only environment of functions over v. v may be nil when the
// scope is only used for its types (compilation).
func ExprEnv(v View, opts ...ExprOption) *ExprScope {
	s := &ExprScope{v: v, trust: proactive.TrustSystem, refs: map[string]float64{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Env is the map the expression is compiled and run against.
func (s *ExprScope) Env() map[string]any {
	return map[string]any{
		"price": s.price, "weight": s.weight, "drawdown_day": s.drawdownDay,
		"plan_funding_apy": s.planFundingAPY, "plan_health": s.planHealth,
		"position_value": s.positionValue, "now": s.now, "hour_local": s.hourLocal,
	}
}

// Unknown reports that a function met an unknown input during the evaluation.
func (s *ExprScope) Unknown() bool { return s.unknown }

// Trust is the lowest trust among the facts the evaluation read (TrustSystem for none).
func (s *ExprScope) Trust() proactive.Trust { return s.trust }

// Refs are the values the evaluation read, by name.
func (s *ExprScope) Refs() []ExprRef {
	out := make([]ExprRef, 0, len(s.refs))
	for k, v := range s.refs {
		out = append(out, ExprRef{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *ExprScope) miss() (float64, error) {
	s.unknown = true
	return 0, ErrExprUnknown
}

// step counts a call and checks the bounds; false means the evaluation is unknown.
func (s *ExprScope) step() bool {
	s.calls++
	if s.v == nil || s.calls > maxExprCalls || (s.ctx != nil && s.ctx.Err() != nil) {
		s.unknown = true
		return false
	}
	return true
}

// note records a value read. The name is code-built from an argument that matched a
// fact in the View (or a class the code lists), never from the expression's text.
func (s *ExprScope) note(name string, value float64, tr proactive.Trust) {
	s.trust = proactive.MinTrust(s.trust, tr)
	if _, ok := s.refs[name]; ok || len(s.refs) < maxExprRefs {
		s.refs[name] = value
	}
}

func (s *ExprScope) price(sym string) (float64, error) {
	if !s.step() || !exprSymbolPattern.MatchString(sym) || s.providers == nil {
		return s.miss()
	}
	for _, prov := range s.providers() {
		p, at, ok := s.v.Price(prov, sym)
		if !ok || !finite(p) || p <= 0 || !s.v.Fresh(at, exprPriceMaxAge) {
			continue
		}
		if tr, ok := s.v.PriceTrust(prov, sym); ok && tr >= proactive.TrustExchange {
			s.note("price("+sym+")", p, tr)
			return p, nil
		}
	}
	return s.miss()
}

// portfolio is the last snapshot when it can be judged: fresh, from an exchange-level
// source, positive, and not while the portfolio source cannot read an account.
func (s *ExprScope) portfolio() (PortfolioView, bool) {
	pf, ok := s.v.Portfolio()
	if !ok || !s.v.Fresh(pf.At, exprPortfolioMaxAge) || pf.Trust < proactive.TrustExchange ||
		!finite(pf.TotalUSD) || pf.TotalUSD <= 0 {
		return pf, false
	}
	if ss, ok := s.v.SourceState(PortfolioSource); ok && ss.Degraded {
		return pf, false
	}
	return pf, true
}

func (s *ExprScope) weight(class string) (float64, error) {
	if !s.step() {
		return s.miss()
	}
	if !slices.Contains(exprClasses, class) {
		return s.miss()
	}
	pf, ok := s.portfolio()
	if !ok {
		return s.miss()
	}
	w := pf.ByClass[class] / pf.TotalUSD * 100
	if !finite(w) {
		return s.miss()
	}
	s.note("weight("+class+")", w, pf.Trust)
	return w, nil
}

func (s *ExprScope) drawdownDay() (float64, error) {
	if !s.step() {
		return s.miss()
	}
	pf, ok := s.portfolio()
	if !ok || pf.DayDate == "" || !finite(pf.DayStartUSD) || pf.DayStartUSD <= 0 ||
		!pf.DayStartAt.Before(pf.At) || !s.v.Fresh(pf.DayStartAt, exprBaselineMaxAge) {
		return s.miss()
	}
	dd := (pf.DayStartUSD - pf.TotalUSD) / pf.DayStartUSD * 100
	if !finite(dd) {
		return s.miss()
	}
	s.note("drawdown_day()", dd, pf.Trust)
	return dd, nil
}

func (s *ExprScope) positionValue(asset string) (float64, error) {
	if !s.step() || !exprSymbolPattern.MatchString(asset) {
		return s.miss()
	}
	pf, ok := s.portfolio()
	if !ok {
		return s.miss()
	}
	var sum float64
	name := ""
	for _, p := range pf.Positions {
		if strings.EqualFold(p.Asset, asset) && finite(p.Value) {
			sum += p.Value
			name = p.Asset
		}
	}
	if name == "" {
		if pf.PositionsTruncated {
			return s.miss() // it may be in the part that was cut
		}
		s.trust = proactive.MinTrust(s.trust, pf.Trust)
		return 0, nil // not held; the name is the expression's, so it is not recorded
	}
	s.note("position_value("+name+")", sum, pf.Trust)
	return sum, nil
}

// plan reads one number of a delta-neutral plan's health when the report is fresh (three
// check intervals, five minutes at least, six hours at most), from an exchange-level
// source and complete (data_ok).
func (s *ExprScope) plan(id int, key, name string) (float64, error) {
	if !s.step() || id <= 0 {
		return s.miss()
	}
	pv, ok := s.v.Plan(ExprPlanKind, int64(id))
	if !ok || pv.Trust < proactive.TrustExchange || pv.Health[ExprPlanKeyDataOK] != 1 {
		return s.miss()
	}
	maxAge := min(max(3*time.Duration(pv.Health[ExprPlanKeyInterval])*time.Second, exprPlanMinAge), exprPlanMaxAge)
	x, has := pv.Health[key]
	if !has || !finite(x) || !s.v.Fresh(pv.At, maxAge) {
		return s.miss()
	}
	s.note(name+"("+strconv.Itoa(id)+")", x, pv.Trust)
	return x, nil
}

func (s *ExprScope) planFundingAPY(id int) (float64, error) {
	return s.plan(id, ExprPlanKeyAPY, "plan_funding_apy")
}

func (s *ExprScope) planHealth(id int) (float64, error) {
	return s.plan(id, ExprPlanKeyScore, "plan_health")
}

func (s *ExprScope) now() (float64, error) {
	if !s.step() {
		return s.miss()
	}
	return float64(s.v.Now().Unix()), nil
}

func (s *ExprScope) hourLocal() (int, error) {
	if !s.step() {
		_, err := s.miss()
		return 0, err
	}
	loc := time.UTC
	if s.location != nil {
		if l := s.location(); l != nil {
			loc = l
		}
	}
	return s.v.Now().In(loc).Hour(), nil
}

// ---- compile --------------------------------------------------------------------

// ExprProgram is a compiled expectation. It is safe for concurrent use.
type ExprProgram struct{ prog *vm.Program }

// exprSample is the environment compilation type-checks against.
var exprSample = ExprEnv(nil).Env()

// CompileExpr compiles src against the sample environment with expr.AsBool, so a
// typo, an unknown function, a non-boolean result or an expression over the bounds is
// refused before the expectation is stored. The error is a sentence for the owner, one
// line, without the library's echo of the source.
func CompileExpr(src string) (*ExprProgram, error) {
	src = strings.TrimSpace(src)
	switch {
	case src == "":
		return nil, errors.New("the expression is empty")
	case len(src) > MaxExprBytes:
		return nil, fmt.Errorf("the expression is %d bytes; the limit is %d", len(src), MaxExprBytes)
	}
	vet := &exprVet{}
	prog, err := expr.Compile(src, expr.Env(exprSample), expr.AsBool(), expr.DisableAllBuiltins(),
		expr.Optimize(false), expr.MaxNodes(maxExprNodes), expr.Patch(vet))
	if vet.err != nil {
		return nil, vet.err
	}
	if err != nil {
		msg, _, _ := strings.Cut(err.Error(), "\n")
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return nil, fmt.Errorf("the expression does not compile (it must be a true/false test using only %s): %s",
			strings.Join(exprFuncs, ", "), msg)
	}
	return &ExprProgram{prog: prog}, nil
}

// Eval runs the program against s. known is false when the result is unknown (a
// function's input was missing, or the bounds were hit); the value is then
// meaningless. err is a runtime failure that is not "unknown" (the caller counts it and
// treats the evaluation as unknown too).
func (p *ExprProgram) Eval(ctx context.Context, s *ExprScope) (value, known bool, err error) {
	s.ctx = ctx
	out, runErr := expr.Run(p.prog, s.Env())
	if s.unknown {
		return false, false, nil
	}
	if runErr != nil {
		return false, false, errors.New("the expression failed to evaluate")
	}
	b, ok := out.(bool)
	if !ok {
		return false, false, errors.New("the expression did not give true or false")
	}
	return b, true, nil
}

// exprVet is the whitelist: a visitor that records the first node the language does
// not allow.
type exprVet struct{ err error }

var exprBinaryOps = map[string]bool{
	"+": true, "-": true, "*": true, "/": true, "%": true,
	"<": true, ">": true, "<=": true, ">=": true, "==": true, "!=": true,
	"and": true, "&&": true, "or": true, "||": true,
}

func (x *exprVet) failf(format string, a ...any) {
	if x.err == nil {
		x.err = fmt.Errorf(format, a...)
	}
}

func (x *exprVet) Visit(n *ast.Node) {
	switch t := (*n).(type) {
	case *ast.NilNode, *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode, *ast.StringNode, *ast.ConditionalNode:
	case *ast.UnaryNode:
		if t.Operator != "-" && t.Operator != "+" && t.Operator != "not" && t.Operator != "!" {
			x.failf("the operator %q is not allowed", t.Operator)
		}
	case *ast.BinaryNode:
		if !exprBinaryOps[t.Operator] {
			x.failf("the operator %q is not allowed (use + - * / %% < > <= >= == != and or not)", t.Operator)
		}
	case *ast.IdentifierNode:
		if !slices.Contains(exprFuncs, t.Value) {
			x.failf("unknown function %q; the functions are %s", t.Value, strings.Join(exprFuncs, ", "))
		}
	case *ast.CallNode:
		x.vetCall(t)
	default:
		x.failf("that kind of expression is not allowed; use comparisons, arithmetic, and/or/not and the functions %s",
			strings.Join(exprFuncs, ", "))
	}
}

// vetCall requires the arguments of a function to be literals, so that what an
// expression reads is known without running it.
func (x *exprVet) vetCall(c *ast.CallNode) {
	id, ok := c.Callee.(*ast.IdentifierNode)
	if !ok {
		x.failf("only the functions %s can be called", strings.Join(exprFuncs, ", "))
		return
	}
	switch id.Value {
	case "price", "position_value", "weight":
		s, ok := firstArg(c).(*ast.StringNode)
		switch {
		case !ok || len(c.Arguments) != 1:
			x.failf("%s() takes one quoted name, for example %s", id.Value, exampleOf(id.Value))
		case id.Value == "weight" && !slices.Contains(exprClasses, s.Value):
			x.failf("weight() knows the classes %s, not %q", strings.Join(exprClasses, ", "), s.Value)
		case !exprSymbolPattern.MatchString(s.Value):
			x.failf("%s() was given a name that is not a symbol or asset", id.Value)
		}
	case "plan_funding_apy", "plan_health":
		if n, ok := firstArg(c).(*ast.IntegerNode); !ok || len(c.Arguments) != 1 || n.Value <= 0 {
			x.failf("%s() takes one plan number, for example %s", id.Value, exampleOf(id.Value))
		}
	default:
		if len(c.Arguments) != 0 {
			x.failf("%s() takes no arguments", id.Value)
		}
	}
}

func firstArg(c *ast.CallNode) ast.Node {
	if len(c.Arguments) == 0 {
		return nil
	}
	return c.Arguments[0]
}

func exampleOf(fn string) string {
	switch fn {
	case "price":
		return `price("BTC/USDT")`
	case "weight":
		return `weight("crypto")`
	case "position_value":
		return `position_value("SOL")`
	}
	return fn + "(12)"
}
