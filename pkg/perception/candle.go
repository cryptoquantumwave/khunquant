package perception

import "time"

// CandleClose is one candle_closed event as a consumer that cannot import the market
// source reads it (a trigger judging an indicator on a closed bar).
type CandleClose struct {
	Provider, Symbol, Timeframe string
	// BarOpen is the opening time of the newest closed bar.
	BarOpen time.Time
	// Closes are the closes of the closed bars, oldest first; the last is the bar that
	// just closed. The forming bar is never among them. Read-only.
	Closes []float64
	// At is when the bars were fetched.
	At time.Time
}

// CandlePayload is implemented by the payload of a perception.market.candle_closed
// event. The world model does not track candles, so such an event reaches the
// triggers as an empty Diff whose Payload is this; a trigger type-asserts it and
// treats anything else as not its own.
type CandlePayload interface {
	CandleClose() CandleClose
}
