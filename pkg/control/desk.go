package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// BrokerDesk is the OrderDesk over the real brokers. It reads an order and withdraws
// it through the provider kind the order was placed with:
//
//   - a spot order placed in paper mode lives in the simulation only: while paper mode
//     is still on it is looked at and cancelled through broker.TradingProviderFor (the
//     simulation, never an exchange) and reported as simulated; if paper mode has
//     been switched off since, there is nothing real to touch;
//   - a spot order placed live is cancelled through TradingProviderFor, and if paper
//     mode has been switched on since, through the real provider, because that is
//     where the order is (withdrawing it only reduces exposure);
//   - a futures order goes through the futures provider, which has no simulation.
//
// Lookup never places an order: a cut place step is searched for with
// broker.FindOrder, which returns where PlaceOrderIdempotent would place again.
type BrokerDesk struct {
	Cfg *config.Config
	// Spot and Futures resolve providers; nil uses the broker package. Tests set them.
	Spot    func(cfg *config.Config, provider, account string) (broker.TradingProvider, error)
	Futures func(cfg *config.Config, provider, account string) (broker.FuturesProvider, error)
}

func (d BrokerDesk) spot(ref broker.OrderRef) (tp broker.TradingProvider, simulated bool, err error) {
	paperNow := broker.IsPaperMode(d.Cfg)
	if ref.Paper && !paperNow {
		return nil, true, nil // a simulated order and no simulation: nothing real exists
	}
	if d.Spot != nil {
		tp, err = d.Spot(d.Cfg, ref.Provider, ref.Account)
		return tp, ref.Paper, err
	}
	if !ref.Paper && paperNow {
		p, err := broker.CreateProviderForAccount(ref.Provider, ref.Account, d.Cfg)
		if err != nil {
			return nil, false, err
		}
		tp, ok := p.(broker.TradingProvider)
		if !ok {
			return nil, false, fmt.Errorf("%w: %q", broker.ErrOrderExecutionUnsupported, ref.Provider)
		}
		return tp, false, nil
	}
	tp, _, err = broker.TradingProviderFor(d.Cfg, ref.Provider, ref.Account)
	return tp, ref.Paper, err
}

func (d BrokerDesk) futures(ref broker.OrderRef) (broker.FuturesProvider, error) {
	if d.Futures != nil {
		return d.Futures(d.Cfg, ref.Provider, ref.Account)
	}
	p, err := broker.CreateProviderForAccount(ref.Provider, ref.Account, d.Cfg)
	if err != nil {
		return nil, err
	}
	fp, ok := p.(broker.FuturesProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q does not trade futures", ref.Provider)
	}
	return fp, nil
}

// Lookup implements OrderDesk.
func (d BrokerDesk) Lookup(ctx context.Context, ref broker.OrderRef) (OrderState, error) {
	var (
		o        ccxt.Order
		err      error
		provider broker.Provider
	)
	if ref.Market == broker.OrderMarketFutures {
		fp, ferr := d.futures(ref)
		if ferr != nil {
			return OrderState{}, ferr
		}
		provider = fp
		if ref.OrderID != "" {
			o, err = fp.FetchFuturesOrder(ctx, ref.OrderID, ref.Symbol)
		}
	} else {
		tp, simulated, terr := d.spot(ref)
		if terr != nil {
			return OrderState{}, terr
		}
		if simulated {
			// The simulation answers "open" for any id, which proves nothing; the
			// order is as open as its record says.
			return OrderState{ID: ref.OrderID, Found: ref.OrderID != "", Open: ref.OrderID != "" && !ref.Done, Simulated: true}, nil
		}
		provider = tp
		if ref.OrderID != "" {
			o, err = tp.FetchOrder(ctx, ref.OrderID, ref.Symbol)
		}
	}
	if ref.OrderID == "" { // a cut place step: search for it, read-only
		if ref.Request == nil {
			return OrderState{}, errors.New("the order has no id and no description to look it up by")
		}
		found := false
		o, found, err = broker.FindOrder(ctx, provider, *ref.Request, ref.IdemKey, ref.FirstStartedAt)
		if err != nil || !found {
			return OrderState{}, err
		}
	} else if err != nil {
		return OrderState{}, err
	}
	if o.Status == nil {
		return OrderState{}, errors.New("the exchange did not say whether the order is open")
	}
	id := ref.OrderID
	if o.Id != nil && *o.Id != "" {
		id = *o.Id
	}
	return OrderState{ID: id, Found: true, Open: strings.EqualFold(*o.Status, "open")}, nil
}

// Cancel implements OrderDesk.
func (d BrokerDesk) Cancel(ctx context.Context, ref broker.OrderRef, id string) error {
	if ref.Market == broker.OrderMarketFutures {
		fp, err := d.futures(ref)
		if err != nil {
			return err
		}
		_, err = fp.CancelFuturesOrder(ctx, id, ref.Symbol)
		return err
	}
	tp, _, err := d.spot(ref)
	if err != nil {
		return err
	}
	if tp == nil {
		return nil // simulated order, no simulation: nothing to cancel
	}
	_, err = tp.CancelOrder(ctx, id, ref.Symbol)
	return err
}
