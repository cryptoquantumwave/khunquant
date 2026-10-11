package tools

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/skills"
)

// MaxLoadedSkillBytes caps what load_skill returns for one skill.
const MaxLoadedSkillBytes = 64 * 1024

// LoadSkillTool returns the full instructions of an installed skill. A light
// system prompt lists skills by name and description only; this is how the
// model reads the one it needs.
//
// It reads through the SkillsLoader — the same workspace, global and builtin
// roots the prompt's index was built from — and never through the filesystem
// tools, whose sandbox is the workspace and which must not be widened to reach
// the global and builtin roots.
type LoadSkillTool struct {
	loader *skills.SkillsLoader
}

// NewLoadSkillTool creates a LoadSkillTool over the loader the system prompt
// lists skills from.
func NewLoadSkillTool(loader *skills.SkillsLoader) *LoadSkillTool {
	return &LoadSkillTool{loader: loader}
}

func (t *LoadSkillTool) Name() string {
	return NameLoadSkill
}

func (t *LoadSkillTool) Description() string {
	return "Load the full instructions of an installed skill by its exact name. " +
		"The system prompt lists the available skills with a short description; " +
		"call this before relying on one. Returns the skill's SKILL.md content."
}

func (t *LoadSkillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Exact skill name from the skills index (e.g. 'market-analysis')",
			},
		},
		"required": []string{"name"},
	}
}

func (t *LoadSkillTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrorResult("name is required and must be a non-empty string")
	}
	// A skill name is one directory name. Refuse anything that could name a
	// path before the loader sees it: the loader joins the name onto a root.
	if strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, "..") {
		return ErrorResult(fmt.Sprintf("invalid skill name %q: a skill name has no path separators or '..'", name))
	}
	if t.loader == nil {
		return ErrorResult("no skills are available")
	}

	known := t.loader.ListSkills()
	found := false
	names := make([]string, 0, len(known))
	for _, s := range known {
		names = append(names, s.Name)
		if s.Name == name {
			found = true
		}
	}
	if !found {
		if len(names) == 0 {
			return ErrorResult(fmt.Sprintf("unknown skill %q: no skills are installed", name))
		}
		return ErrorResult(fmt.Sprintf("unknown skill %q. Available skills: %s", name, strings.Join(names, ", ")))
	}

	content, ok := t.loader.LoadSkill(name)
	if !ok {
		return ErrorResult(fmt.Sprintf("skill %q is listed but its SKILL.md could not be read", name))
	}

	return SilentResult(capSkillContent(content))
}

// capSkillContent limits content to MaxLoadedSkillBytes, cutting on a line
// boundary (or a rune boundary for one very long line) and saying so.
func capSkillContent(content string) string {
	if len(content) <= MaxLoadedSkillBytes {
		return content
	}

	cut := MaxLoadedSkillBytes
	if nl := strings.LastIndexByte(content[:cut], '\n'); nl > 0 {
		cut = nl
	} else {
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
	}

	return fmt.Sprintf("%s\n\n[truncated: this skill is %d bytes; showing the first %d bytes, %d omitted]",
		content[:cut], len(content), cut, len(content)-cut)
}
