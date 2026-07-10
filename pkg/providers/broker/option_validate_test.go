package broker

import "testing"

func fptr(f float64) *float64 { return &f }

func validLeg() OptionLeg {
	return OptionLeg{Side: "buy", Quantity: 1, Underlying: "AAPL", Strike: 320, Expiry: "2026-08-21", OptionType: "CALL"}
}

func TestOptionOrderRequestValidate(t *testing.T) {
	base := func() OptionOrderRequest {
		return OptionOrderRequest{
			Underlying: "AAPL", Strategy: "SINGLE", OrderType: "LIMIT", Side: "BUY",
			Quantity: 1, LimitPrice: fptr(1.0), TimeInForce: "DAY", Legs: []OptionLeg{validLeg()},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*OptionOrderRequest)
		wantErr bool
	}{
		{"valid limit day buy", func(*OptionOrderRequest) {}, false},
		{"empty strategy defaults to single", func(r *OptionOrderRequest) { r.Strategy = "" }, false},
		{"multi-leg strategy rejected", func(r *OptionOrderRequest) { r.Strategy = "VERTICAL" }, true},
		{"limit without price", func(r *OptionOrderRequest) { r.LimitPrice = nil }, true},
		{"stop_loss without stop", func(r *OptionOrderRequest) { r.OrderType = "STOP_LOSS"; r.LimitPrice = nil }, true},
		{"stop_loss with stop", func(r *OptionOrderRequest) { r.OrderType = "STOP_LOSS"; r.LimitPrice = nil; r.StopPrice = fptr(2.0) }, false},
		{"stop_loss_limit needs both", func(r *OptionOrderRequest) { r.OrderType = "STOP_LOSS_LIMIT"; r.StopPrice = nil }, true},
		{"market rejected", func(r *OptionOrderRequest) { r.OrderType = "MARKET" }, true},
		{"take_profit rejected", func(r *OptionOrderRequest) { r.OrderType = "TAKE_PROFIT" }, true},
		{"unknown order type", func(r *OptionOrderRequest) { r.OrderType = "ICEBERG" }, true},
		{"bad side", func(r *OptionOrderRequest) { r.Side = "SHORT" }, true},
		{"bad tif", func(r *OptionOrderRequest) { r.TimeInForce = "IOC" }, true},
		{"gtc on sell rejected", func(r *OptionOrderRequest) { r.Side = "SELL"; r.TimeInForce = "GTC"; r.Legs[0].Side = "sell" }, true},
		{"gtc on buy ok", func(r *OptionOrderRequest) { r.TimeInForce = "GTC" }, false},
		{"zero legs", func(r *OptionOrderRequest) { r.Legs = nil }, true},
		{"two legs", func(r *OptionOrderRequest) { r.Legs = []OptionLeg{validLeg(), validLeg()} }, true},
		{"bad leg option type", func(r *OptionOrderRequest) { r.Legs[0].OptionType = "WARRANT" }, true},
		{"bad leg side", func(r *OptionOrderRequest) { r.Legs[0].Side = "hold" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(&r)
			err := r.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
