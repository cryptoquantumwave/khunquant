package attentiontest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The fake stands in for the governor under a real outbox: no database.
func TestOutbox_OverFakeGovernor(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	f := &Fake{}
	del := &Deliverer{}
	ob := attention.NewOutbox(f, del, time.Minute, clock)
	ctx := context.Background()

	for _, k := range []string{"a", "b"} {
		if _, err := ob.Submit(ctx, attention.Item{Candidate: proactive.Candidate{DedupKey: k, Title: "t" + k, Severity: proactive.SeverityNotice}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(del.Messages()) != 0 {
		t.Fatal("flushed inside the window")
	}
	now = now.Add(time.Minute)
	if err := ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := del.Messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 2 || msgs[0].Title != "2 updates" {
		t.Fatalf("messages = %+v", msgs)
	}
	got := f.Deliveries()
	if len(got) != 2 || !got[0].Delivery.Delivered || got[0].Delivery.InterventionID != msgs[0].InterventionID {
		t.Fatalf("deliveries = %+v", got)
	}

	// a scripted digest is sent silently and completed.
	f.SetDigest(&attention.Digest{Day: "2026-03-11", Items: []attention.Decision{
		{ID: "d1", Candidate: proactive.Candidate{DedupKey: "x", Title: "tx", Severity: proactive.SeverityInfo}},
	}})
	now = now.Add(time.Hour)
	if err := ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	msgs = del.Messages()
	if len(msgs) != 2 || !msgs[1].Digest || !msgs[1].Silent {
		t.Fatalf("digest message = %+v", msgs)
	}
	if c := f.Completions(); len(c) != 1 || !c[0].Delivery.Delivered || c[0].Digest.Day != "2026-03-11" {
		t.Fatalf("completions = %+v", c)
	}
}

func TestOutbox_OverFake_FailuresAreReported(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	f := &Fake{}
	del := &Deliverer{Err: errors.New("down")}
	ob := attention.NewOutbox(f, del, 0, func() time.Time { return now })
	ctx := context.Background()
	if _, err := ob.Submit(ctx, attention.Item{Candidate: proactive.Candidate{DedupKey: "a", Severity: proactive.SeverityNotice}}); err != nil {
		t.Fatal(err)
	}
	if err := ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deliveries(); len(got) != 1 || got[0].Delivery.Delivered {
		t.Fatalf("deliveries = %+v", got)
	}

	// scripted results and the digest error path.
	no := false
	del2 := &Deliverer{Result: &no}
	if ok, err := del2.Deliver(ctx, attention.Message{}); ok || err != nil {
		t.Fatalf("Result=false gave %v, %v", ok, err)
	}
	f.DigestErr = errors.New("db down")
	now = now.Add(time.Minute)
	if err := ob.Tick(ctx); err == nil {
		t.Fatal("digest error swallowed")
	}
	del3 := &Deliverer{Fn: func(context.Context, attention.Message) (bool, error) { return true, nil }}
	if ok, _ := del3.Deliver(ctx, attention.Message{}); !ok {
		t.Fatal("Fn ignored")
	}
}
