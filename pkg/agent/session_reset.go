package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// ResetSession empties a session's stored history and summary and persists the
// result, so the session holds nothing from before this call. Scheduled agent
// jobs call it right before each run (see tools.CronTool.ExecuteJob): their
// session is stable per job, and without it every run would append to it forever.
//
// It takes the session lock, so it cannot interleave with a turn on the same
// session, and gives up when ctx is done. A session that does not exist yet is
// left alone (nothing is created). The session may live in any registered agent's
// store, because the agent that handles a turn is chosen by the route, not by the
// key, so every store is cleared.
func (al *AgentLoop) ResetSession(ctx context.Context, sessionKey string) error {
	if sessionKey == "" {
		return errors.New("reset session: empty session key")
	}
	return al.WithSessionLockContext(ctx, sessionKey, func(context.Context) error {
		registry := al.GetRegistry()
		var errs []error
		for _, id := range registry.ListAgentIDs() {
			inst, ok := registry.GetAgent(id)
			if !ok || inst.Sessions == nil {
				continue
			}
			// Some stores create the session on write, so only touch ones that have
			// something to clear (the first run of a job has nothing).
			if len(inst.Sessions.GetHistory(sessionKey)) == 0 && inst.Sessions.GetSummary(sessionKey) == "" {
				continue
			}
			inst.Sessions.SetHistory(sessionKey, []providers.Message{})
			inst.Sessions.SetSummary(sessionKey, "")
			if err := inst.Sessions.Save(sessionKey); err != nil {
				errs = append(errs, fmt.Errorf("agent %s: %w", id, err))
			}
		}
		return errors.Join(errs...)
	})
}
