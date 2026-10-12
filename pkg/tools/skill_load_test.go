package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/skills"
)

func writeSkill(t *testing.T, root, dir, description, body string) {
	t.Helper()
	path := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + dir + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillRoots returns three distinct roots (workspace skills, global, builtin)
// and a loader over them, the way the agent builds it.
func skillRoots(t *testing.T) (workspaceSkills, global, builtin string, loader *skills.SkillsLoader) {
	t.Helper()
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	global = filepath.Join(base, "home", "skills")
	builtin = filepath.Join(base, "builtin")
	for _, d := range []string{filepath.Join(workspace, "skills"), global, builtin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(workspace, "skills"), global, builtin, skills.NewSkillsLoader(workspace, global, builtin)
}

func TestLoadSkill_Workspace(t *testing.T) {
	ws, _, _, loader := skillRoots(t)
	writeSkill(t, ws, "carrots", "How to cook carrots.", "# Carrots\n\nBoil them.\n")

	res := NewLoadSkillTool(loader).Execute(context.Background(), map[string]any{"name": "carrots"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Boil them.") {
		t.Errorf("skill body missing from result: %q", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "description: How to cook carrots.") {
		t.Error("frontmatter leaked into the result")
	}
}

// TestLoadSkill_BuiltinOutsideWorkspace: global and builtin skills live outside
// the workspace, where read_file cannot go. load_skill reads them through the
// loader.
func TestLoadSkill_BuiltinOutsideWorkspace(t *testing.T) {
	_, global, builtin, loader := skillRoots(t)
	writeSkill(t, global, "globby", "A global skill.", "GLOBAL-BODY\n")
	writeSkill(t, builtin, "builtie", "A builtin skill.", "BUILTIN-BODY\n")

	tool := NewLoadSkillTool(loader)
	for name, want := range map[string]string{"globby": "GLOBAL-BODY", "builtie": "BUILTIN-BODY"} {
		res := tool.Execute(context.Background(), map[string]any{"name": name})
		if res.IsError || !strings.Contains(res.ForLLM, want) {
			t.Errorf("load_skill(%q) = error=%v %q, want %q", name, res.IsError, res.ForLLM, want)
		}
	}
}

func TestLoadSkill_UnknownListsNames(t *testing.T) {
	ws, global, builtin, loader := skillRoots(t)
	writeSkill(t, ws, "alpha", "a", "A\n")
	writeSkill(t, global, "beta", "b", "B\n")
	writeSkill(t, builtin, "gamma", "c", "C\n")

	res := NewLoadSkillTool(loader).Execute(context.Background(), map[string]any{"name": "delta"})
	if !res.IsError {
		t.Fatal("unknown skill should be an error")
	}
	for _, want := range []string{`"delta"`, "alpha", "beta", "gamma"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("error %q does not mention %s", res.ForLLM, want)
		}
	}

	// Matching is exact, not by prefix or case.
	for _, name := range []string{"alph", "Alpha", "alpha-2"} {
		if r := NewLoadSkillTool(loader).Execute(context.Background(), map[string]any{"name": name}); !r.IsError {
			t.Errorf("load_skill(%q) matched a skill it does not name exactly", name)
		}
	}
}

func TestLoadSkill_NoSkillsInstalled(t *testing.T) {
	_, _, _, loader := skillRoots(t)
	res := NewLoadSkillTool(loader).Execute(context.Background(), map[string]any{"name": "x"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no skills are installed") {
		t.Errorf("got %v %q", res.IsError, res.ForLLM)
	}
	if r := NewLoadSkillTool(nil).Execute(context.Background(), map[string]any{"name": "x"}); !r.IsError {
		t.Error("a tool with no loader must error, not panic")
	}
}

func TestLoadSkill_RejectsPathTraversal(t *testing.T) {
	ws, _, _, loader := skillRoots(t)
	writeSkill(t, ws, "real", "r", "REAL\n")

	// A SKILL.md reachable only by walking out of the skills root.
	secret := filepath.Join(filepath.Dir(ws), "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "SKILL.md"), []byte("TOP-SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A skill that exists, addressed through a path.
	tool := NewLoadSkillTool(loader)
	for _, name := range []string{
		"../secret", "..", "../../etc", "real/../real", "real/", "./real", "/etc/passwd",
		`..\secret`, `real\..\real`, "real\x00", "sub/real", "..secret", "a..b",
	} {
		res := tool.Execute(context.Background(), map[string]any{"name": name})
		if !res.IsError {
			t.Errorf("load_skill(%q) should be rejected", name)
		}
		if strings.Contains(res.ForLLM, "TOP-SECRET") || strings.Contains(res.ForLLM, "REAL") {
			t.Errorf("load_skill(%q) returned file content: %q", name, res.ForLLM)
		}
	}

	// Missing / wrong-typed names.
	for _, args := range []map[string]any{nil, {}, {"name": ""}, {"name": "   "}, {"name": 5}} {
		if res := tool.Execute(context.Background(), args); !res.IsError {
			t.Errorf("args %v should be rejected", args)
		}
	}
}

func TestLoadSkill_Cap64KB(t *testing.T) {
	ws, _, _, loader := skillRoots(t)

	var big strings.Builder
	for i := 0; big.Len() < 3*MaxLoadedSkillBytes; i++ {
		big.WriteString("line of skill text number ")
		big.WriteString(strings.Repeat("x", i%40))
		big.WriteString("\n")
	}
	writeSkill(t, ws, "huge", "h", big.String())
	writeSkill(t, ws, "small", "s", "tiny body\n")

	tool := NewLoadSkillTool(loader)
	res := tool.Execute(context.Background(), map[string]any{"name": "huge"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	body, marker, ok := strings.Cut(res.ForLLM, "\n\n[truncated:")
	if !ok {
		t.Fatal("oversized skill carries no truncation marker")
	}
	if len(body) > MaxLoadedSkillBytes {
		t.Errorf("body is %d bytes, want <= %d", len(body), MaxLoadedSkillBytes)
	}
	if big.String()[len(body)] != '\n' {
		t.Error("truncation did not land on a line boundary")
	}
	if !strings.Contains(marker, "omitted") {
		t.Errorf("marker does not say what was dropped: %q", marker)
	}

	// A skill under the cap is returned whole.
	small := tool.Execute(context.Background(), map[string]any{"name": "small"})
	if small.IsError || strings.Contains(small.ForLLM, "truncated") || !strings.Contains(small.ForLLM, "tiny body") {
		t.Errorf("small skill altered: %q", small.ForLLM)
	}
}

func TestCapSkillContent_SingleLongLineStaysValidUTF8(t *testing.T) {
	long := strings.Repeat("é", MaxLoadedSkillBytes) // 2 bytes each, no newline
	got := capSkillContent(long)
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a multi-byte rune")
	}
	if !strings.Contains(got, "[truncated:") {
		t.Fatal("no truncation marker")
	}
}

func TestLoadSkillTool_Definition(t *testing.T) {
	tool := NewLoadSkillTool(nil)
	if tool.Name() != "load_skill" || tool.Name() != NameLoadSkill {
		t.Errorf("Name() = %q", tool.Name())
	}
	params := tool.Parameters()
	required, _ := params["required"].([]string)
	if len(required) != 1 || required[0] != "name" {
		t.Errorf("required = %v, want [name]", required)
	}
	if ClassOf(NameLoadSkill) != ClassRead {
		t.Errorf("load_skill is %v, want ClassRead", ClassOf(NameLoadSkill))
	}
}
