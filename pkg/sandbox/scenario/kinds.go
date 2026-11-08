package scenario

import (
	"regexp"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// stepSpec says what a step kind is served by. Group and Op name the lab op that runs it
// ("" op: the group has one op), PR the plan that adds it, Req the arguments it cannot
// run without. Steps with no Group are the runner's own (config.patch, restart).
type stepSpec struct {
	Group, Op, PR string
	Req           []string
}

var stepKinds = map[string]stepSpec{
	"chat":             {"chat", "send", "SBX-02", []string{"as", "text"}},
	"chat.press":       {"chat", "press", "SBX-02", []string{"action"}},
	"chat.react":       {"chat", "react", "SBX-02", []string{"emoji"}},
	"chat.channel":     {"chat", "set_state", "SBX-02", []string{"state"}},
	"call_tool":        {"call_tool", "", "SBX-06", []string{"name"}},
	"manage":           {"manage", "", "SBX-06", []string{"route"}},
	"market.set":       {"market", "set", "SBX-03", []string{"symbol"}},
	"market.path":      {"market", "path", "SBX-03", []string{"symbol", "to", "over"}},
	"market.shock":     {"market", "shock", "SBX-03", []string{"symbol", "pct"}},
	"market.walk":      {"market", "walk", "SBX-03", []string{"symbol", "vol", "seed"}},
	"market.replay":    {"market", "replay", "SBX-03", []string{"symbol", "timeframe"}},
	"market.delist":    {"market", "delist", "SBX-03", []string{"symbol"}},
	"market.relist":    {"market", "relist", "SBX-03", []string{"symbol"}},
	"market.recording": {"market", "recording", "SBX-21", []string{"symbol"}},
	"llm.script":       {"llm", "script_put", "SBX-04", []string{"rules"}},
	"fault.add":        {"fault", "add", "SBX-13", nil},
	"fault.clear":      {"fault", "clear", "SBX-13", nil},
	"web.put":          {"web", "put", "SBX-15", nil},
	"config.patch":     {"inspect", "wait_for", "SBX-05", nil}, // waits for config.reload
	"restart":          {"", "", "SBX-10", nil},
	"advance":          {"clock", "advance", "SBX-07", nil},
	"run_until":        {"clock", "advance", "SBX-07", nil},
	"wait_for":         {"inspect", "wait_for", "SBX-05", []string{"match"}},
	"settle":           {"inspect", "settle", "SBX-09", nil},
}

// blockSpec is the group a top-level block is applied through.
var blockSpec = map[string]stepSpec{
	"profile":    {"call_tool", "", "SBX-06", nil},
	"market":     {"market", "", "SBX-03", nil},
	"account":    {"account", "seed", "SBX-06", nil},
	"faults":     {"fault", "add", "SBX-13", nil},
	"web":        {"web", "put", "SBX-15", nil},
	"llm_script": {"llm", "script_put", "SBX-04", nil},
}

// kindOwner names the plan that adds a journal kind, first prefix match.
var kindOwner = []struct{ prefix, pr string }{
	{"chat.", "SBX-02"}, {"exchange.req", "SBX-03"}, {"exchange.order", "SBX-14a"}, {"exchange.funding", "SBX-14b"}, {"llm.budget", "SBX-16"},
	{"llm.", "SBX-04"}, {"event.", "SBX-05"}, {"tool.", "SBX-05"}, {"workflow.", "SBX-05"}, {"proposal", "SBX-05"},
	{"policy.", "SBX-05"}, {"manage.", "SBX-06"}, {"clock.", "SBX-07"}, {"settle", "SBX-09"}, {"config.", "SBX-11"},
	{"web.", "SBX-15"}, {"fault.", "SBX-13"}, {"crash", "SBX-20"},
}

func ownerOf(kind string) string {
	for _, o := range kindOwner {
		if strings.HasPrefix(kind, o.prefix) {
			return o.pr
		}
	}
	return "a later SBX PR"
}

// expectSpec lists the fields a kind accepts (besides the modifiers) and the journal
// kind it reads.
var expectSpec = map[string]struct {
	Fields  []string
	Journal string
}{
	"message":  {[]string{"to", "contains", "severity", "silent", "buttons"}, "chat.out"},
	"event":    {[]string{"kind", "fields", "subject", "trigger", "severity", "disposition", "reason"}, ""},
	"decision": {[]string{"trigger", "severity", "disposition", "reason"}, "event.attention.decision"},
	"order":    {[]string{"venue", "symbol", "side", "reduce_only", "client_id", "status"}, ""},
	"workflow": {[]string{"kind", "state"}, ""}, // read from inspect workflows, which lists no steps
	"proposal": {[]string{"kind", "status"}, "proposal"},
	"policy":   {[]string{"verdict", "gate"}, "policy.decision"},
	"probe":    {[]string{"mandate_status", "dn_plan_status", "alert_job_exists", "equals"}, ""},
	"llm":      {[]string{"turn"}, "llm.call"},
	"web":      {[]string{"host"}, "web.req"},
	"logs":     {[]string{"level", "allow"}, ""},
}

// The values the journal holds where the set is closed, for Validate's value checks.
var (
	// orderStatuses are the statuses of an exchange.order entry: journalStates in
	// pkg/sandbox/orderbook.go (unexported), never a venue's own word such as closed.
	orderStatuses = []string{"open", "partially_filled", "filled", "canceled", "expired"}
	// orderSides are an order's sides; a position's long and short are not journaled.
	orderSides = []string{"buy", "sell"}
	// unifiedSymbolRE is an exchange.order symbol: BTC/USDT for spot, BTC/USDT:USDT for a
	// swap, PTT/THB or BRK.B/USD for an equity (a ticker may hold a dot), never the venue's
	// wire form (BTCUSDT, BTC-USDT-SWAP).
	unifiedSymbolRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.]*/[A-Z0-9]+(:[A-Z0-9]+)?$`)
	// equityVenues are the venues whose wire symbol is a bare ticker (orderbook.go
	// equityQuote). A ticker holding "-", "_" or a space is not split, so the order is
	// journaled under that wire ticker as it is; equityWireRE accepts those, for these venues only.
	equityVenues = []string{"settrade", "webull"}
	equityWireRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.]*[-_ ][A-Z0-9._ -]*$`)
	// workflowStates are the statuses inspect workflows reports, which state is compared with whole.
	workflowStates = []string{
		string(journal.StatusPending), string(journal.StatusRunning), string(journal.StatusWaiting),
		string(journal.StatusSucceeded), string(journal.StatusFailed), string(journal.StatusCancelled),
	}
	// dispositions are attention's decisions (event.payload.Disposition).
	dispositions = []string{
		string(proactive.DispDrop), string(proactive.DispLog), string(proactive.DispDigest),
		string(proactive.DispNotify), string(proactive.DispEscalate),
	}
)

// probeGroup is the lab group a probe is read through.
var probeGroup = map[string]struct{ group, pr string }{
	"mandate_status": {"manage", "SBX-06"},
	"dn_plan_status": {"call_tool", "SBX-06"}, "alert_job_exists": {"call_tool", "SBX-06"},
}
