package gateway

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// These are characterisation tests of the legacy delta-neutral monitor job
// (handleDeltaNeutralMonitorJob): they pin what a run does for a fixed set of
// exchange states, so that moving the monitor into pkg/deltaneutral (Monitor.Check)
// is provably a move, and every later change to what legacy users see shows up here
// as a changed expectation in the commit that makes it.

// charFutures is a futures provider with a configurable position, mark price,
// funding rate and funding history.
type charFutures struct {
	monitorFuturesProvider // order and other methods the monitor never calls
	positions              []ccxt.Position
	mark                   float64
	rate                   *float64
	history                []ccxt.FundingRateHistory
	marketSize             float64
	posErr                 error
}

func (f *charFutures) FetchFuturesPositions(_ context.Context, _ []string) ([]ccxt.Position, error) {
	return f.positions, f.posErr
}
func (f *charFutures) FetchFuturesMarkPrice(_ context.Context, _ string) (float64, error) {
	return f.mark, nil
}
func (f *charFutures) FetchFuturesFundingRate(_ context.Context, _ string) (ccxt.FundingRate, error) {
	if f.rate == nil {
		return ccxt.FundingRate{}, nil
	}
	r := *f.rate
	return ccxt.FundingRate{FundingRate: &r}, nil
}
func (f *charFutures) FetchPublicFundingRateHistory(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.FundingRateHistory, error) {
	return f.history, nil
}
func (f *charFutures) LoadFuturesMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	cs := f.marketSize
	swap, active := true, true
	return map[string]ccxt.MarketInterface{"CHZ/USDT:USDT": {ContractSize: &cs, Swap: &swap, Active: &active}}, nil
}

func fp(v float64) *float64 { return &v }
func sp(v string) *string   { return &v }

// charPos is a CHZ short with a 10-CHZ contract.
func charPos(contracts, liq, marginRatio, upl float64) ccxt.Position {
	return ccxt.Position{
		Symbol: sp("CHZ/USDT:USDT"), Side: sp("short"), Contracts: &contracts, ContractSize: fp(10),
		MarkPrice: fp(0.033), LiquidationPrice: &liq, MarginRatio: &marginRatio, UnrealizedPnl: &upl,
	}
}

type charRun struct {
	result  string
	err     error
	snap    *deltaneutral.MonitorSnapshot
	alert   *deltaneutral.Alert
	silence map[string]time.Time
	sent    []bus.OutboundMessage
}

// runChar seeds a plan whose notify route is telegram:42, runs the legacy handler once
// and returns everything it left behind.
func runChar(t *testing.T, store *deltaneutral.Store, planID int64, jobName string, spot *monitorSpotProvider, fut *charFutures) charRun {
	t.Helper()
	broker.RegisterFactory("mockspot", func(_ *config.Config) (broker.Provider, error) { return spot, nil })
	t.Cleanup(func() { broker.RegisterFactory("mockspot", nil) })
	if fut != nil {
		broker.RegisterFactory("mockfut", func(_ *config.Config) (broker.Provider, error) { return fut, nil })
		t.Cleanup(func() { broker.RegisterFactory("mockfut", nil) })
	}
	_ = cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
	mb := bus.NewMessageBus()
	t.Cleanup(mb.Close)

	var r charRun
	r.result, r.err = handleDeltaNeutralMonitorJob(context.Background(), &cron.CronJob{Name: jobName},
		&config.Config{}, store, (*tools.CronTool)(nil), mb)
	ctx := context.Background()
	if snaps, err := store.ListSnapshots(ctx, planID, 1, 0); err == nil && len(snaps) > 0 {
		r.snap = &snaps[0]
	}
	r.alert, _ = store.LatestAlert(ctx, planID)
	r.silence, _ = store.GetActiveAlertSilences(ctx, planID)
	for {
		select {
		case m := <-mb.OutboundChan():
			r.sent = append(r.sent, m)
		default:
			return r
		}
	}
}

func charPlan(t *testing.T) (*deltaneutral.Store, int64, string) {
	t.Helper()
	store := newMonitorStore(t)
	id, job := seedMonitorPlan(t, store, "mockspot")
	p, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	p.FuturesProvider, p.NotifyChannel, p.NotifyChatID = "mockfut", "telegram", "42"
	if err := store.UpdatePlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return store, id, job
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func charSpot(qty float64) *monitorSpotProvider {
	return &monitorSpotProvider{
		tickerPrice: 0.033, balances: []broker.Balance{{Asset: "CHZ", Free: qty}}, earnErr: errFakeEarnUnavailable,
	}
}

// A healthy hedge: both legs ~44.55 USDT, a positive rate, a far liquidation price.
func TestCharacterise_Monitor_HealthyHedge(t *testing.T) {
	store, id, job := charPlan(t)
	fut := &charFutures{positions: []ccxt.Position{charPos(135, 0.1, 0.8, 0.12)}, mark: 0.033, rate: fp(0.0001)}
	r := runChar(t, store, id, job, charSpot(1350), fut)

	if r.err != nil || r.result != "ok: no breach" {
		t.Fatalf("result = %q, %v", r.result, r.err)
	}
	if r.snap == nil {
		t.Fatal("no snapshot")
	}
	s := r.snap
	if !near(s.SpotValueUSDT, 44.55) || !near(s.FuturesNotionalUSDT, 44.55) || s.DeltaDriftPct > 1e-9 {
		t.Errorf("legs: spot %v futures %v drift %v", s.SpotValueUSDT, s.FuturesNotionalUSDT, s.DeltaDriftPct)
	}
	if !near(s.SpotQuantity, 1350) || !near(s.FuturesContracts, 135) || !near(s.FuturesUnrealizedPnLUSDT, 0.12) {
		t.Errorf("quantities: %+v", s)
	}
	if s.FundingState != "positive" || s.MarginState != "safe" || !near(s.MarginRatioPct, 80) {
		t.Errorf("states: funding %q margin %q ratio %v", s.FundingState, s.MarginState, s.MarginRatioPct)
	}
	if !near(s.CurrentFundingRate, 0.0001) || !near(s.FundingAPYPct, 0.0001*3*365*100) {
		t.Errorf("funding: rate %v apy %v", s.CurrentFundingRate, s.FundingAPYPct)
	}
	if !near(s.LiquidationDistancePct, 100) { // |0.033-0.1|/0.033 is capped at 100
		t.Errorf("liquidation distance = %v", s.LiquidationDistancePct)
	}
	if s.ThresholdBreached || len(s.BreachCodes) != 0 || s.DataStatus != deltaneutral.DataStatusOK || !s.CrossExchange {
		t.Errorf("flags: %+v", s)
	}
	// 20 funding + 25 margin + 20 delta + 6 cross-exchange. (MR-210: it was 96, "excellent",
	// because liquidity and profit progress were awarded 25 points with no data.)
	if s.HealthScore != 71 || s.HealthLabel != deltaneutral.HealthLabelWatch {
		t.Errorf("score %d label %q", s.HealthScore, s.HealthLabel)
	}
	if r.alert != nil || len(r.silence) != 0 || len(r.sent) != 0 {
		t.Errorf("a healthy run left an alert/silence/message: %+v %v %v", r.alert, r.silence, r.sent)
	}
}

// A near-liquidation position: two critical codes, an alert row, one bus message, no silence.
func TestCharacterise_Monitor_MarginCritical(t *testing.T) {
	store, id, job := charPlan(t)
	fut := &charFutures{positions: []ccxt.Position{charPos(135, 0.0345, 0.9, -0.5)}, mark: 0.033, rate: fp(0.0001)}
	r := runChar(t, store, id, job, charSpot(1350), fut)

	if r.result != "breach alerted (no agent)" || r.err != nil {
		t.Fatalf("result = %q, %v", r.result, r.err)
	}
	s := r.snap
	if s == nil || !s.ThresholdBreached {
		t.Fatalf("snapshot = %+v", s)
	}
	want := []string{"liquidation_distance_low", "margin_critical"}
	if strings.Join(s.BreachCodes, ",") != strings.Join(want, ",") {
		t.Errorf("codes = %v, want %v", s.BreachCodes, want)
	}
	if s.MarginState != "critical" || s.HealthScore != 46 || s.HealthLabel != deltaneutral.HealthLabelDanger { // MR-210: was 71, "watch"
		t.Errorf("margin %q score %d label %q", s.MarginState, s.HealthScore, s.HealthLabel)
	}
	a := r.alert
	if a == nil {
		t.Fatal("no alert row")
	}
	if a.Severity != "critical" || a.Code != "liquidation_distance_low" || a.AgentInvoked ||
		a.DeliveredChannel != "telegram" || a.DeliveredChatID != "42" || a.SnapshotID == nil {
		t.Errorf("alert = %+v", a)
	}
	if a.Message != fmt.Sprintf("Delta-neutral plan %d (monitor-test) health alert: [liquidation_distance_low margin_critical]", id) {
		t.Errorf("alert message = %q", a.Message)
	}
	if !strings.HasPrefix(a.RecommendedAction, "Liquidation distance is dangerously low.") { // the first critical code decides
		t.Errorf("recommended action = %q", a.RecommendedAction)
	}
	if len(r.silence) != 0 {
		t.Errorf("critical codes must never be silenced: %v", r.silence)
	}
	if len(r.sent) != 1 || r.sent[0].Channel != "telegram" || r.sent[0].ChatID != "42" || r.sent[0].Content != a.Message {
		t.Errorf("bus messages = %+v", r.sent)
	}
}

// A non-critical breach alerts once, is silenced for the cooldown, and the next run
// says so without a second alert.
func TestCharacterise_Monitor_DeltaDriftAlertsOnceThenSilenced(t *testing.T) {
	store, id, job := charPlan(t)
	fut := &charFutures{positions: []ccxt.Position{charPos(135, 0.1, 0.8, 0)}, mark: 0.033, rate: fp(0.0001)}
	spot := charSpot(675) // half the notional: 22.275 vs 44.55

	r := runChar(t, store, id, job, spot, fut)
	if r.result != "breach alerted (no agent)" {
		t.Fatalf("first run: %q", r.result)
	}
	// MR-210: leg_mismatch joins it (the futures leg is twice the spot quantity).
	if got := strings.Join(r.snap.BreachCodes, ","); got != "delta_drift_high,leg_mismatch" {
		t.Fatalf("codes = %s", got)
	}
	if !near(r.snap.DeltaDriftPct, 50) {
		t.Errorf("drift = %v", r.snap.DeltaDriftPct)
	}
	if r.alert == nil || r.alert.Severity != "danger" || r.alert.Code != "delta_drift_high" {
		t.Fatalf("alert = %+v", r.alert)
	}
	if _, ok := r.silence["delta_drift_high"]; !ok || len(r.sent) != 1 {
		t.Fatalf("silence %v, sent %d", r.silence, len(r.sent))
	}
	first := r.alert.ID

	r2 := runChar(t, store, id, job, spot, fut)
	if r2.result != "silenced: cooldown active for all breach codes" || len(r2.sent) != 0 {
		t.Fatalf("second run: %q, %d messages", r2.result, len(r2.sent))
	}
	if r2.alert == nil || r2.alert.ID != first {
		t.Errorf("a second alert row was written: %+v", r2.alert)
	}
}

// A negative funding rate after a run of negative settlements is a reversal. (MR-210:
// the legacy handler left RecentRates empty, so this was only ever "negative" and
// funding_reversal could never fire.)
func TestCharacterise_Monitor_NegativeFundingAfterNegativeRunReverses(t *testing.T) {
	store, id, job := charPlan(t)
	neg := -0.0002
	var hist []ccxt.FundingRateHistory
	now := time.Now().UnixMilli()
	for i := 0; i < 4; i++ { // a crafted series: four negative settlements in a row
		ts, rate := now-int64(4-i)*8*3600*1000, neg
		hist = append(hist, ccxt.FundingRateHistory{Timestamp: &ts, FundingRate: &rate})
	}
	fut := &charFutures{positions: []ccxt.Position{charPos(135, 0.1, 0.8, 0)}, mark: 0.033, rate: &neg, history: hist}
	r := runChar(t, store, id, job, charSpot(1350), fut)

	if r.snap == nil || r.snap.FundingState != "reversing" {
		t.Fatalf("funding state = %+v", r.snap)
	}
	if got := strings.Join(r.snap.BreachCodes, ","); got != "funding_reversal" {
		t.Fatalf("codes = %s", got)
	}
	if r.alert == nil || r.alert.Severity != "danger" || r.alert.Code != "funding_reversal" {
		t.Fatalf("alert = %+v", r.alert)
	}
}

// With no futures provider the run is a data failure: one critical data_unavailable.
func TestCharacterise_Monitor_DataUnavailable(t *testing.T) {
	store, id, job := charPlan(t)
	p, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	p.FuturesProvider = "no-such-futures-provider"
	if err := store.UpdatePlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r := runChar(t, store, id, job, charSpot(1350), nil)

	if r.snap == nil || r.snap.DataStatus != deltaneutral.DataStatusError {
		t.Fatalf("snapshot = %+v", r.snap)
	}
	if got := strings.Join(r.snap.BreachCodes, ","); got != "data_unavailable" {
		t.Fatalf("codes = %s", got)
	}
	if r.alert == nil || r.alert.Severity != "critical" || r.alert.Code != "data_unavailable" || len(r.silence) != 0 {
		t.Fatalf("alert %+v silence %v", r.alert, r.silence)
	}
}

// The monitor reads the position of the plan's symbol and side, not whichever the
// exchange lists first. (MR-210: it used to take positions[0].)
func TestCharacterise_Monitor_MatchesPositionBySymbolAndSide(t *testing.T) {
	store, id, job := charPlan(t)
	otherSymbol := charPos(500, 0.1, 0.8, 0)
	otherSymbol.Symbol = sp("BTC/USDT:USDT")
	otherSide := charPos(700, 0.1, 0.8, 0)
	otherSide.Side = sp("long")
	fut := &charFutures{positions: []ccxt.Position{otherSymbol, otherSide, charPos(135, 0.1, 0.8, 0)}, mark: 0.033, rate: fp(0.0001)}
	r := runChar(t, store, id, job, charSpot(1350), fut)

	if r.snap == nil || !near(r.snap.FuturesContracts, 135) {
		t.Fatalf("snapshot = %+v", r.snap)
	}
}
