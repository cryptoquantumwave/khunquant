package config

import (
	"math"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestCandidatePolicy_Validation(t *testing.T) {
	ok := func(c *CandidatePolicy) bool {
		p := DefaultConfig().Proactive
		p.Learning.CandidatePolicy = c
		return ValidateProactive(p) == nil
	}
	for name, c := range map[string]*CandidatePolicy{
		"nil":      nil,
		"empty":    {},
		"edges":    {DailyBudget: ptr(0), LowAcceptanceThreshold: ptr(0.0), HoldoutRate: ptr(0.0)},
		"top edge": {DailyBudget: ptr(50), LowAcceptanceThreshold: ptr(0.5), HoldoutRate: ptr(0.2)},
	} {
		if !ok(c) {
			t.Errorf("%s rejected", name)
		}
	}
	for name, c := range map[string]*CandidatePolicy{
		"budget high":     {DailyBudget: ptr(51)},
		"budget negative": {DailyBudget: ptr(-1)},
		"threshold high":  {LowAcceptanceThreshold: ptr(0.51)},
		"threshold low":   {LowAcceptanceThreshold: ptr(-0.01)},
		"threshold nan":   {LowAcceptanceThreshold: ptr(math.NaN())},
		"rate high":       {HoldoutRate: ptr(0.21)},
		"rate negative":   {HoldoutRate: ptr(-0.01)},
		"rate nan":        {HoldoutRate: ptr(math.NaN())},
	} {
		if ok(c) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLearningAdapt_RequiresLearning(t *testing.T) {
	p := DefaultConfig().Proactive
	p.Enabled = true
	p.Owner = OwnerConfig{Channel: "telegram", ChatID: "1", SenderID: "telegram:1"}
	p.Learning.Adapt = true
	e := p.Effective()
	if e.LearningAdapt || e.Learning || len(e.Downgrades) == 0 {
		t.Errorf("adapt without learning: %+v", e)
	}
	p.Learning.Enabled = true
	if e := p.Effective(); !e.LearningAdapt || !e.Learning {
		t.Errorf("adapt with learning: %+v", e)
	}
	p.Owner = OwnerConfig{}
	if e := p.Effective(); e.LearningAdapt || e.Learning {
		t.Errorf("adapt without an owner: %+v", e)
	}
	q := DefaultConfig().Proactive
	if e := q.Effective(); e.LearningAdapt {
		t.Error("adapt is on by default")
	}
}
