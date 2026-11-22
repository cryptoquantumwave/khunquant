package tools

import (
	"errors"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// orderListProvider returns the provider the order-listing tools (open orders,
// order history, trade history) read from. In paper mode that is the paper order
// book (broker.SimulationProvider), so the orders the owner placed in paper mode
// are the ones listed; otherwise it is the real provider. paper reports which.
func orderListProvider(cfg *config.Config, providerID, account string) (tp broker.TradingProvider, paper bool, fail *ToolResult) {
	if broker.IsPaperMode(cfg) {
		tp, _, err := broker.TradingProviderFor(cfg, providerID, account)
		if err != nil {
			return nil, true, orderProviderFailure(err, providerID)
		}
		return tp, true, nil
	}
	return realTradingProvider(cfg, providerID, account)
}

// orderByIDProvider returns the provider to send one order ID to (fetch, cancel).
//
//   - A paper- ID with paper mode off is refused: it was issued by the simulation
//     and must never reach a real exchange.
//   - A paper- ID with paper mode on is answered by the paper order book.
//   - Any other ID goes to the real provider, in paper mode too: it names a real
//     order (exits stay real), and the paper book never answers for it.
func orderByIDProvider(cfg *config.Config, providerID, account, orderID string) (tp broker.TradingProvider, paper bool, fail *ToolResult) {
	if err := broker.RefusePaperIDWhileLive(cfg, orderID); err != nil {
		return nil, false, ErrorResult(fmt.Sprintf("order %s: %v", orderID, err))
	}
	if broker.IsPaperMode(cfg) && broker.IsPaperOrderID(orderID) {
		tp, _, err := broker.TradingProviderFor(cfg, providerID, account)
		if err != nil {
			return nil, true, orderProviderFailure(err, providerID)
		}
		return tp, true, nil
	}
	return realTradingProvider(cfg, providerID, account)
}

func realTradingProvider(cfg *config.Config, providerID, account string) (broker.TradingProvider, bool, *ToolResult) {
	p, err := broker.CreateProviderForAccount(providerID, account, cfg)
	if err != nil {
		return nil, false, ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}
	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return nil, false, ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}
	return tp, false, nil
}

// listPaperAndReal runs a listing against the right provider(s). Outside paper mode
// that is the real provider alone. In paper mode it is the paper order book first
// (rows marked [PAPER]), then the real exchange as a second section, because real
// orders placed before paper mode was switched on still exist (exits stay real) and
// must not seem to vanish. A failed real listing is reported under its heading and
// never hides the paper section; a failed paper listing is the result.
func listPaperAndReal(cfg *config.Config, providerID, account string, run func(tp broker.TradingProvider, paper bool) *ToolResult) *ToolResult {
	if !broker.IsPaperMode(cfg) {
		tp, _, fail := realTradingProvider(cfg, providerID, account)
		if fail != nil {
			return fail
		}
		return run(tp, false)
	}
	ptp, _, fail := orderListProvider(cfg, providerID, account)
	if fail != nil {
		return fail
	}
	paperRes := run(ptp, true)
	if paperRes.IsError {
		return paperRes
	}
	realText := ""
	if rtp, _, rfail := realTradingProvider(cfg, providerID, account); rfail != nil {
		realText = "(the real exchange could not be listed: " + rfail.ForLLM + ")"
	} else if rr := run(rtp, false); rr.IsError {
		realText = "(the real exchange could not be listed: " + rr.ForLLM + ")"
	} else {
		realText = rr.ForLLM
	}
	return UserResult(paperRes.ForLLM + "\n\nReal exchange orders (not simulated):\n" + realText)
}

func orderProviderFailure(err error, providerID string) *ToolResult {
	if errors.Is(err, broker.ErrOrderExecutionUnsupported) {
		return ErrorResult(fmt.Sprintf("provider %q does not support order management", providerID))
	}
	return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
}
