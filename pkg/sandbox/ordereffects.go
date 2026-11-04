package sandbox

import "time"

// The order-level contract between the fault injector (which matches rules) and the
// simulator (which executes what a firing says). It lives here, next to the simulator,
// because pkg/sandbox/fault imports this package for Middleware: the reverse would cycle.
// SBX-13 matches and counts; SBX-14a executes.

// OrderOp is what a request does to an order.
type OrderOp string

const (
	OrderCreate OrderOp = "create"
	OrderCancel OrderOp = "cancel"
	OrderFetch  OrderOp = "fetch"
	OrderList   OrderOp = "list"
)

// OrderInfo describes the order a request is about, as the venue wrote it: Symbol is the
// venue's wire symbol (BTCUSDT, BTC-USDT-SWAP), Side is lower case.
type OrderInfo struct {
	Venue, Market, Symbol, Side, Type, ClientID string
	ReduceOnly                                  bool
	Amount                                      float64
}

// OrderEffect is one firing of an order-state rule.
type OrderEffect struct {
	RuleID   string
	Kind     string // partial|accept_no_fill|cancel_after_accept|create_reports_fill|lookup_lag
	Fraction float64
	After    time.Duration
	Lag      time.Duration
	Reported float64
}

// OrderEffects is what the simulator asks before it handles an order operation.
type OrderEffects interface {
	// OrderEffect consumes one firing of the first active matching order-state rule and
	// journals it. ok is false when no rule fires.
	OrderEffect(op OrderOp, o OrderInfo) (OrderEffect, bool)
}
