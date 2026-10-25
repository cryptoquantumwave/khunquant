package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const (
	alertTestProviderID = "mockspot"
	alertTestChannel    = "telegram"
	alertTestChatID     = "chat-1"
)

// alertTestProvider extends monitorSpotProvider (delta_neutral_handler_test.go)
// with controllable OHLCV data for indicator alerts.
type alertTestProvider struct {
	monitorSpotProvider
	ohlcv    []ccxt.OHLCV
	ohlcvErr error
}

func (a *alertTestProvider) FetchOHLCV(_ context.Context, _, _ string, _ *int64, _ int) ([]ccxt.OHLCV, error) {
	return a.ohlcv, a.ohlcvErr
}

// risingBars returns n candles whose close is 1, 2, ..., n.
func risingBars(n int) []ccxt.OHLCV {
	bars := make([]ccxt.OHLCV, n)
	for i := range bars {
		c := float64(i + 1)
		bars[i] = ccxt.OHLCV{Timestamp: int64(i) * 60_000, Open: c, High: c, Low: c, Close: c}
	}
	return bars
}

func registerAlertProvider(t *testing.T, p *alertTestProvider) {
	t.Helper()
	broker.RegisterFactory(alertTestProviderID, func(_ *config.Config) (broker.Provider, error) {
		return p, nil
	})
	t.Cleanup(func() { broker.RegisterFactory(alertTestProviderID, nil) })
}

func newAlertCron(t *testing.T) *cron.CronService {
	t.Helper()
	return cron.NewCronService(filepath.Join(t.TempDir(), "alerts.json"), nil)
}

// setAlertClock freezes alertNow at start and returns a function that advances it.
func setAlertClock(t *testing.T, start time.Time) func(d time.Duration) {
	t.Helper()
	cur := start
	prev := alertNow
	alertNow = func() time.Time { return cur }
	t.Cleanup(func() { alertNow = prev })
	return func(d time.Duration) { cur = cur.Add(d) }
}

func addAlertJob(t *testing.T, cs *cron.CronService, name string, payload any, channel, chatID string) *cron.CronJob {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job, err := cs.AddJob(name, cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, string(raw), false, channel, chatID)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.ID == "" || job.Payload.Message == "" {
		t.Fatal("expected a job with an ID and a non-empty payload")
	}
	return job
}

func drainOutbound(mb *bus.MessageBus) []bus.OutboundMessage {
	var out []bus.OutboundMessage
	for {
		select {
		case m := <-mb.OutboundChan():
			out = append(out, m)
		default:
			return out
		}
	}
}

func closedBus() *bus.MessageBus {
	mb := bus.NewMessageBus()
	mb.Close()
	return mb
}

func priceJob(t *testing.T, cs *cron.CronService, p tools.PriceAlertPayload, channel, chatID string) *cron.CronJob {
	t.Helper()
	if p.ProviderID == "" {
		p.ProviderID = alertTestProviderID
	}
	if p.Symbol == "" {
		p.Symbol = "BTC/USDT"
	}
	if p.Condition == "" {
		p.Condition = "above"
	}
	return addAlertJob(t, cs, "price_alert:mockspot:BTC/USDT:above:100", p, channel, chatID)
}

func indicatorJob(t *testing.T, cs *cron.CronService, p tools.IndicatorAlertPayload, channel, chatID string) *cron.CronJob {
	t.Helper()
	if p.ProviderID == "" {
		p.ProviderID = alertTestProviderID
	}
	if p.Symbol == "" {
		p.Symbol = "BTC/USDT"
	}
	if p.Timeframe == "" {
		p.Timeframe = "1h"
	}
	return addAlertJob(t, cs, "indicator_alert:mockspot:BTC/USDT:SMA5:above:1", p, channel, chatID)
}

// runPrice runs one tick of the price handler against the live job state.
func runPrice(t *testing.T, cs *cron.CronService, id string, mb *bus.MessageBus) (string, error) {
	t.Helper()
	job := cs.GetJob(id)
	if job == nil {
		t.Fatalf("job %s not found", id)
	}
	return handlePriceAlertJob(context.Background(), job, &config.Config{}, cs, mb)
}

func runIndicator(t *testing.T, cs *cron.CronService, id string, mb *bus.MessageBus) (string, error) {
	t.Helper()
	job := cs.GetJob(id)
	if job == nil {
		t.Fatalf("job %s not found", id)
	}
	return handleIndicatorAlertJob(context.Background(), job, &config.Config{}, cs, mb)
}

func pricePayloadOf(t *testing.T, cs *cron.CronService, id string) tools.PriceAlertPayload {
	t.Helper()
	job := cs.GetJob(id)
	if job == nil {
		t.Fatalf("job %s not found", id)
	}
	var p tools.PriceAlertPayload
	if err := json.Unmarshal([]byte(job.Payload.Message), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return p
}

func TestPriceAlert_OneShotKeptOnPublishFailure(t *testing.T) {
	registerAlertProvider(t, &alertTestProvider{monitorSpotProvider: monitorSpotProvider{tickerPrice: 150}})
	cs := newAlertCron(t)
	job := priceJob(t, cs, tools.PriceAlertPayload{Threshold: 100}, alertTestChannel, alertTestChatID)

	// A closed bus makes PublishOutbound fail with ErrBusClosed.
	_, err := runPrice(t, cs, job.ID, closedBus())
	if err == nil {
		t.Fatal("expected an error when the publish fails")
	}
	if !errors.Is(err, bus.ErrBusClosed) {
		t.Fatalf("expected ErrBusClosed in the error chain, got %v", err)
	}
	if cs.GetJob(job.ID) == nil {
		t.Fatal("one-shot job must be kept when delivery failed")
	}

	// Next tick with a working bus delivers and then removes the job.
	mb := bus.NewMessageBus()
	if _, err := runPrice(t, cs, job.ID, mb); err != nil {
		t.Fatalf("retry: unexpected error: %v", err)
	}
	if msgs := drainOutbound(mb); len(msgs) != 1 {
		t.Fatalf("expected 1 message on retry, got %d", len(msgs))
	}
	if cs.GetJob(job.ID) != nil {
		t.Fatal("one-shot job must be removed after successful delivery")
	}
}

func TestPriceAlert_OneShotRemovedOnSuccess(t *testing.T) {
	registerAlertProvider(t, &alertTestProvider{monitorSpotProvider: monitorSpotProvider{tickerPrice: 150}})
	cs := newAlertCron(t)
	// Legacy payload without any of the new fields must still decode and fire.
	legacy := `{"provider":"mockspot","account":"","symbol":"BTC/USDT","condition":"above","threshold":100,"alert_msg":"","recurring":false}`
	job, err := cs.AddJob("price_alert:mockspot:BTC/USDT:above:100",
		cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, legacy, false, alertTestChannel, alertTestChatID)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	mb := bus.NewMessageBus()
	if _, err := runPrice(t, cs, job.ID, mb); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := drainOutbound(mb)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Channel != alertTestChannel || msgs[0].ChatID != alertTestChatID || msgs[0].Content == "" {
		t.Fatalf("unexpected message routing/content: %+v", msgs[0])
	}
	if cs.GetJob(job.ID) != nil {
		t.Fatal("one-shot job must be removed after successful delivery")
	}
}

func TestPriceAlert_RecurringCooldown(t *testing.T) {
	cases := []struct {
		name     string
		cooldown int // 0 = default (60)
		want     int
	}{
		{"default cooldown fires at most twice in 2h", 0, 2},
		{"custom 30 minute cooldown", 30, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerAlertProvider(t, &alertTestProvider{monitorSpotProvider: monitorSpotProvider{tickerPrice: 150}})
			advance := setAlertClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			cs := newAlertCron(t)
			job := priceJob(t, cs, tools.PriceAlertPayload{Threshold: 100, Recurring: true, CooldownMinutes: tc.cooldown},
				alertTestChannel, alertTestChatID)
			mb := bus.NewMessageBus()

			fired := 0
			for minute := 0; minute < 120; minute++ { // condition stays true for 2 hours of minute ticks
				if _, err := runPrice(t, cs, job.ID, mb); err != nil {
					t.Fatalf("minute %d: unexpected error: %v", minute, err)
				}
				fired += len(drainOutbound(mb))
				advance(time.Minute)
			}
			if fired != tc.want {
				t.Fatalf("expected %d notifications in 2h, got %d", tc.want, fired)
			}
			if cs.GetJob(job.ID) == nil {
				t.Fatal("recurring job must be kept")
			}
			p := pricePayloadOf(t, cs, job.ID)
			if p.LastFiredMS == 0 {
				t.Fatal("last_fired_ms must be persisted in the payload")
			}
			if p.CooldownMinutes != tc.cooldown || p.Threshold != 100 || !p.Recurring {
				t.Fatalf("handler dropped or altered payload fields: %+v", p)
			}
		})
	}
}

func TestAlert_BrokenNoticeAfterThreeErrors(t *testing.T) {
	prov := &alertTestProvider{monitorSpotProvider: monitorSpotProvider{tickerErr: errors.New("exchange unreachable")}}
	registerAlertProvider(t, prov)
	cs := newAlertCron(t)
	job := priceJob(t, cs, tools.PriceAlertPayload{Threshold: 100, Recurring: true}, alertTestChannel, alertTestChatID)
	mb := bus.NewMessageBus()

	failTick := func(tick int) []bus.OutboundMessage {
		t.Helper()
		if _, err := runPrice(t, cs, job.ID, mb); err == nil {
			t.Fatalf("tick %d: expected the data error to be returned to cron", tick)
		}
		return drainOutbound(mb)
	}

	for tick := 1; tick <= 2; tick++ {
		if msgs := failTick(tick); len(msgs) != 0 {
			t.Fatalf("tick %d: no notice expected yet, got %d", tick, len(msgs))
		}
	}
	msgs := failTick(3)
	if len(msgs) != 1 {
		t.Fatalf("tick 3: expected exactly one notice, got %d", len(msgs))
	}
	want := []string{"Alert '" + job.Name + "'", "BTC/USDT", "mockspot", "exchange unreachable", "It will keep retrying."}
	for _, w := range want {
		if !strings.Contains(msgs[0].Content, w) {
			t.Errorf("notice %q is missing %q", msgs[0].Content, w)
		}
	}
	if msgs[0].Channel != alertTestChannel || msgs[0].ChatID != alertTestChatID {
		t.Errorf("notice sent to wrong destination: %+v", msgs[0])
	}
	if got := pricePayloadOf(t, cs, job.ID).ConsecutiveErrors; got != 3 {
		t.Fatalf("expected consecutive_errors=3 persisted, got %d", got)
	}

	for tick := 4; tick <= 5; tick++ {
		if msgs := failTick(tick); len(msgs) != 0 {
			t.Fatalf("tick %d: notice must not repeat, got %d", tick, len(msgs))
		}
	}
	if got := pricePayloadOf(t, cs, job.ID).ConsecutiveErrors; got != 5 {
		t.Fatalf("expected consecutive_errors=5 after 5 failing ticks, got %d", got)
	}

	// A good evaluation (condition not met) resets the counter.
	prov.tickerErr = nil
	prov.tickerPrice = 50
	if _, err := runPrice(t, cs, job.ID, mb); err != nil {
		t.Fatalf("success tick: unexpected error: %v", err)
	}
	if got := pricePayloadOf(t, cs, job.ID).ConsecutiveErrors; got != 0 {
		t.Fatalf("expected consecutive_errors reset to 0, got %d", got)
	}
	if msgs := drainOutbound(mb); len(msgs) != 0 {
		t.Fatalf("no message expected on a non-triggering success, got %d", len(msgs))
	}

	// A new failure streak can notify again.
	prov.tickerErr = errors.New("exchange unreachable again")
	for tick := 1; tick <= 2; tick++ {
		if msgs := failTick(tick); len(msgs) != 0 {
			t.Fatalf("second streak tick %d: no notice expected, got %d", tick, len(msgs))
		}
	}
	if msgs := failTick(3); len(msgs) != 1 {
		t.Fatalf("second streak tick 3: expected one notice, got %d", len(msgs))
	}
}

func TestAlert_BrokenNoticeIndicatorData(t *testing.T) {
	// Only 3 bars for SMA(5): "not enough data" is a data error.
	registerAlertProvider(t, &alertTestProvider{ohlcv: risingBars(3)})
	cs := newAlertCron(t)
	job := indicatorJob(t, cs, tools.IndicatorAlertPayload{Indicator: "SMA", Period: 5, Condition: "above", Threshold: 1, Recurring: true},
		alertTestChannel, alertTestChatID)
	mb := bus.NewMessageBus()

	total := 0
	for tick := 1; tick <= 5; tick++ {
		if _, err := runIndicator(t, cs, job.ID, mb); err == nil {
			t.Fatalf("tick %d: expected an error", tick)
		}
		msgs := drainOutbound(mb)
		total += len(msgs)
		if tick == 3 && (len(msgs) != 1 || !strings.Contains(msgs[0].Content, "not enough data")) {
			t.Fatalf("tick 3: expected one notice mentioning the cause, got %+v", msgs)
		}
	}
	if total != 1 {
		t.Fatalf("expected exactly one notice over 5 ticks, got %d", total)
	}
}

func TestAlert_InvalidStoredPayloadIsDataError(t *testing.T) {
	registerAlertProvider(t, &alertTestProvider{ohlcv: risingBars(50), monitorSpotProvider: monitorSpotProvider{tickerPrice: 150}})
	cs := newAlertCron(t)
	mb := bus.NewMessageBus()

	price := priceJob(t, cs, tools.PriceAlertPayload{Threshold: 100, Condition: "sideways"}, alertTestChannel, alertTestChatID)
	if _, err := runPrice(t, cs, price.ID, mb); err == nil {
		t.Fatal("expected an error for an invalid stored price condition")
	}
	if got := pricePayloadOf(t, cs, price.ID).ConsecutiveErrors; got != 1 {
		t.Fatalf("expected consecutive_errors=1, got %d", got)
	}

	ind := indicatorJob(t, cs, tools.IndicatorAlertPayload{Indicator: "STOCH", Period: 5, Condition: "above", Threshold: 1},
		alertTestChannel, alertTestChatID)
	if _, err := runIndicator(t, cs, ind.ID, mb); err == nil {
		t.Fatal("expected an error for an unknown stored indicator")
	}
	if cs.GetJob(ind.ID) == nil {
		t.Fatal("job must be kept")
	}
	if msgs := drainOutbound(mb); len(msgs) != 0 {
		t.Fatalf("no user message expected below the threshold, got %d", len(msgs))
	}
}

func TestAlert_MissingRoutingKeepsOneShot(t *testing.T) {
	registerAlertProvider(t, &alertTestProvider{
		ohlcv:               risingBars(120),
		monitorSpotProvider: monitorSpotProvider{tickerPrice: 150},
	})
	cs := newAlertCron(t)
	mb := bus.NewMessageBus()

	cases := []struct {
		name    string
		channel string
		chatID  string
	}{
		{"empty channel", "", alertTestChatID},
		{"empty chat id", alertTestChannel, ""},
	}
	for _, tc := range cases {
		t.Run("price "+tc.name, func(t *testing.T) {
			job := priceJob(t, cs, tools.PriceAlertPayload{Threshold: 100}, tc.channel, tc.chatID)
			_, err := runPrice(t, cs, job.ID, mb)
			if err == nil || !strings.Contains(err.Error(), "channel") {
				t.Fatalf("expected a descriptive routing error, got %v", err)
			}
			if cs.GetJob(job.ID) == nil {
				t.Fatal("one-shot job must be kept when it has no routing")
			}
		})
		t.Run("indicator "+tc.name, func(t *testing.T) {
			job := indicatorJob(t, cs, tools.IndicatorAlertPayload{Indicator: "SMA", Period: 5, Condition: "above", Threshold: 1},
				tc.channel, tc.chatID)
			_, err := runIndicator(t, cs, job.ID, mb)
			if err == nil || !strings.Contains(err.Error(), "channel") {
				t.Fatalf("expected a descriptive routing error, got %v", err)
			}
			if cs.GetJob(job.ID) == nil {
				t.Fatal("one-shot job must be kept when it has no routing")
			}
		})
	}
	if msgs := drainOutbound(mb); len(msgs) != 0 {
		t.Fatalf("nothing can be delivered without routing, got %d messages", len(msgs))
	}
}

func TestIndicatorAlert_OneShotKeptOnPublishFailure(t *testing.T) {
	registerAlertProvider(t, &alertTestProvider{ohlcv: risingBars(120)})
	cs := newAlertCron(t)
	job := indicatorJob(t, cs, tools.IndicatorAlertPayload{Indicator: "SMA", Period: 5, Condition: "above", Threshold: 1},
		alertTestChannel, alertTestChatID)

	_, err := runIndicator(t, cs, job.ID, closedBus())
	if err == nil || !errors.Is(err, bus.ErrBusClosed) {
		t.Fatalf("expected ErrBusClosed, got %v", err)
	}
	if cs.GetJob(job.ID) == nil {
		t.Fatal("one-shot job must be kept when delivery failed")
	}

	mb := bus.NewMessageBus()
	if _, err := runIndicator(t, cs, job.ID, mb); err != nil {
		t.Fatalf("retry: unexpected error: %v", err)
	}
	msgs := drainOutbound(mb)
	if len(msgs) != 1 || msgs[0].Content == "" {
		t.Fatalf("expected one non-empty message on retry, got %+v", msgs)
	}
	if cs.GetJob(job.ID) != nil {
		t.Fatal("one-shot job must be removed after successful delivery")
	}
}

func TestIndicatorAlert_RecurringCooldown(t *testing.T) {
	registerAlertProvider(t, &alertTestProvider{ohlcv: risingBars(120)})
	advance := setAlertClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cs := newAlertCron(t)
	job := indicatorJob(t, cs, tools.IndicatorAlertPayload{Indicator: "SMA", Period: 5, Condition: "above", Threshold: 1, Recurring: true},
		alertTestChannel, alertTestChatID)
	mb := bus.NewMessageBus()

	fired := 0
	for minute := 0; minute < 120; minute++ {
		if _, err := runIndicator(t, cs, job.ID, mb); err != nil {
			t.Fatalf("minute %d: unexpected error: %v", minute, err)
		}
		fired += len(drainOutbound(mb))
		advance(time.Minute)
	}
	if fired != 2 {
		t.Fatalf("expected 2 notifications in 2h, got %d", fired)
	}
}
