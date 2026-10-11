package agent

import (
	"context"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// profileOpenTimeout bounds opening the database and migrating it at startup.
const profileOpenTimeout = 15 * time.Second

// DefaultAgentWorkspace is the workspace of the default ("main") agent: the one
// the shared proactive database lives under and the profile belongs to. It is
// derived from config alone, so every place that needs it (the tools, the
// prompt contributor, the gateway's database helper) agrees without a registry.
func DefaultAgentWorkspace(cfg *config.Config) string {
	defaults := &cfg.Agents.Defaults
	list := cfg.Agents.List
	if len(list) == 0 {
		return resolveAgentWorkspace(nil, defaults)
	}
	// Mirror AgentRegistry.GetDefaultAgent: "main" wins; otherwise the agent
	// marked default, otherwise the first listed.
	pick := &list[0]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Default {
			pick = &list[i]
		}
	}
	for i := range list {
		if routing.NormalizeAgentID(list[i].ID) == "main" {
			pick = &list[i]
			break
		}
	}
	return resolveAgentWorkspace(pick, defaults)
}

// openProfileStore opens the profile store on the default agent's workspace
// through the shared database handle, runs the intent migrations and, on the
// first run only, seeds the profile from USER.md. A store that cannot be opened
// is logged at ERROR and returned as nil: the profile tools are then left out,
// and the agent starts regardless.
//
// It is called from registerSharedTools, which both the CLI agent and the
// gateway reach through NewAgentLoop and which a config reload runs again; the
// shared handle makes that cheap and the migrations and the import are
// idempotent. Closing the handle is the caller's job (the gateway registers
// sqlitex.CloseShared as a shared resource).
func openProfileStore(cfg *config.Config) *intent.Store {
	workspace := DefaultAgentWorkspace(cfg)
	if workspace == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), profileOpenTimeout)
	defer cancel()

	db, err := sqlitex.Shared(workspace)
	if err != nil {
		logger.ErrorCF("agent", "Profile store unavailable: could not open the shared database; profile tools are disabled",
			map[string]any{"workspace": workspace, "error": err.Error()})
		return nil
	}
	store, err := intent.NewStore(ctx, db)
	if err != nil {
		logger.ErrorCF("agent", "Profile store unavailable: migration failed; profile tools are disabled",
			map[string]any{"error": err.Error()})
		return nil
	}
	if _, err := store.ImportIfEmpty(ctx, filepath.Join(workspace, "USER.md")); err != nil {
		// The store works; only the seed failed. Keep the tools.
		logger.WarnCF("agent", "Could not seed the profile from USER.md",
			map[string]any{"error": err.Error()})
	}
	return store
}

// profileTools builds the enabled profile tools over store, or none when the
// store is missing.
func profileTools(cfg *config.Config, store *intent.Store) []tools.Tool {
	if store == nil {
		return nil
	}
	var out []tools.Tool
	if cfg.Tools.IsToolEnabled(tools.NameGetProfile) {
		out = append(out, tools.NewGetProfileTool(store))
	}
	if cfg.Tools.IsToolEnabled(tools.NameUpdateProfile) {
		out = append(out, tools.NewUpdateProfileTool(store, cfg))
	}
	return out
}
