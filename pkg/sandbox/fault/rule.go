// Package fault is the Sandbox Lab's fault injector: rules that break the simulated
// exchanges, and the simulated model, the way production breaks them. A rule is a match,
// one effect and a window (when). It exists only in a locked process.
package fault

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// Limits on what a rule may carry. The fault group takes model- and scenario-supplied JSON.
const (
	MaxRules    = 256
	maxBody     = 64 << 10
	maxTimes    = 1_000_000
	maxPathLen  = 512
	venueLLM    = "llm"
	venueAny    = "*"
	defaultHold = 60 * time.Second
)

// Duration is a time.Duration written as a string ("200ms") in JSON.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`a duration is a string such as "200ms"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Rule is one fault. Exactly one effect is set; the first active matching rule fires.
type Rule struct {
	ID     string `json:"id,omitempty"` // assigned by Add when empty
	Match  Match  `json:"match"`
	Effect Effect `json:"effect"`
	When   When   `json:"when,omitempty"`
}

// Match selects requests. An unset field matches anything; Venue is required.
type Match struct {
	Venue      string `json:"venue"` // binance|binanceth|okx|bitkub|settrade|webull|web|"*", or "llm"
	Method     string `json:"method,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"` // the original path, as the router sees it; matched as data
	Order      string `json:"order,omitempty"`       // create|cancel|fetch|list (see ClassifyOrder)
	Market     string `json:"market,omitempty"`      // spot|futures
	Symbol     string `json:"symbol,omitempty"`      // "BTC/USDT:USDT" or the venue's wire form
	Side       string `json:"side,omitempty"`
	ReduceOnly *bool  `json:"reduce_only,omitempty"`
	Turn       string `json:"turn,omitempty"`   // llm only: a turn kind, or "complete"
	Caller     string `json:"caller,omitempty"` // llm only: news|seahorse|complete
}

// Effect is what a rule does. Set exactly one of: status/body, error, timeout, latency,
// drop_connection, order, llm_latency, llm_error.
type Effect struct {
	Status         int             `json:"status,omitempty"`
	Body           json.RawMessage `json:"body,omitempty"`
	Error          string          `json:"error,omitempty"` // a library name such as "rate_limit"
	Timeout        bool            `json:"timeout,omitempty"`
	Latency        Duration        `json:"latency,omitempty"`
	DropConnection bool            `json:"drop_connection,omitempty"`
	Order          string          `json:"order,omitempty"` // reject|written_then_timeout|partial|accept_no_fill|cancel_after_accept|create_reports_fill|lookup_lag
	Code           string          `json:"code,omitempty"`  // reject: a library name or the venue's raw code
	Fraction       float64         `json:"fraction,omitempty"`
	After          Duration        `json:"after,omitempty"`
	Lag            Duration        `json:"lag,omitempty"`
	Reported       *float64        `json:"reported,omitempty"`
	LLMLatency     Duration        `json:"llm_latency,omitempty"`
	LLMError       string          `json:"llm_error,omitempty"` // timeout|rate_limit|server|bad_request
}

// When limits a rule in sim time and in count. From and To are "+10m" (from the moment
// of Add) or RFC 3339; Probability is drawn from a generator seeded by Seed.
type When struct {
	From        string  `json:"from,omitempty"`
	To          string  `json:"to,omitempty"`
	Times       int     `json:"times,omitempty"`       // 0 = unlimited
	Probability float64 `json:"probability,omitempty"` // 0 = always
	Seed        uint64  `json:"seed,omitempty"`
}

// layer says where a rule runs.
type layer int

const (
	layerResponse layer = iota + 1 // StageFault: a venue-shaped answer
	layerWrap                      // the handler wrap: hold, delay, drop
	layerOrder                     // the simulator, through OrderEffect (SBX-14a)
	layerLLM                       // sim.Faults.BeforeLLM
)

var kindLayer = map[string]layer{
	"status": layerResponse, "body": layerResponse, "error": layerResponse, "reject": layerResponse,
	"latency": layerWrap, "timeout": layerWrap, "drop_connection": layerWrap, "written_then_timeout": layerWrap,
	"partial": layerOrder, "accept_no_fill": layerOrder, "cancel_after_accept": layerOrder,
	"create_reports_fill": layerOrder, "lookup_lag": layerOrder,
	"llm_latency": layerLLM, "llm_error": layerLLM,
}

var (
	idRE      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	venueRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	methodRE  = regexp.MustCompile(`^[A-Za-z]{3,10}$`)
	turnRE    = regexp.MustCompile(`^[a-z_]{1,40}$`)
	llmErrors = []string{"timeout", "rate_limit", "server", "bad_request"}
)

// kind names the one effect a rule has (the "effect" of its fault.fired entry).
func (e Effect) kind() (string, error) {
	var set []string
	add := func(on bool, name string) {
		if on {
			set = append(set, name)
		}
	}
	add(e.Status != 0 || len(e.Body) > 0, "status/body")
	add(e.Error != "", "error")
	add(e.Timeout, "timeout")
	add(e.Latency != 0, "latency")
	add(e.DropConnection, "drop_connection")
	add(e.Order != "", "order")
	add(e.LLMLatency != 0, "llm_latency")
	add(e.LLMError != "", "llm_error")
	if len(set) != 1 {
		return "", fmt.Errorf("an effect sets exactly one of status/body, error, timeout, latency, drop_connection, order, llm_latency, llm_error (got %d: %s)", len(set), strings.Join(set, ", "))
	}
	switch set[0] {
	case "status/body":
		if e.Status != 0 {
			return "status", nil
		}
		return "body", nil
	case "order":
		return e.Order, nil
	}
	return set[0], nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// validate checks a rule before it is stored and returns its kind and symbol.
func (i *Injector) validate(r Rule) (kind string, pair *market.Pair, err error) {
	if r.ID != "" && !idRE.MatchString(r.ID) {
		return "", nil, errors.New("id must match [A-Za-z0-9_.-]{1,64}")
	}
	if kind, err = r.Effect.kind(); err != nil {
		return "", nil, err
	}
	ly, ok := kindLayer[kind]
	if !ok {
		return "", nil, fmt.Errorf("unknown order effect %q", kind)
	}
	if err := i.validateMatch(r.Match, ly == layerLLM, kind); err != nil {
		return "", nil, err
	}
	if r.Match.Symbol != "" {
		p, perr := market.ParseSymbol(r.Match.Venue, r.Match.Symbol)
		if perr != nil {
			return "", nil, perr
		}
		pair = &p
	}
	if err := i.validateEffect(r, kind, ly); err != nil {
		return "", nil, err
	}
	w := r.When
	switch {
	case w.Times < 0 || w.Times > maxTimes:
		return "", nil, fmt.Errorf("times must be 0..%d", maxTimes)
	case !finite(w.Probability) || w.Probability < 0 || w.Probability > 1:
		return "", nil, errors.New("probability must be a number in [0, 1]")
	}
	return kind, pair, nil
}

func (i *Injector) validateMatch(m Match, llm bool, kind string) error {
	if m.Venue == "" {
		return errors.New(`match.venue is required ("*" matches any venue)`)
	}
	if llm != (m.Venue == venueLLM) {
		return errors.New(`llm_latency and llm_error need match.venue "llm", and an exchange effect must not use it`)
	}
	if m.Venue != venueAny && !venueRE.MatchString(m.Venue) {
		return fmt.Errorf("match.venue %q is not a venue name", m.Venue)
	}
	if !llm && m.Venue != venueAny && !servedVenue(m.Venue) {
		return fmt.Errorf("match.venue %q is not a venue the sandbox serves: the rule would never fire", m.Venue)
	}
	if llm {
		if m.Method != "" || m.PathPrefix != "" || m.Order != "" || m.Market != "" || m.Symbol != "" || m.Side != "" || m.ReduceOnly != nil {
			return errors.New("an llm rule matches on turn and caller only")
		}
		if (m.Turn != "" && !turnRE.MatchString(m.Turn)) || (m.Caller != "" && !oneOf(m.Caller, "news", "seahorse", "complete")) {
			return errors.New("match.turn is a turn kind such as proactive or complete; match.caller is news|seahorse|complete")
		}
		return nil
	}
	switch {
	case m.Turn != "" || m.Caller != "":
		return errors.New("match.turn and match.caller are for llm rules")
	case m.Method != "" && !methodRE.MatchString(m.Method):
		return fmt.Errorf("match.method %q is not an HTTP method", m.Method)
	case m.PathPrefix != "" && (m.PathPrefix[0] != '/' || len(m.PathPrefix) > maxPathLen):
		return fmt.Errorf("match.path_prefix must start with / and be at most %d bytes", maxPathLen)
	case !oneOf(m.Order, "", "create", "cancel", "fetch", "list"):
		return errors.New("match.order is create|cancel|fetch|list")
	case !oneOf(m.Market, "", "spot", "futures"):
		return errors.New("match.market is spot|futures")
	case !oneOf(strings.ToLower(m.Side), "", "buy", "sell"):
		return errors.New("match.side is buy|sell")
	case (kind == "written_then_timeout" || kindLayer[kind] == layerOrder) && !oneOf(m.Order, "", "create"):
		// The simulator asks about an order once, when it is created: a rule for another
		// operation would never fire.
		return fmt.Errorf("%s applies to create only", kind)
	}
	// What only an order request carries is seen only where requests are classified as
	// orders, and order-state effects only where the simulator keeps an order ledger.
	// Anywhere else the rule would be stored and never fire.
	orderMatch := m.Order != "" || m.Market != "" || m.Symbol != "" || m.Side != "" || m.ReduceOnly != nil
	switch {
	case m.Venue == venueAny:
	case (orderMatch || kind == "reject" || kind == "written_then_timeout") && !classifiesOrders(m.Venue):
		return fmt.Errorf("match.venue %q has no order classifier: order, market, symbol, side, reduce_only, reject and written_then_timeout never match there", m.Venue)
	case kindLayer[kind] == layerOrder && !slices.Contains(sandbox.AccountVenues, m.Venue):
		return fmt.Errorf("%s needs a venue whose simulator keeps orders (%s)", kind, strings.Join(sandbox.AccountVenues, ", "))
	case m.Symbol != "" && m.Venue == "binanceth":
		return errors.New("match.symbol is not supported on binanceth: its wire symbols are not parsed")
	}
	return nil
}

func (i *Injector) validateEffect(r Rule, kind string, ly layer) error {
	e := r.Effect
	if (e.Fraction != 0 && kind != "partial") || (e.After != 0 && kind != "cancel_after_accept") ||
		(e.Lag != 0 && kind != "lookup_lag") || (e.Reported != nil && kind != "create_reports_fill") ||
		(e.Code != "" && kind != "reject") || ((e.Status != 0 || len(e.Body) > 0) && kind != "status" && kind != "body") {
		return fmt.Errorf("effect field does not belong to %s", kind)
	}
	hold := func(d Duration, name string) error {
		if d < 0 || time.Duration(d) > i.maxHold {
			return fmt.Errorf("%s must be between 0 and %s (the cap on every hold)", name, i.maxHold)
		}
		return nil
	}
	switch kind {
	case "status", "body":
		if e.Status != 0 && (e.Status < 100 || e.Status > 599) {
			return fmt.Errorf("status %d is outside 100..599", e.Status)
		}
		if len(e.Body) > maxBody || (len(e.Body) > 0 && !json.Valid(e.Body)) {
			return fmt.Errorf("body must be JSON of at most %d bytes", maxBody)
		}
	case "error", "reject":
		name := e.Error
		if kind == "reject" {
			name = e.Code
		}
		if name == "" {
			return fmt.Errorf("%s needs a library name or code", kind)
		}
		if !libraryHas(r.Match.Venue, name, kind == "reject") {
			return fmt.Errorf("venue %q has no error %q in the library (concrete venue and known name required)", r.Match.Venue, name)
		}
	case "latency":
		if e.Latency <= 0 {
			return errors.New("latency must be positive")
		}
		return hold(e.Latency, "latency")
	case "llm_latency":
		if e.LLMLatency <= 0 {
			return errors.New("llm_latency must be positive")
		}
		return hold(e.LLMLatency, "llm_latency")
	case "llm_error":
		if !oneOf(e.LLMError, llmErrors...) {
			return fmt.Errorf("llm_error is one of %s", strings.Join(llmErrors, ", "))
		}
	case "partial":
		if !finite(e.Fraction) || e.Fraction <= 0 || e.Fraction >= 1 {
			return errors.New("fraction must be in (0, 1)")
		}
	case "cancel_after_accept":
		if e.After < 0 {
			return errors.New("after must not be negative")
		}
	case "lookup_lag":
		if e.Lag <= 0 {
			return errors.New("lag must be positive")
		}
	case "create_reports_fill":
		if e.Reported != nil && (!finite(*e.Reported) || *e.Reported < 0) {
			return errors.New("reported must be a finite number, 0 or more")
		}
		// OKX's create answer carries no fill and no state, so there is nothing to under-report
		// there; a rule for every venue would match it.
		if r.Match.Venue == "okx" || r.Match.Venue == venueAny {
			return errors.New("create_reports_fill needs a venue whose create answer reports a fill (binance); okx's carries none")
		}
	}
	if ly == layerOrder {
		if !i.orderKindEnabled(kind) {
			return fmt.Errorf("order effect %q is not enabled in this build (SBX-14a executes it)", kind)
		}
		if r.Match.Method != "" || r.Match.PathPrefix != "" {
			return errors.New("an order-state rule matches the order, not the HTTP request: no method or path_prefix")
		}
	}
	return nil
}

func oneOf(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

// window resolves From and To against now: "+10m" is an offset, otherwise RFC 3339.
func (w When) window(now time.Time) (from, to time.Time, err error) {
	parse := func(s string) (time.Time, error) {
		if s == "" {
			return time.Time{}, nil
		}
		if strings.HasPrefix(s, "+") {
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 {
				return time.Time{}, fmt.Errorf("%q is not an offset such as +10m", s)
			}
			return now.Add(d), nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is neither +<duration> nor RFC 3339", s)
		}
		return t, nil
	}
	if from, err = parse(w.From); err != nil {
		return
	}
	if to, err = parse(w.To); err != nil {
		return
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		err = errors.New("when.from is after when.to")
	}
	return
}
