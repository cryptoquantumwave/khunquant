package channels

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

type stubRouter struct{}

func (stubRouter) TryHandle(context.Context, ControlInput) (ControlReply, bool) {
	return ControlReply{}, false
}
func (stubRouter) Recognises(ControlInput) bool { return false }

// routable is a channel that takes a router, as every BaseChannel does.
type routable struct{ *BaseChannel }

func (routable) Start(context.Context) error                     { return nil }
func (routable) Stop(context.Context) error                      { return nil }
func (routable) Send(context.Context, bus.OutboundMessage) error { return nil }

func TestManager_SetControlRouter(t *testing.T) {
	m := newTestManager()
	a := routable{NewBaseChannel("a", nil, m.bus, nil)}
	m.channels["a"] = a

	if m.ControlEnabled() || a.ControlRouter() != nil {
		t.Fatal("a control path exists before one is installed")
	}
	r := stubRouter{}
	m.SetControlRouter(r)
	if a.ControlRouter() == nil {
		t.Error("the router was not given to the channel")
	}
	if !m.ControlEnabled() {
		t.Error("message actions stay disabled although the control path exists")
	}

	// A channel created later gets it too.
	if b := (routable{NewBaseChannel("b", nil, m.bus, nil)}); m.controlRouter.Load() == nil {
		t.Error("the manager forgot the router")
	} else {
		b.SetControlRouter(m.controlRouter.Load().r)
		if b.ControlRouter() == nil {
			t.Error("a router taken from the manager does not stick")
		}
	}

	m.SetControlRouter(nil)
	if a.ControlRouter() != nil || m.ControlEnabled() {
		t.Error("removing the router left the control path installed")
	}
}
