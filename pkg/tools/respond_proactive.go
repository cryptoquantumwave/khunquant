package tools

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// MaxProactiveTextRunes bounds the text and question of one decision.
	MaxProactiveTextRunes = 1200
	// MaxProactiveDedupKeyLen bounds a decision's dedup key.
	MaxProactiveDedupKeyLen = 64
)

// ProactiveCandidateRef is what respond_proactive knows about one candidate of
// the request: enough to check what the model cites and to cap how urgent it may
// call it. It carries no text, so nothing a candidate says reaches the tool.
type ProactiveCandidateRef struct {
	// RenderedID is the ID as the model was shown it (control characters
	// stripped, length capped). candidate_ids must match this.
	RenderedID string
	// ID is the candidate's own ID, which the decision carries back to the caller.
	ID         string
	Severity   proactive.Severity
	Protective bool
}

// RespondProactiveTool is how a proactive turn declares what it wants to tell
// the owner. It sends nothing: it records a validated proactive.Decision in the
// turn's collector, and the runtime decides what to deliver.
//
// It is built per turn, from that turn's candidates, and registered on that
// turn's registry only - never on a shared one. It also refuses to run outside
// a proactive turn or without a collector, so a registration mistake cannot make
// it reachable from a turn a person started.
//
// What the model declares is checked here, in code. The model cannot raise the
// urgency of a message past what the candidates it cites support, cannot name a
// candidate the request did not contain, and cannot ask for an approval unless
// approvals are on.
type RespondProactiveTool struct {
	all       []ProactiveCandidateRef
	byID      map[string]ProactiveCandidateRef
	approvals bool
}

// NewRespondProactiveTool builds the tool for one turn. approvals reports
// whether proactive.approvals is on: when it is not, "propose" is downgraded to
// "ask".
func NewRespondProactiveTool(candidates []ProactiveCandidateRef, approvals bool) *RespondProactiveTool {
	t := &RespondProactiveTool{
		all:       append([]ProactiveCandidateRef(nil), candidates...),
		byID:      make(map[string]ProactiveCandidateRef, len(candidates)),
		approvals: approvals,
	}
	for _, c := range t.all {
		if _, dup := t.byID[c.RenderedID]; !dup {
			t.byID[c.RenderedID] = c
		}
	}
	return t
}

func (t *RespondProactiveTool) Name() string { return NameRespondProactive }

func (t *RespondProactiveTool) Description() string {
	return "Declare what to tell the owner about this wake-up. Call it exactly once per topic. " +
		"If nothing is material, call it once with decision \"silent\". Delivery is done by the system, not by you."
}

func (t *RespondProactiveTool) Parameters() map[string]any {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{
				"type":        "string",
				"enum":        []string{"silent", "notify", "ask", "propose"},
				"description": "silent: say nothing. notify: tell the owner. ask: ask the owner a question. propose: suggest an action for approval.",
			},
			"severity": map[string]any{
				"type":        "string",
				"enum":        []string{"info", "notice", "warning", "critical"},
				"description": "How urgent this is. The system caps it by the candidates you cite.",
			},
			"text": map[string]any{
				"type":      "string",
				"maxLength": MaxProactiveTextRunes,
				"description": "Why it matters to the user and a suggested next step. " +
					"Do NOT restate numbers shown in the evidence block. Required unless decision is silent.",
			},
			"question": map[string]any{
				"type":        "string",
				"maxLength":   MaxProactiveTextRunes,
				"description": "The question to put to the owner, for decision ask.",
			},
			"dedup_key": map[string]any{
				"type":        "string",
				"maxLength":   MaxProactiveDedupKeyLen,
				"description": "Short stable topic key, e.g. 'sol:drop' (lower-case letters, digits, : _ -).",
			},
			"candidate_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The ids of the candidates this decision is about.",
			},
		},
		"required": []string{"decision"},
	}
	if t.approvals {
		// Only while approvals are on: the schema is part of every wake-up's first
		// call, and without approvals there is no proposal to refer to.
		params["properties"].(map[string]any)["proposal_ref"] = map[string]any{
			"type":        "string",
			"maxLength":   approval.CodeLen,
			"description": "propose: the code propose_order returned.",
		}
	}
	return params
}

func (t *RespondProactiveTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	if ToolOrigin(ctx) != OriginProactive {
		return ErrorResult(NameRespondProactive + " is only available in a proactive turn")
	}
	collector := proactive.CollectorFrom(ctx)
	if collector == nil {
		return ErrorResult(NameRespondProactive + " has nowhere to record a decision: this turn collects none")
	}

	kindName, err := proactiveStringArg(args, "decision")
	if err != nil {
		return ErrorResult(err.Error())
	}
	kind, ok := parseDecisionKind(kindName)
	if !ok {
		return ErrorResult(fmt.Sprintf("decision must be one of silent, notify, ask, propose; got %q", clip(kindName, 40)))
	}

	// Severity is optional: the model that omits it gets the lowest that still
	// reaches the owner, never a high one.
	sev := proactive.SeverityNotice
	if kind == proactive.DecideSilent {
		sev = proactive.SeverityInfo
	}
	if sevName, err := proactiveStringArg(args, "severity"); err != nil {
		return ErrorResult(err.Error())
	} else if sevName != "" {
		parsed, ok := parseSeverityExact(sevName)
		if !ok {
			return ErrorResult(fmt.Sprintf("severity must be one of info, notice, warning, critical; got %q", clip(sevName, 40)))
		}
		sev = parsed
	}

	text, err := proactiveStringArg(args, "text")
	if err != nil {
		return ErrorResult(err.Error())
	}
	question, err := proactiveStringArg(args, "question")
	if err != nil {
		return ErrorResult(err.Error())
	}
	dedupRaw, err := proactiveStringArg(args, "dedup_key")
	if err != nil {
		return ErrorResult(err.Error())
	}
	text = CleanProactiveText(text)
	question = CleanProactiveText(question)
	if kind != proactive.DecideSilent && text == "" {
		return ErrorResult("text is required for notify, ask and propose: say why it matters and what to do next")
	}

	refs, unknown := t.resolve(args["candidate_ids"])
	var notes []string
	if unknown > 0 {
		notes = append(notes, fmt.Sprintf("%d candidate id(s) are not in this request and were ignored", unknown))
	}

	capSev := t.severityCap(refs)
	if sev > capSev {
		logger.WarnCF("tool", "respond_proactive severity clamped",
			map[string]any{"asked": sev.String(), "cap": capSev.String()})
		notes = append(notes, fmt.Sprintf("severity clamped to %s", capSev))
		sev = capSev
	}

	proposalRef, err := proactiveStringArg(args, "proposal_ref")
	if err != nil {
		return ErrorResult(err.Error())
	}
	proposalRef = approval.NormalizeCode(proposalRef)
	if kind == proactive.DecidePropose {
		scope := ProposalScopeFrom(ctx)
		switch {
		case !t.approvals:
			kind = proactive.DecideAsk
			notes = append(notes, "approvals are off, so this was recorded as ask")
		case proposalRef == "" || scope == nil || !scope.Created(proposalRef):
			// A propose names a proposal this turn created through propose_order;
			// anything else is only a question to the owner.
			kind = proactive.DecideAsk
			proposalRef = ""
			notes = append(notes, "no proposal made in this turn matches proposal_ref, so this was recorded as ask")
		}
	}

	d := proactive.Decision{
		Kind:     kind,
		Severity: sev,
		DedupKey: normalizeDedupKey(dedupRaw),
	}
	if kind == proactive.DecidePropose {
		d.ProposalRef = proposalRef
	}
	if kind != proactive.DecideSilent {
		d.Text = text
		d.Question = question
		for _, r := range refs {
			d.CandidateIDs = append(d.CandidateIDs, r.ID)
		}
	}

	if !collector.Add(d) {
		logger.WarnCF("tool", "respond_proactive decision dropped: turn is over the decision cap",
			map[string]any{"cap": proactive.MaxDecisionsPerTurn})
		return ErrorResult(fmt.Sprintf("not recorded: a turn may declare at most %d decisions", proactive.MaxDecisionsPerTurn))
	}

	msg := fmt.Sprintf("recorded: %s (severity %s)", kind, sev)
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	return SilentResult(msg)
}

// resolve maps the ids the model cited to the request's candidates, dropping
// duplicates and ids the request did not contain. It returns how many it
// dropped for being unknown.
func (t *RespondProactiveTool) resolve(raw any) (refs []ProactiveCandidateRef, unknown int) {
	var ids []string
	switch v := raw.(type) {
	case []string:
		ids = v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				ids = append(ids, s)
			} else {
				unknown++
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		ref, ok := t.byID[strings.TrimSpace(id)]
		if !ok {
			unknown++
			continue
		}
		if seen[ref.RenderedID] {
			continue
		}
		seen[ref.RenderedID] = true
		refs = append(refs, ref)
	}
	return refs, unknown
}

// severityCap is the highest severity a decision citing refs may carry: one tier
// above the most severe candidate cited, critical only when one of them is
// protective, and never more than warning when the request had no candidates.
// Citing none (or only ids the request lacks) means all of the request's
// candidates count.
func (t *RespondProactiveTool) severityCap(refs []ProactiveCandidateRef) proactive.Severity {
	if len(refs) == 0 {
		refs = t.all
	}
	if len(refs) == 0 {
		return proactive.SeverityWarning
	}
	highest, protective := proactive.SeverityInfo, false
	for _, r := range refs {
		if r.Severity > highest {
			highest = r.Severity
		}
		protective = protective || r.Protective
	}
	capSev := highest + 1
	if capSev > proactive.SeverityCritical {
		capSev = proactive.SeverityCritical
	}
	if capSev == proactive.SeverityCritical && !protective {
		capSev = proactive.SeverityWarning
	}
	return capSev
}

func parseDecisionKind(s string) (proactive.DecisionKind, bool) {
	switch k := proactive.DecisionKind(s); k {
	case proactive.DecideSilent, proactive.DecideNotify, proactive.DecideAsk, proactive.DecidePropose:
		return k, true
	}
	return "", false
}

// parseSeverityExact accepts only the four lower-case names. proactive.ParseSeverity
// is case-insensitive; a value the schema's enum does not list is refused here.
func parseSeverityExact(s string) (proactive.Severity, bool) {
	switch s {
	case "info":
		return proactive.SeverityInfo, true
	case "notice":
		return proactive.SeverityNotice, true
	case "warning":
		return proactive.SeverityWarning, true
	case "critical":
		return proactive.SeverityCritical, true
	}
	return 0, false
}

// proactiveStringArg reads an optional string argument; a value of another type is an
// error the model can read.
func proactiveStringArg(args map[string]any, key string) (string, error) {
	v, present := args[key]
	if !present || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

// CleanProactiveText prepares model text for a person to read: control and
// invisible formatting characters out (newlines and tabs stay), trimmed, and cut
// to MaxProactiveTextRunes on a rune boundary.
func CleanProactiveText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return -1
		case unicode.IsControl(r), unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxProactiveTextRunes {
		s = strings.TrimSpace(string(r[:MaxProactiveTextRunes]))
	}
	return s
}

// normalizeDedupKey lower-cases the key, turns whitespace into '_', drops any
// character outside [a-z0-9:_-] and cuts the result to MaxProactiveDedupKeyLen.
func normalizeDedupKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ':', r == '_', r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('_')
		}
		if b.Len() >= MaxProactiveDedupKeyLen {
			break
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
