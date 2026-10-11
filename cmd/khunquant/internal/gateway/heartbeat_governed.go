package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// heartbeatTriggerID is the trigger every heartbeat candidate carries.
const heartbeatTriggerID = "heartbeat"

// derivedKeyRunes is how much of a decision's text names its topic when the model
// gave no dedup_key.
const derivedKeyRunes = 40

// proactiveRunner is the part of *agent.AgentLoop the governed heartbeat uses.
type proactiveRunner interface {
	RunProactiveTurn(ctx context.Context, req agent.ProactiveTurnRequest) (agent.ProactiveTurnResult, error)
}

// attentionSubmitter is the part of *attention.Outbox the governed heartbeat
// uses: the only way it delivers anything.
type attentionSubmitter interface {
	Submit(ctx context.Context, it attention.Item) (attention.Decision, error)
}

// heartbeatContract picks the heartbeat's contract for this generation of
// services. It must run after buildAttention: a governed heartbeat delivers
// through the outbox, and without one it stays legacy rather than lose output.
//
//   - legacy, and sweep (MR-213 builds it): legacy;
//   - governed with an outbox that has a Run loop: governed. Effective has already
//     required attention to be live, and the Run loop exists only when the owner
//     can be reached; an outbox without one would hold decisions nobody flushes;
//   - governed without such an outbox: legacy, with one WARN.
func heartbeatContract(cfg *config.Config, s *gatewayServices) heartbeat.Contract {
	eff := cfg.Proactive.Effective()
	switch eff.HeartbeatMode {
	case config.HeartbeatModeGoverned:
		if s.Attention != nil && s.attentionRun != nil {
			return heartbeat.ContractGoverned
		}
		if cfg.Heartbeat.Enabled {
			logger.WarnCF("heartbeat", "proactive.heartbeat.mode is governed but attention cannot deliver "+
				"(its database is unavailable or the owner cannot be reached): "+
				"the heartbeat stays on its legacy path", nil)
		}
	case config.HeartbeatModeSweep:
		if cfg.Heartbeat.Enabled {
			logger.InfoCF("heartbeat", "proactive.heartbeat.mode sweep is not available yet: "+
				"the heartbeat runs on its legacy path", nil)
		}
	}
	return heartbeat.ContractLegacy
}

const (
	// heartbeatDeadlineMax and heartbeatDeadlineMin bound a governed heartbeat
	// turn. The proactive turn's own default (60 s) is sized for composing a
	// message about known candidates; a heartbeat scans the whole portfolio with
	// tools, and a turn that times out delivers nothing.
	heartbeatDeadlineMax = 5 * time.Minute
	heartbeatDeadlineMin = time.Minute
)

// heartbeatDeadline is how long a governed heartbeat turn may run: half the
// interval, so that a slow turn cannot run into the next tick, but between a
// minute and five.
func heartbeatDeadline(interval time.Duration) time.Duration {
	return max(heartbeatDeadlineMin, min(heartbeatDeadlineMax, interval/2))
}

// governedHeartbeatHandler runs a heartbeat as a proactive turn and hands what it
// decided to attention. It is the whole delivery path of a governed heartbeat:
//
//   - nothing is published to the bus and no tool delivers (the turn cannot call
//     `message`);
//   - the last active chat is not consulted (the service passes none);
//   - notify is not called here: attention decides, groups and delivers to the
//     owner.
//
// A turn that fails or times out delivers nothing and the next tick runs as
// usual; there is deliberately no fallback to the legacy path inside a tick,
// which would deliver a second, ungoverned answer for the same wake-up.
func governedHeartbeatHandler(
	run proactiveRunner,
	out attentionSubmitter,
	interval time.Duration,
) heartbeat.ContextHandler {
	deadline := heartbeatDeadline(interval)
	return func(ctx context.Context, prompt, _, _ string) *tools.ToolResult {
		res, err := run.RunProactiveTurn(ctx, agent.ProactiveTurnRequest{
			Kind:         proactive.TurnHeartbeatLegacy,
			Prompt:       prompt,
			ForcedSkills: nil,
			// Bounded by the turn, and ended earlier by the service's context on Stop.
			Deadline: deadline,
			// The cheap model when the agent has one configured (routing with a
			// light model); otherwise the normal model.
			UseLightModel: true,
		})
		if err != nil {
			logger.WarnCF("heartbeat", "Governed heartbeat turn failed; nothing delivered",
				map[string]any{"error": err.Error()})
			return tools.ErrorResult(fmt.Sprintf("Heartbeat turn failed: %v", err))
		}
		return tools.SilentResult(submitHeartbeatDecisions(ctx, out, res))
	}
}

// submitHeartbeatDecisions hands the turn's decisions to attention, one item per
// notify or ask, and returns a one-line summary for the heartbeat log.
func submitHeartbeatDecisions(ctx context.Context, out attentionSubmitter, res agent.ProactiveTurnResult) string {
	var submitted, silent, skipped, failed, dropped int

	if res.ContractViolated {
		logger.WarnCF("heartbeat", "Governed heartbeat answered in free text instead of respond_proactive; "+
			"delivering it as a notice", map[string]any{"decisions": len(res.Decisions)})
	}

	for i, d := range res.Decisions {
		item, ok, why := heartbeatItem(d)
		switch {
		case d.Kind == proactive.DecideSilent:
			silent++
			continue
		case !ok:
			skipped++
			logger.WarnCF("heartbeat", "Governed heartbeat decision not delivered",
				map[string]any{"kind": string(d.Kind), "reason": why})
			continue
		}
		if err := ctx.Err(); err != nil {
			// The service is stopping and the outbox with it: a late decision has
			// nowhere to go.
			dropped = countUndelivered(res.Decisions[i:])
			logger.WarnCF("heartbeat", "Governed heartbeat stopped before its decisions were handed over; dropped",
				map[string]any{"dropped": dropped, "error": err.Error()})
			break
		}
		if _, err := out.Submit(ctx, item); err != nil {
			failed++
			logger.WarnCF("heartbeat", "Attention refused a heartbeat decision",
				map[string]any{"dedup_key": item.Candidate.DedupKey, "error": err.Error()})
			continue
		}
		submitted++
	}

	logger.InfoCF("heartbeat", "Governed heartbeat finished", map[string]any{
		"decisions":         len(res.Decisions),
		"submitted":         submitted,
		"silent":            silent,
		"skipped":           skipped,
		"failed":            failed,
		"dropped":           dropped,
		"contract_violated": res.ContractViolated,
		"input_tokens":      res.InputTokens,
		"output_tokens":     res.OutputTokens,
		"model":             res.Model,
	})
	return fmt.Sprintf("governed: %d decision(s), %d submitted, %d silent, %d skipped, %d failed, %d dropped",
		len(res.Decisions), submitted, silent, skipped, failed, dropped)
}

// countUndelivered counts the decisions in ds that would have been submitted.
func countUndelivered(ds []proactive.Decision) int {
	n := 0
	for _, d := range ds {
		if _, ok, _ := heartbeatItem(d); ok {
			n++
		}
	}
	return n
}

// heartbeatItem turns a decision into what attention is given. ok is false, with
// the reason, for a decision that has nothing to deliver; a silent decision is
// never an item.
//
// A propose decision (possible only when approvals are on, and not yet acted on
// in Phase 1) is delivered as a notify so that it is not lost.
//
// The candidate is model-authored from tool output that may include web pages,
// so it is TrustUntrusted: the least trust, as for any value with unknown
// inputs. The heartbeat has no evidence and no subject of its own, and is never
// protective.
func heartbeatItem(d proactive.Decision) (item attention.Item, ok bool, why string) {
	switch d.Kind {
	case proactive.DecideNotify, proactive.DecideAsk, proactive.DecidePropose:
	case proactive.DecideSilent:
		return attention.Item{}, false, "silent"
	default:
		return attention.Item{}, false, fmt.Sprintf("unknown decision kind %q", d.Kind)
	}

	text := strings.TrimSpace(d.Text)
	question := ""
	if d.Kind == proactive.DecideAsk {
		question = strings.TrimSpace(d.Question)
	}
	if text == "" {
		text = question // an ask with only a question: that is the headline
	}
	if text == "" {
		return attention.Item{}, false, "no text"
	}

	title, rest, _ := strings.Cut(text, "\n")
	title = strings.TrimSpace(title)
	prose := strings.TrimSpace(rest)
	if question != "" && question != text {
		// The question stands on its own last line.
		if prose != "" {
			prose += "\n"
		}
		prose += question
	}

	return attention.Item{
		Candidate: proactive.Candidate{
			TriggerID:  heartbeatTriggerID,
			DedupKey:   heartbeatTriggerID + ":" + heartbeatTopicKey(d, text),
			Severity:   d.Severity,
			Protective: false,
			Subject:    "",
			Title:      title,
			Evidence:   nil,
			Trust:      proactive.TrustUntrusted,
			Compose:    proactive.ComposeTemplate,
		},
		Prose: prose,
	}, true, ""
}

// heartbeatTopicKey names the topic of a decision: the model's dedup_key when it
// gave one (already normalised by respond_proactive), otherwise the first
// derivedKeyRunes characters of the text, lower-cased with every run of
// characters outside [a-z0-9:_-] turned into one "-".
//
// The same text prefix always gives the same key, so a repeated wake-up is
// suppressed by attention's dedup window. Text with nothing in [a-z0-9] (Thai,
// say) would normalise to an empty key and make every such heartbeat the same
// topic, so it is keyed by a hash of those characters instead.
func heartbeatTopicKey(d proactive.Decision, text string) string {
	if d.DedupKey != "" {
		return d.DedupKey
	}
	head := text
	if utf8.RuneCountInString(head) > derivedKeyRunes {
		head = string([]rune(head)[:derivedKeyRunes])
	}

	var b strings.Builder
	pendingDash, alnum := false, false
	for _, r := range strings.ToLower(head) {
		keep := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ':' || r == '_'
		if !keep {
			// Anything else, a literal "-" included, is a separator.
			pendingDash = true
			continue
		}
		if pendingDash && b.Len() > 0 {
			b.WriteByte('-')
		}
		pendingDash = false
		alnum = alnum || r != ':' && r != '_'
		b.WriteRune(r)
	}
	if !alnum {
		sum := sha256.Sum256([]byte(head))
		return "x" + hex.EncodeToString(sum[:6])
	}
	return b.String()
}
