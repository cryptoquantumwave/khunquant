package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// composeProseRunes caps the prose a model turn may add to a message.
	composeProseRunes = 600

	// maxForcedSkills and maxSkillNameRunes bound the skills a compose turn
	// carries in full.
	maxForcedSkills   = 4
	maxSkillNameRunes = 64

	// composePrompt is the task of a compose turn. The runtime adds its own
	// contract before it and the candidate list after it.
	composePrompt = "The candidates below were judged worth telling the owner about. " +
		"Their headlines and figures are written by code and are not yours to restate or to change. " +
		"In at most three plain sentences, say why they matter together and what, if anything, the owner " +
		"could look at next. Do not invent figures. Answer through respond_proactive."
)

// TurnRequest is one request for a proactive model turn. It mirrors what the
// agent's RunProactiveTurn takes; the gateway adapts *agent.AgentLoop to
// TurnRunner, so this package never imports the agent.
type TurnRequest struct {
	Kind       proactive.TurnKind
	Prompt     string
	Candidates []proactive.Candidate
	// ForcedSkills is the de-duplicated, capped union of the candidates' forced
	// skills.
	ForcedSkills []string
	// Deadline and Model are left zero by the Composer: the turn's defaults apply.
	Deadline time.Duration
	Model    proactive.ModelTier
}

// TurnResult is what a proactive turn decided. Nothing in it has been delivered.
type TurnResult struct {
	Decisions        []proactive.Decision
	FinalText        string
	ContractViolated bool
}

// TurnRunner runs one proactive turn. It is read-only by construction (the
// agent's turn can call read tools and respond_proactive, nothing else), and it
// clamps every decision's severity to the candidates it was given.
type TurnRunner interface {
	RunProactiveTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
}

// ComposerStats counts what the composer did with the groups it was given.
type ComposerStats struct {
	Composed uint64 // prose was added to the template
	Silent   uint64 // the model had nothing to add: the template went out as it was
	Failed   uint64 // the turn failed: the template went out as it was
	// Protective counts groups with a protective Critical, handed over without a turn.
	Protective uint64
}

// Composer is the attention outbox's Composer for groups that contain an
// escalate decision. It asks one proactive turn to explain the group and adds the
// answer, as prose, under the message the code rendered.
//
// What stays with the code: the title, the evidence, the severity, the dedup key,
// the intervention ID and the decisions of the message. The model's text is
// prose, nothing else, cleaned to one line and capped.
//
// Every way the turn can fail leaves the template message to be delivered, so
// nothing is lost: an error, a timeout, a cancelled context, a model that
// answered in free text and said nothing, and a model that answered "silent".
// That last one is deliberate. Escalation means the governor has already decided
// the owner should hear about this; a model that finds nothing to add cannot
// veto it, it only declines to add prose.
//
// A group with a protective Critical in it is never composed: it is delivered with
// the template at once, and Compose does not call the runner for it (the outbox
// does not even ask; this is the second lock on the same door). Attention decides
// a protective Critical that asks for composition "escalate", and a liquidation-
// class alert must not wait on a model call, nor be followed by a second message.
// The composer only runs for groups that have an escalate decision and no
// protective Critical member.
//
// One turn runs at a time: a second group waits for the first (and gives up if its
// context ends first), whichever goroutine of the outbox it comes from.
type Composer struct {
	run TurnRunner
	sem chan struct{}

	composed, silent, failed, protective atomic.Uint64
}

var _ attention.Composer = (*Composer)(nil)

// NewComposer returns a Composer over run.
func NewComposer(run TurnRunner) *Composer {
	return &Composer{run: run, sem: make(chan struct{}, 1)}
}

// Stats returns the counters.
func (c *Composer) Stats() ComposerStats {
	return ComposerStats{Composed: c.composed.Load(), Silent: c.silent.Load(), Failed: c.failed.Load(), Protective: c.protective.Load()}
}

// Compose implements attention.Composer.
func (c *Composer) Compose(ctx context.Context, m attention.Message, items []attention.Item) (attention.Message, error) {
	if c.run == nil {
		return m, errors.New("compose: no turn runner")
	}
	for _, it := range items {
		if it.Candidate.Protective && it.Candidate.Severity == proactive.SeverityCritical {
			c.protective.Add(1)
			return m, nil // the template goes out as it is
		}
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		c.failed.Add(1)
		return m, fmt.Errorf("compose: waiting for the previous turn: %w", ctx.Err())
	}

	cands := make([]proactive.Candidate, len(items))
	for i, it := range items {
		cands[i] = it.Candidate
	}
	res, err := c.run.RunProactiveTurn(ctx, TurnRequest{
		Kind:         proactive.TurnProactive,
		Prompt:       composePrompt,
		Candidates:   cands,
		ForcedSkills: forcedSkills(cands),
	})
	if err != nil {
		c.failed.Add(1)
		return m, fmt.Errorf("compose: %w", err)
	}

	prose := proseOf(res)
	if prose == "" {
		c.silent.Add(1)
		return m, nil // the template stands; see the type comment
	}
	out := m // title, severity, key and the rest stay exactly as rendered
	if strings.TrimSpace(m.Body) == "" {
		out.Body = prose
	} else {
		out.Body = m.Body + "\n\n" + prose
	}
	c.composed.Add(1)
	return out, nil
}

// forcedSkills is the union of the candidates' forced skills, in first-seen
// order, without repeats or blanks, each name at most maxSkillNameRunes long and
// at most maxForcedSkills in all.
func forcedSkills(cands []proactive.Candidate) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		for _, s := range c.ForcedSkills {
			s = attention.CleanLine(s, maxSkillNameRunes)
			if s == "" || seen[s] {
				continue
			}
			if len(out) >= maxForcedSkills {
				return out
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// proseOf is the text the turn's decisions add: a notify, ask or propose
// decision contributes its text (and an ask its question), a silent one nothing.
func proseOf(res TurnResult) string {
	var parts []string
	for _, d := range res.Decisions {
		if d.Kind == proactive.DecideSilent {
			continue
		}
		if t := attention.CleanLine(d.Text, composeProseRunes); t != "" {
			parts = append(parts, t)
		}
		if d.Kind == proactive.DecideAsk {
			if q := attention.CleanLine(d.Question, composeProseRunes); q != "" {
				parts = append(parts, q)
			}
		}
	}
	return attention.CleanLine(strings.Join(parts, " "), composeProseRunes)
}
