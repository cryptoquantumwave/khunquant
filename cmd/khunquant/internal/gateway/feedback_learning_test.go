package gateway

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func learningConfig(adapt bool) config.ProactiveConfig {
	p := config.DefaultConfig().Proactive
	p.Enabled = true
	p.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1", SenderID: "telegram:1"}
	p.Learning.Enabled = true
	p.Learning.Adapt = adapt
	p.Learning.HoldoutRate = 0.1
	return p
}

// The governor learns only as far as the effective switches say, and gets the config's
// holdout rate and candidate policy.
func TestAttentionLearning_FollowsTheEffectiveSwitches(t *testing.T) {
	budget := 2
	p := learningConfig(false)
	p.Learning.CandidatePolicy = &config.CandidatePolicy{DailyBudget: &budget}
	l := attentionLearning(p, nil)
	if !l.Enabled || l.Adapt || l.HoldoutRate != 0.1 || l.Candidate == nil || l.Rand == nil || l.Source == nil {
		t.Errorf("learning on, adapt off: %+v", l)
	}
	if l := attentionLearning(learningConfig(true), nil); !l.Enabled || !l.Adapt {
		t.Errorf("adapt on: %+v", l)
	}
	noOwner := learningConfig(true)
	noOwner.Owner = config.OwnerConfig{}
	if l := attentionLearning(noOwner, nil); l.Enabled || l.Adapt {
		t.Errorf("no owner: %+v", l)
	}
}

// The zone a response is filed under is the zone attention reads: the profile's, the
// default profile's when it cannot be read.
func TestOwnerZone_MatchesAttention(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	st, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	zone := ownerZone(st)
	want, _ := intent.DefaultProfile().Location()
	if got := zone(ctx); got.String() != want.String() {
		t.Errorf("zone = %v, want %v", got, want)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := zone(cctx); got == nil || got.String() != want.String() {
		t.Errorf("unreadable profile: zone = %v, want %v", got, want)
	}
}
