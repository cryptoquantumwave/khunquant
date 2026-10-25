package attentiontest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestFake_DefaultsToNotify(t *testing.T) {
	f := &Fake{}
	c := proactive.Candidate{DedupKey: "k", Severity: proactive.SeverityNotice}
	d, err := f.Decide(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Disposition != proactive.DispNotify || d.Reason != attention.ReasonTemplate || d.ID == "" || d.Candidate.DedupKey != "k" {
		t.Fatalf("default decision = %+v", d)
	}
	if got := f.Candidates(); len(got) != 1 || got[0].DedupKey != "k" {
		t.Fatalf("recorded candidates = %+v", got)
	}
}

func TestFake_ScriptRepeatsLastAndKeepsGivenID(t *testing.T) {
	f := &Fake{Script: []attention.Decision{
		{ID: "d1", Disposition: proactive.DispDrop, Reason: attention.ReasonMuted},
		{Disposition: proactive.DispDigest, Reason: attention.ReasonQuietHours},
	}}
	var got []attention.Decision
	for i := 0; i < 3; i++ {
		d, err := f.Decide(context.Background(), proactive.Candidate{DedupKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, d)
	}
	if got[0].ID != "d1" || got[0].Disposition != proactive.DispDrop {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Disposition != proactive.DispDigest || got[2].Disposition != proactive.DispDigest || got[1].ID == "" {
		t.Errorf("repeat = %+v %+v", got[1], got[2])
	}
}

func TestFake_FuncAndErrors(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{
		DecideFn: func(_ context.Context, c proactive.Candidate) (attention.Decision, error) {
			return attention.Decision{Disposition: proactive.DispEscalate, Candidate: c}, nil
		},
	}
	if d, _ := f.Decide(context.Background(), proactive.Candidate{}); d.Disposition != proactive.DispEscalate {
		t.Fatalf("DecideFn ignored: %+v", d)
	}

	f = &Fake{DecideErr: boom, MarkErr: boom, MuteErr: boom}
	if _, err := f.Decide(context.Background(), proactive.Candidate{}); !errors.Is(err, boom) {
		t.Errorf("Decide err = %v", err)
	}
	if err := f.MarkDelivered(context.Background(), attention.Decision{ID: "x"}, attention.Delivery{Delivered: true}); !errors.Is(err, boom) {
		t.Errorf("MarkDelivered err = %v", err)
	}
	until := time.Unix(100, 0)
	if err := f.Mute(context.Background(), "dn:", until, "why"); !errors.Is(err, boom) {
		t.Errorf("Mute err = %v", err)
	}
	if err := f.Unmute(context.Background(), "dn:"); !errors.Is(err, boom) {
		t.Errorf("Unmute err = %v", err)
	}

	if got := f.Deliveries(); len(got) != 1 || got[0].Decision.ID != "x" || !got[0].Delivery.Delivered {
		t.Errorf("deliveries = %+v", got)
	}
	m := f.Mutes()
	if len(m) != 2 || m[0].Prefix != "dn:" || !m[0].Until.Equal(until) || m[0].Reason != "why" || m[0].Unmute || !m[1].Unmute {
		t.Errorf("mutes = %+v", m)
	}
}
