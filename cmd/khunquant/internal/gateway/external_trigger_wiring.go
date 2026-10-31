package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// registerTriggerAPIFor registers the external trigger route when perception is
// effectively on (proactive.enabled and proactive.perception.enabled). It reads the
// running pipeline through the generation's holder at request time, because the
// route is registered before the pipeline is built, and answers 409 while there is
// none or while triggers are off.
func registerTriggerAPIFor(mux handleMux, s *gatewayServices, d assemblyDeps) {
	cfg := d.Cfg
	if !cfg.Proactive.Effective().Perception {
		return
	}
	registerTriggerAPI(mux, cfg.Gateway.ManagementToken, &triggerAPI{
		runtime: s.perceptionLive.Load,
		mode:    func() string { return cfg.Proactive.Effective().TriggersMode },
		limiter: d.Shared.triggerLimiter(),
		//simclock:wall the external trigger rate limiter bounds requests in wall time
		now: time.Now,
	})
}

// ensureExternalSpec makes sure the registry holds the builtin spec of the external
// kind, the one that turns a request to POST /api/proactive/trigger into a
// candidate. It is live: the effective mode is the lower of this and the global
// triggers mode. A spec of the kind that is already there (including one the owner
// switched off) is left as it is. It runs only when the registry was built, so with
// the default configuration nothing is written.
func ensureExternalSpec(ctx context.Context, reg *triggers.Registry) {
	have, err := reg.List(ctx, triggers.ListFilter{Kind: triggers.KindExternal})
	if err != nil || len(have) > 0 {
		return
	}
	_, err = reg.Add(ctx, triggers.Spec{
		ID: triggers.ExternalSpecID, Kind: triggers.KindExternal, Mode: triggers.ModeLive, Origin: "builtin",
	})
	if err != nil && !errors.Is(err, triggers.ErrExists) {
		logger.WarnCF("perception", "The external trigger could not be registered",
			map[string]any{"error": err.Error()})
	}
}
