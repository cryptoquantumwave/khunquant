package gateway

import (
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// registerQuiescers registers, once per process and only when locked, one quiescer for
// every component that can hold work in flight, so that an advance and a settle wait for
// the whole gateway. Each closure reads the current generation's services at call time,
// never a captured pointer: a reload builds new components, and between generations
// there are none (a component that is off in this generation is idle). A reload in
// progress is its own quiescer (gateway.reload).
//
// Not registered, and why:
//   - control state (the kill-flag cache): a refresh is a read with no work in flight and
//     its interval is a timer;
//   - the device perception source: it only reacts to real hotplug events, which a lab
//     does not produce. (The clock and DCA-plan sources do run a pass after a sim-clock
//     wake, so they are Idlers under "perception".)
//   - SimWeb and the fault injector's latencies: SBX-15 and SBX-13 add them.
func registerQuiescers(reg *quiesce.Registry, shared *sharedResources, l *lab.Lab) {
	// idleOf adapts a per-generation check: a missing generation is idle.
	idleOf := func(name string, f func(s *gatewayServices) bool) {
		reg.Register(quiesce.Func(name, func() bool {
			s := shared.labGen.Load()
			return s == nil || f(s)
		}))
	}
	perception := func(f func(rt *perceptionRuntime) bool) func(*gatewayServices) bool {
		return func(s *gatewayServices) bool { return s.Perception == nil || f(s.Perception) }
	}

	idleOf("engine", perception(func(rt *perceptionRuntime) bool { return rt.engine == nil || rt.engine.Idle() }))
	idleOf("perception", perception(func(rt *perceptionRuntime) bool { return rt.manager == nil || rt.manager.Idle() }))
	idleOf("outbox", func(s *gatewayServices) bool { return s.Attention == nil || s.Attention.Idle() })
	idleOf("channels", func(s *gatewayServices) bool { return s.ChannelManager == nil || s.ChannelManager.Idle() })
	idleOf("cognition", func(s *gatewayServices) bool {
		return s.Cognition == nil || s.Cognition.sched == nil || s.Cognition.sched.Pending() == 0
	})
	idleOf("cron", func(s *gatewayServices) bool { return s.CronService == nil || s.CronService.Idle() })
	idleOf("notify", func(s *gatewayServices) bool { return s.NotifyService.Idle() })
	idleOf("worldmodel", perception(func(rt *perceptionRuntime) bool {
		return rt.worldModel == nil || rt.worldModel.store == nil || rt.worldModel.store.Idle()
	}))
	idleOf("journal", func(s *gatewayServices) bool {
		rt := s.journalLive.Load()
		return rt == nil || rt.engine == nil || rt.engine.Idle()
	})
	idleOf("approval", func(s *gatewayServices) bool { return s.Approval == nil || s.Approval.svc == nil || s.Approval.svc.Idle() })
	idleOf("mandate_sync", func(s *gatewayServices) bool {
		return s.DCAFlow == nil || s.DCAFlow.sync == nil || s.DCAFlow.sync.idle()
	})
	idleOf("feedback", func(s *gatewayServices) bool { return s.Feedback == nil || s.Feedback.svc.Idle() })
	idleOf("hindsight", func(s *gatewayServices) bool { return s.Feedback == nil || s.Feedback.svc.HindsightIdle() })
	idleOf("policy", func(s *gatewayServices) bool { return s.Policy == nil || s.Policy.rec.Idle() })
	idleOf("replay", perception(func(rt *perceptionRuntime) bool { return rt.recorder.Idle() }))
	idleOf("reflection", func(s *gatewayServices) bool { return s.Reflection == nil || s.Reflection.svc.Idle() })
	idleOf("intents", perception(func(rt *perceptionRuntime) bool { return rt.intentComp.Idle() }))
	idleOf("agent", func(s *gatewayServices) bool { return s.labSetup == nil || s.labSetup.al == nil || s.labSetup.al.Idle() })
	idleOf("bus", func(s *gatewayServices) bool { return s.labSetup == nil || s.labSetup.bus == nil || s.labSetup.bus.Idle() })

	// Process-lifetime components: the order feed is a package-level publisher, the
	// simulated chat's world and the exchange server live as long as the lab, and a
	// reload is process state.
	reg.Register(quiesce.Func("orderfeed", orderfeed.Idle))
	reg.Register(quiesce.Func("gateway.reload", func() bool { return shared.reloading.Load() == 0 }))
	if shared.simWorld != nil {
		reg.Register(quiesce.Func("simchat", shared.simWorld.Idle))
	}
	if l != nil {
		if l.Server != nil {
			reg.Register(quiesce.Func("exchange_sim", l.Server.Idle))
		}
		reg.Register(quiesce.Func("labjournal", func() bool {
			if l.Journal != nil && !l.Journal.Idle() {
				return false
			}
			shared.mu.Lock()
			obs := shared.labObserver
			shared.mu.Unlock()
			return obs == nil || obs.Idle()
		}))
	}
}
