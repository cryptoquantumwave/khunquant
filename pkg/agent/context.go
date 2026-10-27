package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/skills"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

type ContextBuilder struct {
	workspace          string
	skillsLoader       *skills.SkillsLoader
	memory             *MemoryStore
	toolDiscoveryBM25  bool
	toolDiscoveryRegex bool
	splitOnMarker      bool

	// Cache for system prompts to avoid rebuilding on every call.
	// This fixes issue #607: repeated reprocessing of the entire context.
	// Every entry auto-invalidates when workspace source files change (mtime
	// check). There is one entry per promptCacheKey (mode, forced skills, memory
	// cap), so a light background turn and a full user turn do not evict each
	// other. The map is bounded: see maxPromptCacheEntries.
	systemPromptMutex sync.RWMutex
	promptCache       map[promptCacheKey]*promptCacheEntry
	promptCacheSeq    uint64 // build counter, for oldest-first eviction

	financialCollector *FinancialContextCollector

	// background is the section only background turns carry (see
	// background_sections.go); nil or empty adds nothing.
	background *backgroundSections

	// clock, when set (SetClock), is the time of the prompt's "Current Time" and of the
	// prompt-cache day; nil is time.Now.
	clock atomic.Pointer[func() time.Time]
}

// SetClock makes the prompt read now instead of the wall clock. The "Current Time"
// section and the daily-note cache day both follow it, so they roll over at the same
// (simulated) midnight. A nil now restores time.Now.
func (cb *ContextBuilder) SetClock(now func() time.Time) {
	if now == nil {
		cb.clock.Store(nil)
		return
	}
	cb.clock.Store(&now)
}

func (cb *ContextBuilder) now() time.Time {
	if f := cb.clock.Load(); f != nil {
		return (*f)()
	}
	return time.Now()
}

func (cb *ContextBuilder) WithToolDiscovery(useBM25, useRegex bool) *ContextBuilder {
	cb.toolDiscoveryBM25 = useBM25
	cb.toolDiscoveryRegex = useRegex
	return cb
}

// WithSplitOnMarker tells the model it may mark message boundaries. Without
// this the marker is never emitted and the manager-side split is inert, so the
// two halves of the feature have to be enabled together.
func (cb *ContextBuilder) WithSplitOnMarker(enabled bool) *ContextBuilder {
	cb.splitOnMarker = enabled
	return cb
}

func (cb *ContextBuilder) WithFinancialCollector(c *FinancialContextCollector) *ContextBuilder {
	cb.financialCollector = c
	return cb
}

// WithBackgroundSections shares the agent loop's background-section holder with
// the builder. The holder is read through an atomic pointer, so attaching it while
// turns are running is safe.
func (cb *ContextBuilder) WithBackgroundSections(b *backgroundSections) *ContextBuilder {
	cb.background = b
	return cb
}

func getGlobalConfigDir() string {
	if home := os.Getenv("KHUNQUANT_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".khunquant")
}

func NewContextBuilder(workspace string) *ContextBuilder {
	// builtin skills: skills directory in current project
	// Use the skills/ directory under the current working directory
	builtinSkillsDir := strings.TrimSpace(os.Getenv("KHUNQUANT_BUILTIN_SKILLS"))
	if builtinSkillsDir == "" {
		wd, _ := os.Getwd()
		builtinSkillsDir = filepath.Join(wd, "skills")
	}
	globalSkillsDir := filepath.Join(getGlobalConfigDir(), "skills")

	return &ContextBuilder{
		workspace:    workspace,
		skillsLoader: skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir),
		memory:       NewMemoryStore(workspace),
	}
}

func (cb *ContextBuilder) getIdentity() string {
	workspacePath, _ := filepath.Abs(filepath.Join(cb.workspace))
	toolDiscovery := cb.getDiscoveryRule()
	version := config.FormatVersion()

	return fmt.Sprintf(
		`# khunquant 🦞 (%s)

You are khunquant, a helpful AI assistant.

## Workspace
Your workspace is at: %s
- Memory: %s/memory/MEMORY.md
- Daily Notes: %s/memory/YYYYMM/YYYYMMDD.md
- Skills: %s/skills/{skill-name}/SKILL.md

## Important Rules

1. **ALWAYS use tools** - When you need to perform an action (schedule reminders, send messages, execute commands, etc.), you MUST call the appropriate tool. Do NOT just say you'll do it or pretend to do it.

2. **Be helpful and accurate** - When using tools, briefly explain what you're doing.

3. **Memory** - When interacting with me if something seems memorable, update %s/memory/MEMORY.md

4. **Context summaries** - Conversation summaries provided as context are approximate references only. They may be incomplete or outdated. Always defer to explicit user instructions over summary content.

%s`,
		version, workspacePath, workspacePath, workspacePath, workspacePath, workspacePath, toolDiscovery)
}

func (cb *ContextBuilder) getDiscoveryRule() string {
	if !cb.toolDiscoveryBM25 && !cb.toolDiscoveryRegex {
		return ""
	}

	var toolNames []string
	if cb.toolDiscoveryBM25 {
		toolNames = append(toolNames, `"tool_search_tool_bm25"`)
	}
	if cb.toolDiscoveryRegex {
		toolNames = append(toolNames, `"tool_search_tool_regex"`)
	}

	return fmt.Sprintf(
		`5. **Tool Discovery** - Your visible tools are limited to save memory, but a vast hidden library exists. If you lack the right tool for a task, BEFORE giving up, you MUST search using the %s tool. Do not refuse a request unless the search returns nothing. Found tools will temporarily unlock for your next turn.`,
		strings.Join(toolNames, " or "),
	)
}

func (cb *ContextBuilder) BuildSystemPrompt() string {
	parts := []string{}

	// Core identity section
	parts = append(parts, cb.getIdentity())

	// Bootstrap files
	bootstrapContent := cb.LoadBootstrapFiles()
	if bootstrapContent != "" {
		parts = append(parts, bootstrapContent)
	}

	// Skills - embed full content so the LLM can act on iteration 1
	// without a prior read_file call.
	skillsContent := cb.skillsLoader.BuildSkillsFullContent()
	if skillsContent != "" {
		parts = append(parts, fmt.Sprintf("# Skills\n\n%s", skillsContent))
	}

	// Memory context
	memoryContext := cb.memory.GetMemoryContext()
	if memoryContext != "" {
		parts = append(parts, "# Memory\n\n"+memoryContext)
	}

	// Multi-message output, when enabled.
	//
	// Phrased as a capability rather than an instruction. Upstream's wording
	// ("You MUST frequently... NEVER output a single long wall of text") pushes
	// the model to fragment answers that are better whole; a marker is useful
	// when the content genuinely divides, and harmful when it does not.
	if cb.splitOnMarker {
		parts = append(parts, multiMessageSection())
	}

	// Join with "---" separator
	return strings.Join(parts, "\n\n---\n\n")
}

// sourcePaths returns non-skill workspace source files tracked for cache
// invalidation (bootstrap files + memory + the daily notes the memory section
// reads). Skill roots are handled separately because they require both
// directory-level and recursive file-level checks.
func (cb *ContextBuilder) sourcePaths() []string {
	paths := []string{
		filepath.Join(cb.workspace, "AGENTS.md"),
		filepath.Join(cb.workspace, "SOUL.md"),
		filepath.Join(cb.workspace, "USER.md"),
		filepath.Join(cb.workspace, "IDENTITY.md"),
		filepath.Join(cb.workspace, "memory", "MEMORY.md"),
	}
	// Daily notes (memory/YYYYMM/YYYYMMDD.md) are in the prompt too.
	if cb.memory != nil {
		paths = append(paths, cb.memory.recentDailyNotePaths()...)
	}
	return paths
}

// skillRoots returns all skill root directories that can affect
// BuildSkillsSummary output (workspace/global/builtin).
func (cb *ContextBuilder) skillRoots() []string {
	if cb.skillsLoader == nil {
		return []string{filepath.Join(cb.workspace, "skills")}
	}

	roots := cb.skillsLoader.SkillRoots()
	if len(roots) == 0 {
		return []string{filepath.Join(cb.workspace, "skills")}
	}
	return roots
}

// cacheBaseline holds the file existence snapshot and the latest observed
// mtime across all tracked paths. Used as the cache reference point.
type cacheBaseline struct {
	existed    map[string]bool
	skillFiles map[string]time.Time
	maxMtime   time.Time
	day        string // calendar day of the snapshot; see promptCacheEntry.day
}

// buildCacheBaseline records which tracked paths currently exist and computes
// the latest mtime across all tracked files + skills directory contents.
// Called under write lock when the cache is built.
func (cb *ContextBuilder) buildCacheBaseline() cacheBaseline {
	skillRoots := cb.skillRoots()

	// All paths whose existence we track: source files + all skill roots.
	allPaths := append(cb.sourcePaths(), skillRoots...)

	existed := make(map[string]bool, len(allPaths))
	skillFiles := make(map[string]time.Time)
	var maxMtime time.Time

	for _, p := range allPaths {
		info, err := os.Stat(p)
		existed[p] = err == nil
		if err == nil && info.ModTime().After(maxMtime) {
			maxMtime = info.ModTime()
		}
	}

	// Walk all skill roots recursively to snapshot skill files and mtimes.
	// Use os.Stat (not d.Info) for consistency with sourceFilesChanged checks.
	for _, root := range skillRoots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr == nil && !d.IsDir() {
				if info, err := os.Stat(path); err == nil {
					skillFiles[path] = info.ModTime()
					if info.ModTime().After(maxMtime) {
						maxMtime = info.ModTime()
					}
				}
			}
			return nil
		})
	}

	// If no tracked files exist yet (empty workspace), maxMtime is zero.
	// Use a very old non-zero time so that:
	// 1. cachedAt.IsZero() won't trigger perpetual rebuilds.
	// 2. Any real file created afterwards has mtime > cachedAt, so it
	//    will be detected by fileChangedSince (unlike time.Now() which
	//    could race with a file whose mtime <= Now).
	if maxMtime.IsZero() {
		maxMtime = time.Unix(1, 0)
	}

	return cacheBaseline{existed: existed, skillFiles: skillFiles, maxMtime: maxMtime, day: promptDay(cb.now())}
}

// errWalkStop is a sentinel error used to stop filepath.WalkDir early.
// Using a dedicated error (instead of fs.SkipAll) makes the early-exit
// intent explicit and avoids the nilerr linter warning that would fire
// if the callback returned nil when its err parameter is non-nil.
var errWalkStop = errors.New("walk stop")

// skillFilesChangedSince compares the current recursive skill file tree
// against the cache-time snapshot. Any create/delete/mtime drift invalidates
// the cache.
func skillFilesChangedSince(skillRoots []string, filesAtCache map[string]time.Time) bool {
	// Defensive: if the snapshot was never initialized, force rebuild.
	if filesAtCache == nil {
		return true
	}

	// Check cached files still exist and keep the same mtime.
	for path, cachedMtime := range filesAtCache {
		info, err := os.Stat(path)
		if err != nil {
			// A previously tracked file disappeared (or became inaccessible):
			// either way, cached skill summary may now be stale.
			return true
		}
		if !info.ModTime().Equal(cachedMtime) {
			return true
		}
	}

	// Check no new files appeared under any skill root.
	changed := false
	for _, root := range skillRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}

		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				// Treat unexpected walk errors as changed to avoid stale cache.
				if !os.IsNotExist(walkErr) {
					changed = true
					return errWalkStop
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if _, ok := filesAtCache[path]; !ok {
				changed = true
				return errWalkStop
			}
			return nil
		})

		if changed {
			return true
		}
		if err != nil && !errors.Is(err, errWalkStop) && !os.IsNotExist(err) {
			logger.DebugCF("agent", "skills walk error", map[string]any{"error": err.Error()})
			return true
		}
	}

	return false
}

func (cb *ContextBuilder) LoadBootstrapFiles() string {
	return cb.loadBootstrapFiles("AGENTS.md", "SOUL.md", "USER.md", "IDENTITY.md")
}

// loadBootstrapFiles renders the named workspace files, in the order given,
// each under a "## name" heading; a file that is missing is skipped.
func (cb *ContextBuilder) loadBootstrapFiles(bootstrapFiles ...string) string {
	var sb strings.Builder
	for _, filename := range bootstrapFiles {
		filePath := filepath.Join(cb.workspace, filename)
		if data, err := os.ReadFile(filePath); err == nil {
			fmt.Fprintf(&sb, "## %s\n\n%s\n\n", filename, data)
		}
	}

	return sb.String()
}

// buildDynamicContext returns a short dynamic context string with per-request info.
// This changes every request (time, session) so it is NOT part of the cached prompt.
// LLM-side KV cache reuse is achieved by each provider adapter's native mechanism:
//   - Anthropic: per-block cache_control (ephemeral) on the static SystemParts block
//   - OpenAI / Codex: prompt_cache_key for prefix-based caching
//
// See: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
// See: https://platform.openai.com/docs/guides/prompt-caching
func (cb *ContextBuilder) buildDynamicContext(channel, chatID string) string {
	return cb.buildDynamicContextFor(channel, chatID, false)
}

// buildDynamicContextFor is buildDynamicContext for a full prompt (compact false)
// or for a light or proactive one (compact true), which tells the financial
// context's live sections to keep to their smaller size.
func (cb *ContextBuilder) buildDynamicContextFor(channel, chatID string, compact bool) string {
	now := cb.now().Format("2006-01-02 15:04 (Monday)")
	rt := fmt.Sprintf("%s %s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version())

	var sb strings.Builder
	fmt.Fprintf(&sb, "## Current Time\n%s\n\n## Runtime\n%s", now, rt)

	if channel != "" && chatID != "" {
		fmt.Fprintf(&sb, "\n\n## Current Session\nChannel: %s\nChat ID: %s", channel, chatID)
	}

	if cb.financialCollector != nil {
		if fin := cb.financialCollector.GetSummaryFor(compact); fin != "" {
			fmt.Fprintf(&sb, "\n\n%s", fin)
		}
	}

	return sb.String()
}

// BuildMessages builds the request messages with the full system prompt, which
// is what user turns get. It is BuildMessagesWithOptions in PromptFull.
func (cb *ContextBuilder) BuildMessages(
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
	channel, chatID string,
) []providers.Message {
	return cb.BuildMessagesWithOptions(history, summary, currentMessage, media, channel, chatID,
		BuildOptions{Mode: PromptFull})
}

// BuildMessagesWithOptions builds the request messages with the system prompt
// the options select. Everything except the static prompt (dynamic context,
// summary, history, the current message) is the same in every mode.
func (cb *ContextBuilder) BuildMessagesWithOptions(
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
	channel, chatID string,
	o BuildOptions,
) []providers.Message {
	messages := []providers.Message{}

	// The static part (identity, bootstrap, skills, memory) is cached locally to
	// avoid repeated file I/O and string building on every call (fixes issue #607).
	// Dynamic parts (time, session, summary) are appended per request.
	// Everything is sent as a single system message for provider compatibility:
	// - Anthropic adapter extracts messages[0] (Role=="system") and maps its content
	//   to the top-level "system" parameter in the Messages API request. A single
	//   contiguous system block makes this extraction straightforward.
	// - Codex maps only the first system message to its instructions field.
	// - OpenAI-compat passes messages through as-is.
	staticPrompt := cb.BuildSystemPromptFor(o)

	// Build short dynamic context (time, runtime, session) — changes per request
	dynamicCtx := cb.buildDynamicContextFor(channel, chatID, o.Mode == PromptLight || o.Mode == PromptProactive)
	// Background turns, and only they, also carry the sections attached for them.
	// A user turn's dynamic context is exactly what it always was.
	if (o.Mode == PromptLight || o.Mode == PromptProactive) && !o.NoBackgroundSections {
		if extra := cb.background.render(context.Background()); extra != "" {
			dynamicCtx += "\n\n" + extra
		}
	}

	// Compose a single system message: static (cached) + dynamic + optional summary.
	// Keeping all system content in one message ensures every provider adapter can
	// extract it correctly (Anthropic adapter -> top-level system param,
	// Codex -> instructions field).
	//
	// SystemParts carries the same content as structured blocks so that
	// cache-aware adapters (Anthropic) can set per-block cache_control.
	// The static block is marked "ephemeral" — its prefix hash is stable
	// across requests, enabling LLM-side KV cache reuse.
	stringParts := []string{staticPrompt, dynamicCtx}

	contentBlocks := []providers.ContentBlock{
		{Type: "text", Text: staticPrompt, CacheControl: &providers.CacheControl{Type: "ephemeral"}},
		{Type: "text", Text: dynamicCtx},
	}

	if summary != "" {
		summaryText := fmt.Sprintf(
			"CONTEXT_SUMMARY: The following is an approximate summary of prior conversation "+
				"for reference only. It may be incomplete or outdated — always defer to explicit instructions.\n\n%s",
			summary)
		stringParts = append(stringParts, summaryText)
		contentBlocks = append(contentBlocks, providers.ContentBlock{Type: "text", Text: summaryText})
	}

	fullSystemPrompt := strings.Join(stringParts, "\n\n---\n\n")

	// Log system prompt summary for debugging (debug mode only).
	// Read the cache size under lock to avoid a data race with
	// concurrent InvalidateCache / BuildSystemPromptFor writes.
	cb.systemPromptMutex.RLock()
	isCached := len(cb.promptCache) > 0
	cb.systemPromptMutex.RUnlock()

	logger.DebugCF("agent", "System prompt built",
		map[string]any{
			"static_chars":  len(staticPrompt),
			"dynamic_chars": len(dynamicCtx),
			"total_chars":   len(fullSystemPrompt),
			"has_summary":   summary != "",
			"cached":        isCached,
			"mode":          int(o.Mode),
		})

	// Log preview of system prompt (avoid logging huge content)
	preview := utils.Truncate(fullSystemPrompt, 500)
	logger.DebugCF("agent", "System prompt preview",
		map[string]any{
			"preview": preview,
		})

	history = sanitizeHistoryForProvider(history)

	// Single system message containing all context — compatible with all providers.
	// SystemParts enables cache-aware adapters to set per-block cache_control;
	// Content is the concatenated fallback for adapters that don't read SystemParts.
	messages = append(messages, providers.Message{
		Role:        "system",
		Content:     fullSystemPrompt,
		SystemParts: contentBlocks,
	})

	// Add conversation history
	messages = append(messages, history...)

	// Add current user message
	if strings.TrimSpace(currentMessage) != "" {
		msg := providers.Message{
			Role:    "user",
			Content: currentMessage,
		}
		if len(media) > 0 {
			msg.Media = media
		}
		messages = append(messages, msg)
	}

	return messages
}

func sanitizeHistoryForProvider(history []providers.Message) []providers.Message {
	if len(history) == 0 {
		return history
	}

	sanitized := make([]providers.Message, 0, len(history))
	for _, msg := range history {
		switch msg.Role {
		case "system":
			// Drop system messages from history. BuildMessages always
			// constructs its own single system message (static + dynamic +
			// summary); extra system messages would break providers that
			// only accept one (Anthropic, Codex).
			logger.DebugCF("agent", "Dropping system message from history", map[string]any{})
			continue

		case "tool":
			if len(sanitized) == 0 {
				logger.DebugCF("agent", "Dropping orphaned leading tool message", map[string]any{})
				continue
			}
			// Walk backwards to find the nearest assistant message,
			// skipping over any preceding tool messages (multi-tool-call case).
			foundAssistant := false
			for i := len(sanitized) - 1; i >= 0; i-- {
				if sanitized[i].Role == "tool" {
					continue
				}
				if sanitized[i].Role == "assistant" && len(sanitized[i].ToolCalls) > 0 {
					foundAssistant = true
				}
				break
			}
			if !foundAssistant {
				logger.DebugCF("agent", "Dropping orphaned tool message", map[string]any{})
				continue
			}
			sanitized = append(sanitized, msg)

		case "assistant":
			if len(msg.ToolCalls) > 0 {
				if len(sanitized) == 0 {
					logger.DebugCF("agent", "Dropping assistant tool-call turn at history start", map[string]any{})
					continue
				}
				prev := sanitized[len(sanitized)-1]
				if prev.Role != "user" && prev.Role != "tool" {
					logger.DebugCF(
						"agent",
						"Dropping assistant tool-call turn with invalid predecessor",
						map[string]any{"prev_role": prev.Role},
					)
					continue
				}
			}
			sanitized = append(sanitized, msg)

		default:
			sanitized = append(sanitized, msg)
		}
	}

	// Second pass: ensure every assistant message with tool_calls has matching
	// tool result messages following it. This is required by strict providers
	// like DeepSeek that enforce: "An assistant message with 'tool_calls' must
	// be followed by tool messages responding to each 'tool_call_id'."
	//
	// Deduplication is scoped to the contiguous tool-result block that follows a
	// single assistant tool-call message. Some providers legitimately reuse call
	// IDs across separate turns (for example "call_0"), so global deduplication
	// would incorrectly delete later valid tool results and leave an
	// assistant(tool_calls) -> assistant sequence behind.
	final := make([]providers.Message, 0, len(sanitized))
	for i := 0; i < len(sanitized); i++ {
		msg := sanitized[i]

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			expected := make(map[string]bool, len(msg.ToolCalls))
			invalidToolCallID := false
			for _, tc := range msg.ToolCalls {
				if tc.ID == "" {
					invalidToolCallID = true
					continue
				}
				expected[tc.ID] = false
			}

			block := make([]providers.Message, 0, len(expected))
			seenInBlock := make(map[string]bool, len(expected))
			j := i + 1
			for ; j < len(sanitized); j++ {
				next := sanitized[j]
				if next.Role != "tool" {
					break
				}
				if next.ToolCallID == "" {
					logger.DebugCF("agent", "Dropping tool result without tool_call_id", map[string]any{})
					continue
				}
				if _, ok := expected[next.ToolCallID]; !ok {
					logger.DebugCF("agent", "Dropping unexpected tool result", map[string]any{
						"tool_call_id": next.ToolCallID,
					})
					continue
				}
				if seenInBlock[next.ToolCallID] {
					logger.DebugCF("agent", "Dropping duplicate tool result in tool block", map[string]any{
						"tool_call_id": next.ToolCallID,
					})
					continue
				}
				seenInBlock[next.ToolCallID] = true
				expected[next.ToolCallID] = true
				block = append(block, next)
			}

			allFound := !invalidToolCallID
			if invalidToolCallID {
				logger.DebugCF("agent", "Dropping assistant message with empty tool_call_id", map[string]any{})
			}
			for toolCallID, found := range expected {
				if !found {
					allFound = false
					logger.DebugCF(
						"agent",
						"Dropping assistant message with incomplete tool results",
						map[string]any{
							"missing_tool_call_id": toolCallID,
							"expected_count":       len(expected),
							"found_count":          len(block),
						},
					)
					break
				}
			}

			if !allFound {
				i = j - 1
				continue
			}

			final = append(final, msg)
			final = append(final, block...)
			i = j - 1
			continue
		}

		if msg.Role == "tool" {
			logger.DebugCF("agent", "Dropping orphaned tool message after validation", map[string]any{
				"tool_call_id": msg.ToolCallID,
			})
			continue
		}

		// A proactive message (pkg/notify) is appended to the session as an
		// assistant message and can follow another assistant message. Several
		// providers insist on alternating roles, so consecutive plain-text
		// assistant messages become one. A message that carries tool calls is
		// never merged: its tool results must stay paired with it.
		if n := len(final); n > 0 && mergeableAssistantText(final[n-1]) && mergeableAssistantText(msg) {
			final[n-1] = mergeAssistantText(final[n-1], msg)
			continue
		}

		final = append(final, msg)
	}

	// A session can open with a proactive assistant message (pkg/notify writes
	// it into a session that has no history yet). Chat APIs differ in how they
	// treat a conversation that does not start with a user turn: Anthropic
	// rejects it, Gemini-style backends can too. Fixing it here, once, keeps
	// every provider adapter out of it. A leading assistant message that
	// carries tool calls never gets here: the first pass drops it.
	if len(final) > 0 && final[0].Role == "assistant" && len(final[0].ToolCalls) == 0 {
		final = append([]providers.Message{{Role: "user", Content: providers.ConversationStartPlaceholder}}, final...)
	}

	return final
}

// mergeableAssistantText reports whether m is an assistant message that is
// nothing but text (no tool calls, no media), the only kind that may be merged
// with a neighbour.
func mergeableAssistantText(m providers.Message) bool {
	return m.Role == "assistant" && len(m.ToolCalls) == 0 && len(m.Media) == 0 && m.ToolCallID == ""
}

// mergeAssistantText joins two text-only assistant messages with a blank line.
func mergeAssistantText(a, b providers.Message) providers.Message {
	a.Content = joinNonEmpty(a.Content, b.Content)
	a.ReasoningContent = joinNonEmpty(a.ReasoningContent, b.ReasoningContent)
	return a
}

func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

func (cb *ContextBuilder) AddToolResult(
	messages []providers.Message,
	toolCallID, toolName, result string,
) []providers.Message {
	messages = append(messages, providers.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: toolCallID,
	})
	return messages
}

func (cb *ContextBuilder) AddAssistantMessage(
	messages []providers.Message,
	content string,
	toolCalls []map[string]any,
) []providers.Message {
	msg := providers.Message{
		Role:    "assistant",
		Content: content,
	}
	// Always add assistant message, whether or not it has tool calls
	messages = append(messages, msg)
	return messages
}

// GetSkillsInfo returns information about loaded skills.
func (cb *ContextBuilder) GetSkillsInfo() map[string]any {
	allSkills := cb.skillsLoader.ListSkills()
	skillNames := make([]string, 0, len(allSkills))
	for _, s := range allSkills {
		skillNames = append(skillNames, s.Name)
	}
	return map[string]any{
		"total":     len(allSkills),
		"available": len(allSkills),
		"names":     skillNames,
	}
}
