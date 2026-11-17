package deltaneutral

import (
	"context"
	"errors"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// legSpot is a spot provider whose balance and earn reads can be made to fail or come back
// empty, to pin how the monitor judges a spot leg it cannot see (F-DN-1).
type legSpot struct {
	price    float64
	balances []broker.Balance
	balErr   error
	earn     []broker.EarnPosition
	earnErr  error
}

func (s *legSpot) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	p := s.price
	return ccxt.Ticker{Last: &p}, nil
}
func (s *legSpot) GetBalances(context.Context) ([]broker.Balance, error) { return s.balances, s.balErr }
func (s *legSpot) FetchFlexibleEarnPositions(context.Context) ([]broker.EarnPosition, error) {
	return s.earn, s.earnErr
}
func (s *legSpot) FetchFlexibleEarnProducts(context.Context, string) ([]broker.EarnProduct, error) {
	return nil, nil
}
func (s *legSpot) FetchFlexibleEarnRateHistory(context.Context, string, string, *int64, int) ([]broker.EarnRatePoint, error) {
	return nil, nil
}

func legCheck(t *testing.T, spot *legSpot) MonitorResult {
	t.Helper()
	// A matched short: 135 contracts x 10 CHZ = 1350 CHZ, the size the plan's notional buys.
	fut := &fakeFutures{positions: []ccxt.Position{shortPos("CHZ/USDT:USDT", "short", 135, 10)}, mark: 0.033, rate: 0.0001}
	m := &Monitor{Providers: ProviderResolverFunc(func(id, _ string) (any, error) {
		if id == "spot" {
			return spot, nil
		}
		return fut, nil
	})}
	res, err := m.Check(context.Background(), monitorPlan())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMonitorSpotLeg_NormalSizeIsHealthy(t *testing.T) {
	res := legCheck(t, &legSpot{price: 0.033, balances: []broker.Balance{{Asset: "CHZ", Free: 1350}}})
	if res.Eval.DataStatus != DataStatusOK || hasCode(res.Eval.BreachCodes, planhealth.CodeSpotLegNotVisible) || res.Eval.ThresholdBreached {
		t.Fatalf("status %s codes %v", res.Eval.DataStatus, res.Eval.BreachCodes)
	}
	if res.Snapshot.DeltaDriftPct > 1 {
		t.Fatalf("drift %v for a matched hedge", res.Snapshot.DeltaDriftPct)
	}
}

func TestMonitorSpotLeg_SuccessfulReadWithNoRowIsNotVisible(t *testing.T) {
	// The balance reply holds other assets but no CHZ: the leg was sold. Never "healthy".
	res := legCheck(t, &legSpot{price: 0.033, balances: []broker.Balance{{Asset: "USDT", Free: 100}}})
	assertNotVisible(t, res)
	if res.Snapshot.SpotQuantity != 0 || res.Snapshot.SpotValueUSDT != 0 {
		t.Fatalf("the real quantity is 0, got qty %v value %v (the plan notional must never be assumed)", res.Snapshot.SpotQuantity, res.Snapshot.SpotValueUSDT)
	}
	if res.Eval.LegMismatchPct != 100 {
		t.Fatalf("leg mismatch %v, want the real gap 100", res.Eval.LegMismatchPct)
	}
}

func TestMonitorSpotLeg_RowUnderOnePercentIsNotVisible(t *testing.T) {
	// 1 CHZ x 0.033 = 0.033 USDT, under 1% of 44.55.
	res := legCheck(t, &legSpot{price: 0.033, balances: []broker.Balance{{Asset: "CHZ", Free: 1}}})
	assertNotVisible(t, res)
	if res.Snapshot.SpotQuantity != 1 {
		t.Fatalf("qty %v, want the real 1", res.Snapshot.SpotQuantity)
	}
}

func TestMonitorSpotLeg_EarnHoldingCountsAsVisible(t *testing.T) {
	res := legCheck(t, &legSpot{price: 0.033, balances: nil, earn: []broker.EarnPosition{{Asset: "CHZ", Amount: 1350}}})
	if hasCode(res.Eval.BreachCodes, planhealth.CodeSpotLegNotVisible) || res.Eval.DataStatus != DataStatusOK {
		t.Fatalf("status %s codes %v", res.Eval.DataStatus, res.Eval.BreachCodes)
	}
}

func TestMonitorSpotLeg_SpotReadFailedIsDataNotOK(t *testing.T) {
	res := legCheck(t, &legSpot{price: 0.033, balErr: errors.New("api down")})
	assertDataNotOK(t, res)
}

func TestMonitorSpotLeg_EarnReadFailedWithSpotPresentIsJudgedOnTheSpotRead(t *testing.T) {
	// Earn can only add to the balance, so a visible leg stays visible.
	res := legCheck(t, &legSpot{price: 0.033, balances: []broker.Balance{{Asset: "CHZ", Free: 1350}}, earnErr: errors.New("no fixture")})
	if res.Eval.DataStatus != DataStatusOK || hasCode(res.Eval.BreachCodes, planhealth.CodeSpotLegNotVisible) || res.Eval.ThresholdBreached {
		t.Fatalf("status %s codes %v", res.Eval.DataStatus, res.Eval.BreachCodes)
	}
}

func TestMonitorSpotLeg_EarnReadFailedWithNoSpotIsDataNotOK(t *testing.T) {
	// Nothing in the balance and the earn read failed: the leg may be in earn. Unknown, not "not visible".
	res := legCheck(t, &legSpot{price: 0.033, balances: nil, earnErr: errors.New("api down")})
	assertDataNotOK(t, res)
}

func TestMonitorSpotLeg_BothReadsFailedIsDataNotOK(t *testing.T) {
	res := legCheck(t, &legSpot{price: 0.033, balErr: errors.New("api down"), earnErr: errors.New("api down")})
	assertDataNotOK(t, res)
}

func TestMonitorSpotLeg_NotVisibleIsAWarningNotACritical(t *testing.T) {
	// Danger class is the Warning severity; a Critical code would re-alert every tick.
	if c, ok := planhealth.ClassOf(planhealth.CodeSpotLegNotVisible); !ok || c != planhealth.ClassDanger {
		t.Fatalf("class %v ok %v", c, ok)
	}
}

func assertNotVisible(t *testing.T, res MonitorResult) {
	t.Helper()
	if res.Eval.DataStatus != DataStatusOK {
		t.Fatalf("a successful read is data OK, got %s", res.Eval.DataStatus)
	}
	if !hasCode(res.Eval.BreachCodes, planhealth.CodeSpotLegNotVisible) || !res.Eval.ThresholdBreached {
		t.Fatalf("codes %v: a missing leg must be reported", res.Eval.BreachCodes)
	}
	if res.Snapshot.DeltaDriftPct < 90 {
		t.Fatalf("drift %v: the real gap must show, never 0", res.Snapshot.DeltaDriftPct)
	}
	if res.Eval.Severity == "critical" {
		t.Fatalf("severity %s, want a warning", res.Eval.Severity)
	}
}

func assertDataNotOK(t *testing.T, res MonitorResult) {
	t.Helper()
	if res.Eval.DataStatus == DataStatusOK {
		t.Fatalf("a failed read must not be data OK")
	}
	if hasCode(res.Eval.BreachCodes, planhealth.CodeSpotLegNotVisible) {
		t.Fatalf("codes %v: an unknown leg is not 'not visible'", res.Eval.BreachCodes)
	}
	if !hasCode(res.Eval.BreachCodes, planhealth.CodeDataUnavailable) {
		t.Fatalf("codes %v, want data_unavailable", res.Eval.BreachCodes)
	}
}
