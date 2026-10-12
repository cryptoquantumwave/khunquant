package agent

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/skills"
)

// PromptMode selects how much of the workspace the system prompt carries.
type PromptMode int

const (
	// PromptFull embeds the full text of every skill. It is what user turns
	// get, and its output is byte-identical to what the builder produced before
	// PromptLight existed.
	PromptFull PromptMode = iota
	// PromptLight lists skills by name and description and leaves their bodies
	// to the load_skill tool, and caps memory. It is for background turns
	// (sub-turns, proactive turns) where the full prompt is mostly dead weight.
	PromptLight
	// PromptProactive is for proactive turns (a governed heartbeat, a wake-up
	// about candidates). It is a light prompt cut down further: no guidance on
	// replying to a person, formatting for chat, writing memory or discovering
	// tools, no bootstrap file that only matters in conversation, and a skills
	// index of one line per skill (only when load_skill is offered). See
	// buildProactiveSystemPrompt.
	PromptProactive
)

// DefaultLightMemoryCapBytes is the memory budget of a light prompt when the
// caller does not set one.
const DefaultLightMemoryCapBytes = 4096

// maxPromptCacheEntries bounds the prompt cache. A builder sees a handful of
// distinct (mode, forced skills, memory cap) combinations in practice; the
// bound only stops a caller that varies ForcedSkills per message from growing
// the map without limit.
const maxPromptCacheEntries = 8

// BuildOptions selects the system prompt a request is built with.
type BuildOptions struct {
	Mode PromptMode
	// ForcedSkills are skills to include in full in a light prompt. Names the
	// loader does not know are skipped with a warning. Ignored in PromptFull,
	// which already carries every skill.
	ForcedSkills []string
	// MemoryCapBytes caps the # Memory section of a light prompt (0 means
	// DefaultLightMemoryCapBytes). Ignored in PromptFull.
	MemoryCapBytes int
	// OfferLoadSkill says the turn has the load_skill tool. Only PromptProactive
	// reads it: with the tool, the prompt lists skills by name and description;
	// without it a listing would be a list of things the turn cannot open, so
	// the listing is left out.
	OfferLoadSkill bool
}

// promptCacheKey identifies one cached system prompt. ForcedSkills is the
// sorted, de-duplicated list joined with a NUL, so equal sets share an entry.
type promptCacheKey struct {
	mode   PromptMode
	forced string
	memCap int
	skills bool // OfferLoadSkill, PromptProactive only
}

// normalize reduces options to the fields that affect the prompt for the mode,
// and returns the canonical (sorted, de-duplicated) forced-skill list.
func (o BuildOptions) normalize() (BuildOptions, promptCacheKey) {
	if o.Mode != PromptLight && o.Mode != PromptProactive {
		// A full prompt carries every skill and uncapped memory already, so
		// the other fields cannot change it: do not let them split the cache.
		return BuildOptions{Mode: PromptFull}, promptCacheKey{mode: PromptFull}
	}

	forced := make([]string, 0, len(o.ForcedSkills))
	for _, name := range o.ForcedSkills {
		if name = strings.TrimSpace(name); name != "" {
			forced = append(forced, name)
		}
	}
	slices.Sort(forced)
	forced = slices.Compact(forced)

	memCap := o.MemoryCapBytes
	if memCap <= 0 {
		memCap = DefaultLightMemoryCapBytes
	}

	norm := BuildOptions{Mode: o.Mode, ForcedSkills: forced, MemoryCapBytes: memCap}
	key := promptCacheKey{mode: o.Mode, forced: strings.Join(forced, "\x00"), memCap: memCap}
	if o.Mode == PromptProactive {
		norm.OfferLoadSkill = o.OfferLoadSkill
		key.skills = o.OfferLoadSkill
	}
	return norm, key
}

// promptCacheEntry is one cached prompt together with the baseline it was
// built against. Staleness is judged per entry because entries are built at
// different times.
type promptCacheEntry struct {
	prompt string
	// cachedAt is the max observed mtime across tracked paths at build time.
	cachedAt time.Time
	// existedAtCache tracks which source file paths existed when the entry was
	// built. This lets staleness detection see files that are newly created
	// (absent then, present now) or deleted (present then, gone now) — both of
	// which should trigger a rebuild.
	existedAtCache map[string]bool
	// skillFilesAtCache snapshots the skill tree file set and mtimes at build
	// time. This catches nested file creations/deletions/mtime changes that
	// may not update the top-level skill root directory mtime.
	skillFilesAtCache map[string]time.Time
	// day is the calendar day the entry was built on. The daily notes in the
	// prompt are "the last three days", so the window moves at midnight even
	// when no file changes.
	day string
	// seq orders builds, oldest first, for eviction.
	seq uint64
}

// BuildSystemPromptFor returns the system prompt for the options, from the
// cache when the workspace sources have not changed since it was built.
// Source file changes are detected via mtime checks (cheap stat calls).
func (cb *ContextBuilder) BuildSystemPromptFor(o BuildOptions) string {
	norm, key := o.normalize()

	// Try read lock first — fast path when the entry is valid.
	cb.systemPromptMutex.RLock()
	if e := cb.promptCache[key]; e != nil && e.prompt != "" && !cb.entryStaleLocked(e) {
		result := e.prompt
		cb.systemPromptMutex.RUnlock()
		return result
	}
	cb.systemPromptMutex.RUnlock()

	// Acquire write lock for building.
	cb.systemPromptMutex.Lock()
	defer cb.systemPromptMutex.Unlock()

	// Double-check: another goroutine may have rebuilt while we waited.
	if e := cb.promptCache[key]; e != nil && e.prompt != "" && !cb.entryStaleLocked(e) {
		return e.prompt
	}

	// Snapshot the baseline (existence + max mtime) BEFORE building the prompt.
	// This way cachedAt reflects the pre-build state: if a file is modified
	// during the build, its new mtime will be > baseline.maxMtime, so the next
	// staleness check will correctly trigger a rebuild. The alternative
	// (baseline after build) risks caching stale content with a too-new
	// baseline, making the staleness invisible.
	baseline := cb.buildCacheBaseline()
	var prompt string
	switch norm.Mode {
	case PromptLight:
		prompt = cb.buildLightSystemPrompt(norm)
	case PromptProactive:
		prompt = cb.buildProactiveSystemPrompt(norm)
	default:
		prompt = cb.BuildSystemPrompt()
	}

	if cb.promptCache == nil {
		cb.promptCache = make(map[promptCacheKey]*promptCacheEntry, maxPromptCacheEntries)
	}
	if _, replacing := cb.promptCache[key]; !replacing {
		cb.evictOldestLocked(maxPromptCacheEntries - 1)
	}
	cb.promptCacheSeq++
	cb.promptCache[key] = &promptCacheEntry{
		prompt:            prompt,
		cachedAt:          baseline.maxMtime,
		existedAtCache:    baseline.existed,
		skillFilesAtCache: baseline.skillFiles,
		day:               baseline.day,
		seq:               cb.promptCacheSeq,
	}

	logger.DebugCF("agent", "System prompt cached",
		map[string]any{
			"length":  len(prompt),
			"mode":    int(norm.Mode),
			"entries": len(cb.promptCache),
		})

	return prompt
}

// BuildSystemPromptWithCache returns the cached full system prompt if
// available and source files haven't changed, otherwise builds and caches it.
func (cb *ContextBuilder) BuildSystemPromptWithCache() string {
	return cb.BuildSystemPromptFor(BuildOptions{Mode: PromptFull})
}

// evictOldestLocked drops the oldest entries until at most keep remain.
// The caller must hold the write lock.
func (cb *ContextBuilder) evictOldestLocked(keep int) {
	for len(cb.promptCache) > keep {
		var oldestKey promptCacheKey
		var oldestSeq uint64
		found := false
		for k, e := range cb.promptCache {
			if !found || e.seq < oldestSeq {
				oldestKey, oldestSeq, found = k, e.seq, true
			}
		}
		if !found {
			return
		}
		delete(cb.promptCache, oldestKey)
	}
}

// buildLightSystemPrompt assembles the light prompt. Per section:
//
//   - identity: same text as the full prompt (workspace layout, the "always use
//     tools" rules, the tool-discovery rule when enabled);
//   - bootstrap files: same text as the full prompt;
//   - skills: an index (name + description) and a pointer to load_skill, plus
//     any forced skills in full — never the other skills' bodies;
//   - memory: capped, newest content kept;
//   - multi-message output: same text as the full prompt, when enabled.
//
// Per-request content (time, session, financial summary, conversation summary)
// is added by BuildMessagesWithOptions for both modes, so it is not here.
func (cb *ContextBuilder) buildLightSystemPrompt(o BuildOptions) string {
	parts := []string{cb.getIdentity()}

	if bootstrap := cb.LoadBootstrapFiles(); bootstrap != "" {
		parts = append(parts, bootstrap)
	}

	if cb.skillsLoader != nil {
		if index := cb.skillsLoader.BuildSkillsSummary(); index != "" {
			parts = append(parts, "# Skills (index)\n\n"+index+
				"\n\nCall `load_skill` with a skill name before relying on it.")
		}
		if forced := cb.forcedSkillsContent(o.ForcedSkills); forced != "" {
			parts = append(parts, "# Skills (loaded)\n\n"+forced)
		}
	}

	if memory := capTail(cb.memory.GetMemoryContext(), o.MemoryCapBytes); memory != "" {
		parts = append(parts, "# Memory\n\n"+memory)
	}

	if cb.splitOnMarker {
		parts = append(parts, multiMessageSection())
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// forcedSkillsContent renders the named skills in full through the loader's
// context path. A name the loader does not list (including any name carrying a
// path separator, which no listed skill can) is skipped with a warning.
func (cb *ContextBuilder) forcedSkillsContent(names []string) string {
	if len(names) == 0 {
		return ""
	}
	known := make(map[string]struct{})
	for _, s := range cb.skillsLoader.ListSkills() {
		known[s.Name] = struct{}{}
	}
	valid := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := known[name]; !ok {
			logger.WarnCF("agent", "Forced skill is not installed; skipping",
				map[string]any{"skill": name})
			continue
		}
		valid = append(valid, name)
	}
	return cb.skillsLoader.LoadSkillsForContext(valid)
}

// multiMessageSection is the split-marker section shared by both modes.
func multiMessageSection() string {
	return "# Multi-Message Output\n\n" +
		"You may insert " + channels.MessageSplitMarker + " where a response divides " +
		"naturally into separate messages — for example distinct steps, or a " +
		"summary followed by detail. Each part is delivered as its own message.\n\n" +
		"Use it where the split helps the reader. A single coherent answer should " +
		"stay a single message."
}

// capTail limits s to at most maxBytes bytes, keeping the end. The cut is moved
// forward to a line boundary (falling back to a rune boundary for a single line
// longer than the budget), and a marker says how much was dropped. s is
// returned unchanged when it already fits.
func capTail(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}

	start := len(s) - maxBytes
	if s[start-1] != '\n' {
		// The cut landed mid-line: drop the partial line.
		if nl := strings.IndexByte(s[start:], '\n'); nl >= 0 && start+nl+1 < len(s) {
			start += nl + 1
		} else {
			// One line longer than the budget: cut on a rune boundary.
			for start < len(s) && !utf8.RuneStart(s[start]) {
				start++
			}
		}
	}

	return fmt.Sprintf("[memory truncated: showing the last %d of %d bytes]\n%s",
		len(s)-start, len(s), s[start:])
}

// EstimateSystemTokens estimates the token count of the full system message
// that would be sent to the LLM for a user turn. See EstimateSystemTokensFor.
func (cb *ContextBuilder) EstimateSystemTokens(summary string, activeSkills []string) int {
	return cb.EstimateSystemTokensFor(BuildOptions{Mode: PromptFull}, summary)
}

// EstimateSystemTokensFor estimates the token count of the system message
// BuildMessagesWithOptions would send for the options, mirroring its
// composition: static prompt, dynamic context, and the summary with its
// wrapping prefix and separators. This avoids needing the per-request
// parameters BuildMessages takes (media, channel, chatID, sender, etc.).
func (cb *ContextBuilder) EstimateSystemTokensFor(o BuildOptions, summary string) int {
	staticPrompt := cb.BuildSystemPromptFor(o)

	// Dynamic context is small and varies per request; use a representative estimate.
	// Actual buildDynamicContext produces ~200-400 chars of time/runtime/session info.
	const dynamicContextChars = 800

	totalChars := utf8.RuneCountInString(staticPrompt) + dynamicContextChars
	if (o.Mode == PromptLight || o.Mode == PromptProactive) && cb.background.attached() {
		totalChars += backgroundSectionChars // the section a background turn carries, at its cap
	}

	// The full-mode estimate has always added the installed skills' text a
	// second time on top of the static prompt (which already embeds it), and is
	// kept as it was so user-turn numbers do not move. A light or proactive
	// prompt carries no skill bodies, so it adds nothing here.
	if o.Mode != PromptLight && o.Mode != PromptProactive {
		if skillsText := cb.buildActiveSkillsContext(nil); skillsText != "" {
			totalChars += utf8.RuneCountInString(skillsText)
			totalChars += 7 // separator \n\n---\n\n
		}
	}

	if summary != "" {
		// Matches the CONTEXT_SUMMARY: prefix added in BuildMessages
		const summaryPrefix = "CONTEXT_SUMMARY: The following is an approximate summary of prior conversation " +
			"for reference only. It may be incomplete or outdated — always defer to explicit instructions.\n\n"
		totalChars += utf8.RuneCountInString(summaryPrefix) + utf8.RuneCountInString(summary)
		totalChars += 7 // separator
	}

	return totalChars * 2 / 5 // same heuristic as tokenizer.EstimateMessageTokens
}

// buildActiveSkillsContext returns the full installed skills content (the
// full-mode estimate's skills term).
func (cb *ContextBuilder) buildActiveSkillsContext(_ []string) string {
	if cb.skillsLoader == nil {
		return ""
	}
	return cb.skillsLoader.BuildSkillsFullContent()
}

// SkillsLoader returns the loader the builder reads skills through, so the
// load_skill tool resolves names against exactly the roots the prompt lists.
func (cb *ContextBuilder) SkillsLoader() *skills.SkillsLoader {
	return cb.skillsLoader
}

// InvalidateCache clears every cached system prompt.
// Normally not needed because the cache auto-invalidates via mtime checks,
// but this is useful for tests or explicit reload commands.
func (cb *ContextBuilder) InvalidateCache() {
	cb.systemPromptMutex.Lock()
	defer cb.systemPromptMutex.Unlock()

	cb.promptCache = nil

	logger.DebugCF("agent", "System prompt cache invalidated", nil)
}

// promptDay is the calendar-day stamp compared against promptCacheEntry.day.
func promptDay(t time.Time) string {
	return t.Format("20060102")
}

// sourceFilesChangedLocked reports whether the cached full (user-turn) prompt is
// stale. It is entryStaleLocked for the default full entry, which is the one
// the cache tests exercise; a missing entry counts as stale.
func (cb *ContextBuilder) sourceFilesChangedLocked() bool {
	return cb.entryStaleLocked(cb.promptCache[promptCacheKey{mode: PromptFull}])
}

// entryStaleLocked checks whether any workspace source file has been
// modified, created, or deleted since the entry was built.
//
// IMPORTANT: The caller MUST hold at least a read lock on systemPromptMutex.
// Go's sync.RWMutex is not reentrant, so this function must NOT acquire the
// lock itself (it would deadlock when called from BuildSystemPromptFor
// which already holds RLock or Lock).
func (cb *ContextBuilder) entryStaleLocked(e *promptCacheEntry) bool {
	if e == nil || e.cachedAt.IsZero() {
		return true
	}

	// The daily-note window ("today and the two days before") moved.
	if e.day != promptDay(time.Now()) {
		return true
	}

	// Check tracked source files (bootstrap + memory + daily notes).
	if slices.ContainsFunc(cb.sourcePaths(), func(p string) bool { return fileChangedSince(e, p) }) {
		return true
	}

	// --- Skill roots (workspace/global/builtin) ---
	//
	// For each root:
	// 1. Creation/deletion and root directory mtime changes are tracked by fileChangedSince.
	// 2. Nested file create/delete/mtime changes are tracked by the skill file snapshot.
	for _, root := range cb.skillRoots() {
		if fileChangedSince(e, root) {
			return true
		}
	}
	if skillFilesChangedSince(cb.skillRoots(), e.skillFilesAtCache) {
		return true
	}

	return false
}

// fileChangedSince returns true if a tracked source file has been modified,
// newly created, or deleted since the entry was built.
//
// Four cases:
//   - existed at cache time, exists now -> check mtime
//   - existed at cache time, gone now   -> changed (deleted)
//   - absent at cache time,  exists now -> changed (created)
//   - absent at cache time,  gone now   -> no change
func fileChangedSince(e *promptCacheEntry, path string) bool {
	// Defensive: if existedAtCache was never initialized, treat as changed
	// so the cache rebuilds rather than silently serving stale data.
	if e.existedAtCache == nil {
		return true
	}

	existedBefore := e.existedAtCache[path]
	info, err := os.Stat(path)
	existsNow := err == nil

	if existedBefore != existsNow {
		return true // file was created or deleted
	}
	if !existsNow {
		return false // didn't exist before, doesn't exist now
	}
	return info.ModTime().After(e.cachedAt)
}
