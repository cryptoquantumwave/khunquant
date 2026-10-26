package gateway

import (
	"context"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// This file is the only place the gateway builds and attaches Sandbox Lab
// components. The lab exists only in a locked process; it is built once at start
// (buildLab), held on sharedResources, and never rebuilt by a reload.

// buildLab builds the process-lifetime lab from the enforced config. It starts the
// sandbox server (which publishes its base URL) and fails when there are no venues.
func buildLab(cfg *config.Config, lk sandbox.LockOptions) (*lab.Lab, error) {
	return lab.New(context.Background(), lab.Options{
		Home:        config.HomeDir(),
		FixturesDir: sandbox.ResolveFixturesDir(cfg),
		Token:       lk.Token,
	})
}

// registerLockedSandbox attaches the running lab to this generation of services and
// puts the /api/sandbox/* routes on mux. The bearer is the instance token, which
// stays in memory: it is not saved to config and not printed.
func registerLockedSandbox(mux handleMux, s *gatewayServices, l *lab.Lab) error {
	if l == nil {
		return errors.New("locked sandbox has no lab")
	}
	s.SandboxServer = l.Server
	s.SandboxToken = l.Token()
	s.SimulatorReseter = &sandboxStateReseter{sm: l.State, store: l.Store}

	mountSandboxAPI(mux, s, l.Store, l.FixturesDir())

	logger.InfoCF("sandbox", "Locked sandbox instance serving", map[string]any{"base_url": l.Server.BaseURL()})
	return nil
}

// mountSandboxAPI registers the /api/sandbox/* routes for the sandbox that s
// carries. Both the locked and the unlocked path end here, so the routes have one
// registration site.
func mountSandboxAPI(mux handleMux, s *gatewayServices, store *sandbox.Store, fixturesDir string) {
	registerSandboxAPI(mux, store, s.SandboxToken, s.SimulatorReseter, fixturesDir)
}
