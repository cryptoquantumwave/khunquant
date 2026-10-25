package perception

import "time"

// DCAPlan is one DCA plan as the DCA source and the due trigger see it: plain values,
// read from the plan store by the gateway, so that neither of them can reach the
// store, the broker or the order path. It deliberately has no name or note field: a
// plan's name is text a person typed, and nothing downstream of the source needs it.
type DCAPlan struct {
	ID       int64
	Provider string
	Account  string
	Symbol   string
	// Side is "buy" or "sell"; AmountUnit "quote" or "base".
	Side       string
	AmountUnit string
	Amount     float64
	// Schedule is the plan's cron expression and Timezone the zone it is read in, as
	// the plan's cron job has them.
	Schedule string
	Timezone string
	// CreatedAt is when the plan was made.
	CreatedAt time.Time
	// Enabled and EndDate (zero: none) are the plan's, as read now.
	Enabled bool
	EndDate time.Time
	// ExecPeriod ("hour", "day", "week" or "") and MaxExecPerPeriod are the plan's
	// cap. PeriodCount is the number of executions that count against it in the
	// current period, set only by a fresh read of one plan (DCAPlanReader.Plan) and
	// only when PeriodCountKnown.
	ExecPeriod       string
	MaxExecPerPeriod int
	PeriodCount      int
	PeriodCountKnown bool
	// Gate is the plan's indicator gate, nil for an unconditional plan.
	Gate *DCAGate
}

// DCAGate is a plan's indicator condition: bars of Timeframe (Lookback of them, as
// the legacy gate asks for), the indicators computed over them and a boolean
// expression over those and the bar variables.
type DCAGate struct {
	Timeframe  string         `json:"timeframe"`
	Lookback   int            `json:"lookback,omitempty"`
	Expression string         `json:"expression"`
	Indicators []DCAIndicator `json:"indicators,omitempty"`
	// Unsupported says the gate could not be copied faithfully (a parameter that is
	// not a number): it cannot be judged, and is reported as such.
	Unsupported bool `json:"unsupported,omitempty"`
}

// DCAIndicator is one named indicator of a gate.
type DCAIndicator struct {
	Alias  string             `json:"alias"`
	Kind   string             `json:"kind"`
	Params map[string]float64 `json:"params,omitempty"`
}

// The names of the numbers a perception.plan.dca_due event carries in the Health of
// its plan payload (worldmodel.PlanPayload). The world model keeps them in the plan's
// view, which is how a trigger of a tracked kind reads them: the model does not
// carry a payload on the Diff of the kinds it folds in.
const (
	// DCAHealthDueMS is the slot the event is for, in unix milliseconds. It is
	// exactly representable: the value stays below 2^53.
	DCAHealthDueMS = "due_ms"
	// DCAHealthLateMS is how long after its slot the event was made, 0 when it was
	// made on time (within a minute).
	DCAHealthLateMS = "late_ms"
	// DCAHealthEnabled is 1 when the plan was enabled when the event was made.
	DCAHealthEnabled = "enabled"
	// DCAHealthEndMS is the plan's end date in unix milliseconds, 0 for none.
	DCAHealthEndMS = "end_ms"
	// DCAHealthPeriodCap is the plan's cap per period, 0 for none.
	DCAHealthPeriodCap = "period_cap"
	// DCAHealthPeriodCount is the number of executions counted against the cap in
	// the current period, -1 when it could not be read.
	DCAHealthPeriodCount = "period_count"
)

// DCAPlanKind is the plan family name of a DCA plan in the world model.
const DCAPlanKind = "dca"
