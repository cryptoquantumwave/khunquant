package gateway

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

func TestCronOptionsFromConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	got := cronOptionsFromConfig(cfg)
	if got.Workers != 4 || got.MisfireGrace != time.Hour || got.JobTimeout != 10*time.Minute {
		t.Errorf("defaults: %+v", got)
	}

	cfg.Tools.Cron.Workers = 2
	cfg.Tools.Cron.MisfireGraceMinutes = 0
	cfg.Tools.Cron.JobTimeoutMinutes = 0
	got = cronOptionsFromConfig(cfg)
	if got.Workers != 2 || got.MisfireGrace != 0 || got.JobTimeout != 0 {
		t.Errorf("explicit zero values must be honoured (no grace, no timeout): %+v", got)
	}

	cfg.Tools.Cron.Workers = -3
	cfg.Tools.Cron.JobTimeoutMinutes = -1
	got = cronOptionsFromConfig(cfg)
	if got.Workers != 4 || got.JobTimeout != 0 {
		t.Errorf("negative values must not leak into the scheduler: %+v", got)
	}
}

func TestMissedJobNotifier_PublishesOneTemplatedMessage(t *testing.T) {
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	due := time.Date(2025, 6, 2, 9, 30, 0, 0, time.UTC).UnixMilli()
	job := cron.CronJob{
		ID: "j1", Name: "stand-up reminder",
		Schedule: cron.CronSchedule{Kind: "at", AtMS: &due, TZ: "Asia/Bangkok"},
		Payload:  cron.CronPayload{Channel: "telegram", To: "chat-7"},
	}
	missedJobNotifier(msgBus)(job)

	select {
	case msg := <-msgBus.OutboundChan():
		want := "Scheduled task 'stand-up reminder' was missed while the assistant was offline " +
			"(due 2025-06-02 16:30 +07) and has been disabled."
		if msg.Channel != "telegram" || msg.ChatID != "chat-7" || msg.Content != want {
			t.Errorf("published %+v\nwant content %q", msg, want)
		}
	default:
		t.Fatal("expected one message on the job's channel")
	}
	select {
	case msg := <-msgBus.OutboundChan():
		t.Errorf("only one message is expected, got another: %+v", msg)
	default:
	}
}

func TestMissedJobNotifier_NoTargetOrBusIsQuiet(t *testing.T) {
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	due := time.Now().Add(-time.Hour).UnixMilli()
	job := cron.CronJob{Name: "x", Schedule: cron.CronSchedule{Kind: "at", AtMS: &due}}
	missedJobNotifier(msgBus)(job) // no channel/chat: nothing to publish to
	missedJobNotifier(nil)(job)    // no bus: must not panic

	select {
	case msg := <-msgBus.OutboundChan():
		t.Errorf("nothing should be published without a delivery target, got %+v", msg)
	default:
	}
}
