package agent

import (
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// proactiveBootstrapFiles are the workspace files a proactive turn is given:
// who the owner is (USER.md) and the voice to write in (SOUL.md). AGENTS.md
// (rules for replying to a person, confirming trades, formatting numbers in
// chat) and IDENTITY.md (what the product is, with links) only matter in a
// conversation, and the turn cannot trade, message or write. The owner's
// structured profile reaches the turn through the financial-context line of the
// dynamic context, which every mode carries.
//
// Standing rules that must reach a proactive turn therefore belong in SOUL.md or
// USER.md (or the owner's HEARTBEAT.md), not AGENTS.md. The rules for what the
// turn may SUGGEST (risk tolerance, no leverage for a conservative owner, no
// acting on one unverified source) are not left to AGENTS.md: the built-in
// contract, prompts/proactive.md, carries them.
var proactiveBootstrapFiles = []string{"SOUL.md", "USER.md"}

// maxProactiveSkillDescRunes caps one skill's description in the proactive
// index, which lists one line per skill. The full description is what
// load_skill and the skill's own file carry.
const maxProactiveSkillDescRunes = 110

// buildProactiveSystemPrompt assembles the system prompt of a proactive turn.
// Per section, against the light prompt:
//
//   - identity: three lines - who it is, that this is a read-only wake-up, and
//     the rule to fetch rather than recall. The workspace layout, the memory
//     and notes paths, the "explain what you are doing" and "update MEMORY.md"
//     rules and the tool-discovery rule are for a conversation (and the
//     discovery tools are outside a proactive turn's ceiling);
//   - bootstrap files: proactiveBootstrapFiles only;
//   - skills: "- name: description" lines and no locations, only when the turn
//     has load_skill (o.OfferLoadSkill); forced skills in full either way;
//   - memory: capped, as in the light prompt;
//   - multi-message output: left out, a proactive turn never replies.
//
// Per-request content (time, financial context with the profile line) is added
// by BuildMessagesWithOptions for every mode and is not here.
func (cb *ContextBuilder) buildProactiveSystemPrompt(o BuildOptions) string {
	parts := []string{proactiveIdentity()}

	if bootstrap := cb.loadBootstrapFiles(proactiveBootstrapFiles...); bootstrap != "" {
		parts = append(parts, bootstrap)
	}

	if cb.skillsLoader != nil {
		if o.OfferLoadSkill {
			if index := cb.proactiveSkillsIndex(); index != "" {
				parts = append(parts, "# Skills\n\nCall `load_skill` with a name before relying on one.\n"+index)
			}
		}
		if forced := cb.forcedSkillsContent(o.ForcedSkills); forced != "" {
			parts = append(parts, "# Skills (loaded)\n\n"+forced)
		}
	}

	if memory := capTail(cb.memory.GetMemoryContext(), o.MemoryCapBytes); memory != "" {
		parts = append(parts, "# Memory\n\n"+memory)
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// proactiveIdentity is the identity section of a proactive prompt.
func proactiveIdentity() string {
	return fmt.Sprintf("# khunquant (%s)\n\n"+
		"You are khunquant, the owner's personal portfolio assistant, answering a wake-up the system started. "+
		"You can only read; the turn's instructions say how to answer.\n\n"+
		"Fetch current prices, balances and news with your tools; never answer from memory.",
		config.FormatVersion())
}

// proactiveSkillsIndex lists the skills as "- name: description", one line each.
func (cb *ContextBuilder) proactiveSkillsIndex() string {
	var b strings.Builder
	for _, s := range cb.skillsLoader.ListSkills() {
		desc := strings.Join(strings.Fields(s.Description), " ")
		if r := []rune(desc); len(r) > maxProactiveSkillDescRunes {
			desc = strings.TrimSpace(string(r[:maxProactiveSkillDescRunes-1])) + "…"
		}
		if desc == "" {
			fmt.Fprintf(&b, "- %s\n", s.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}
