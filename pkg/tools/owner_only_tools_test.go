package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/skills"
)

const (
	cronRefusal  = "scheduled jobs are changed only in the owner's own conversation"
	skillRefusal = "skills are changed only in the owner's own conversation"
)

// ownerOnlyTurns are the turns of the owner-only tables: the ones the owner changes
// things from, and the ones that are refused while an owner is configured.
func ownerOnlyTurns() (allowed, refused map[string]context.Context) {
	owner := remoteTurn(proposeOwner, "telegram")
	allowed = map[string]context.Context{
		"owner root turn": owner,
		"web owner":       webTurn(true),
		"cli":             WithToolContext(WithToolOrigin(context.Background(), OriginInternal), cliChannel, "direct"),
	}
	refused = map[string]context.Context{
		"another sender":         remoteTurn("telegram:999", "telegram"),
		"owner sub-turn":         WithSubTurn(owner),
		"web, not authenticated": webTurn(false),
		"untagged":               context.Background(),
		"system turn":            WithToolContext(WithToolOrigin(context.Background(), OriginInternal), "telegram", "1001"),
	}
	return allowed, refused
}

// jobsSnapshot is the whole scheduler store, for "the store is unchanged".
func jobsSnapshot(t *testing.T, tool *CronTool) string {
	t.Helper()
	b, err := json.Marshal(tool.cronService.ListJobs(true))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedJob puts one job in the store that the turn carried by ctx can reach.
func seedJob(t *testing.T, tool *CronTool, ctx context.Context) string {
	t.Helper()
	ch, chat := ToolChannel(ctx), ToolChatID(ctx)
	if ch == "" {
		ch, chat = "telegram", "chat"
	}
	job, err := tool.cronService.AddJob("seed", cron.CronSchedule{Kind: "every", EveryMS: new(int64)}, "seed message", false, ch, chat)
	if err != nil {
		t.Fatal(err)
	}
	return job.ID
}

func mutatingCronArgs(jobID string) map[string]map[string]any {
	return map[string]map[string]any{
		"add":     {"action": "add", "message": "remind me", "at_seconds": float64(600)},
		"update":  {"action": "update", "job_id": jobID, "message": "changed"},
		"remove":  {"action": "remove", "job_id": jobID},
		"enable":  {"action": "enable", "job_id": jobID},
		"disable": {"action": "disable", "job_id": jobID},
	}
}

func TestCronTool_MutationsAreOwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerOnlyTurns()
	actions := []string{"add", "update", "remove", "enable", "disable"}

	for name, ctx := range allowed {
		for _, action := range actions {
			t.Run("allowed/"+name+"/"+action, func(t *testing.T) {
				tool := newTestCronTool(t)
				id := seedJob(t, tool, ctx)
				res := tool.Execute(ctx, mutatingCronArgs(id)[action])
				if res.IsError {
					t.Fatalf("%s refused to the owner: %s", action, res.ForLLM)
				}
			})
		}
	}

	for name, ctx := range refused {
		for _, action := range actions {
			t.Run("refused/"+name+"/"+action, func(t *testing.T) {
				tool := newTestCronTool(t)
				id := seedJob(t, tool, ctx)
				before := jobsSnapshot(t, tool)
				res := tool.Execute(ctx, mutatingCronArgs(id)[action])
				if !res.IsError || !strings.Contains(res.ForLLM, cronRefusal) {
					t.Fatalf("%s was not refused: %+v", action, res)
				}
				if after := jobsSnapshot(t, tool); after != before {
					t.Fatalf("the store changed:\nbefore %s\nafter  %s", before, after)
				}
			})
		}
	}
}

func TestCronTool_ListAndGetStayOpen(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerOnlyTurns()
	for _, turns := range []map[string]context.Context{allowed, refused} {
		for name, ctx := range turns {
			t.Run(name, func(t *testing.T) {
				tool := newTestCronTool(t)
				id := seedJob(t, tool, ctx)
				if res := tool.Execute(ctx, map[string]any{"action": "list"}); res.IsError {
					t.Fatalf("list refused: %s", res.ForLLM)
				}
				// get needs a channel to find the job; an untagged turn has none, which
				// is the job-access rule and not this one.
				if ToolChannel(ctx) == "" {
					return
				}
				res := tool.Execute(ctx, map[string]any{"action": "get", "job_id": id})
				if strings.Contains(res.ForLLM, cronRefusal) {
					t.Fatalf("get refused by the owner rule: %s", res.ForLLM)
				}
			})
		}
	}
}

func TestCronTool_NoOwnerConfigured_NothingChanges(t *testing.T) {
	armOwner(t, false)
	ctx := remoteTurn("telegram:999", "telegram")
	for _, action := range []string{"add", "update", "remove", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			tool := newTestCronTool(t)
			id := seedJob(t, tool, ctx)
			if res := tool.Execute(ctx, mutatingCronArgs(id)[action]); res.IsError {
				t.Fatalf("%s refused with no owner configured: %s", action, res.ForLLM)
			}
		})
	}
}

// fakeSkillRegistry installs a SKILL.md and nothing else: no network.
type fakeSkillRegistry struct{}

func (fakeSkillRegistry) Name() string { return "fake" }
func (fakeSkillRegistry) Search(context.Context, string, int) ([]skills.SearchResult, error) {
	return nil, nil
}

func (fakeSkillRegistry) GetSkillMeta(context.Context, string) (*skills.SkillMeta, error) {
	return &skills.SkillMeta{}, nil
}

func (fakeSkillRegistry) DownloadAndInstall(_ context.Context, _, _, targetDir string) (*skills.InstallResult, error) {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("# skill\n"), 0o644); err != nil {
		return nil, err
	}
	return &skills.InstallResult{Version: "1.0.0"}, nil
}

func newFakeInstallTool(t *testing.T) (*InstallSkillTool, string) {
	t.Helper()
	rm := skills.NewRegistryManager()
	rm.AddRegistry(fakeSkillRegistry{})
	ws := t.TempDir()
	return NewInstallSkillTool(rm, ws), ws
}

func TestInstallSkill_OwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerOnlyTurns()
	args := map[string]any{"slug": "demo", "registry": "fake"}

	for name, ctx := range allowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			tool, ws := newFakeInstallTool(t)
			if res := tool.Execute(ctx, args); res.IsError {
				t.Fatalf("refused to the owner: %s", res.ForLLM)
			}
			if _, err := os.Stat(filepath.Join(ws, "skills", "demo", "SKILL.md")); err != nil {
				t.Fatalf("the skill was not installed: %v", err)
			}
		})
	}
	for name, ctx := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			tool, ws := newFakeInstallTool(t)
			res := tool.Execute(ctx, args)
			if !res.IsError || !strings.Contains(res.ForLLM, skillRefusal) {
				t.Fatalf("not refused: %+v", res)
			}
			if _, err := os.Stat(filepath.Join(ws, "skills")); !os.IsNotExist(err) {
				t.Fatalf("skills directory was written (stat err %v)", err)
			}
		})
	}
}

func TestInstallSkill_NoOwnerConfigured_NothingChanges(t *testing.T) {
	armOwner(t, false)
	tool, ws := newFakeInstallTool(t)
	res := tool.Execute(remoteTurn("telegram:999", "telegram"), map[string]any{"slug": "demo", "registry": "fake"})
	if res.IsError {
		t.Fatalf("refused with no owner configured: %s", res.ForLLM)
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "demo")); err != nil {
		t.Fatal(err)
	}
}
