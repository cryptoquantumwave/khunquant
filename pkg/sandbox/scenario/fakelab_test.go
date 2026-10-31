package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	labapi "github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/api"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// The runner's tests run against fake instances: each is a real lab (the real clock
// group, the real inspect group over a real journal, the real HTTP front-end) in this
// process, with recorder groups for chat, market and the rest. Nothing is started.

const fakeToken = "fake-token"

// fakeInstance is one instance of fakeSup.
type fakeInstance struct {
	name    string
	srv     *httptest.Server
	lab     *lab.Lab
	j       *labjournal.Journal
	busy    *busyFlag
	home    string
	info    supervisor.Info
	patches []map[string]any
}

// busyFlag is a quiescer a test can make busy.
type busyFlag struct {
	mu sync.Mutex
	on bool
}

func (b *busyFlag) set(v bool) { b.mu.Lock(); b.on = v; b.mu.Unlock() }
func (b *busyFlag) Idle() bool { b.mu.Lock(); defer b.mu.Unlock(); return !b.on }

// fakeSup is an Instances whose instances are fakeInstances.
type fakeSup struct {
	t *testing.T

	mu    sync.Mutex
	insts map[string]*fakeInstance
	order []*fakeInstance
	downs map[string]bool // name -> keepHome

	unlocked bool                         // report locked:false
	upErr    error                        // Up fails
	onUp     func(n int, i *fakeInstance) // after an instance is built (n from 1)
	onPatch  func(i *fakeInstance, patch map[string]any) (changed bool, err error)
	logs     []string
}

func newFakeSup(t *testing.T) *fakeSup {
	return &fakeSup{t: t, insts: map[string]*fakeInstance{}, downs: map[string]bool{}}
}

// recorder is a group whose ops journal "fake.<group>.<op>" with their arguments.
func recorder(l *lab.Lab, j *labjournal.Journal, name string, ops ...string) lab.Group {
	g := lab.Group{Name: name}
	for _, op := range ops {
		g.Ops = append(g.Ops, lab.Op{Name: op, Run: func(_ context.Context, args json.RawMessage) (any, error) {
			j.Append(labjournal.Entry{Kind: "fake." + name + "." + op, Source: "test", Payload: args})
			if name == "call_tool" && strings.Contains(string(args), `"fail_tool"`) { // a tool that ran and failed
				return map[string]any{"is_error": true, "for_llm": "the tool said no"}, nil
			}
			return map[string]any{"ok": true}, nil
		}})
	}
	return g
}

func (f *fakeSup) Up(_ context.Context, o supervisor.UpOptions) (supervisor.Info, error) {
	if f.upErr != nil {
		return supervisor.Info{}, f.upErr
	}
	f.mu.Lock()
	n := len(f.order) + 1
	f.mu.Unlock()
	name := fmt.Sprintf("fake-%d", n)
	home := f.t.TempDir()
	l := &lab.Lab{Home: home, Quiesce: quiesce.NewRegistry()}
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(home, "lab", "journal.db"), Now: func() time.Time { return l.Now() }})
	if err != nil {
		f.t.Fatal(err)
	}
	l.Journal = j
	env := map[string]string{lab.EnvClockStart: o.ClockStart, lab.EnvClockMode: o.ClockMode, lab.EnvClockResolution: o.ClockResolution}
	if err := l.InitClock(func(k string) string { return env[k] }); err != nil {
		f.t.Fatal(err)
	}
	busy := &busyFlag{}
	l.Quiesce.Register(quiesce.Func("test-component", busy.Idle))
	l.Settle = func(ctx context.Context) error { _, err := l.SettleWait(ctx, 300*time.Millisecond); return err }
	l.DeclareKinds("chat.out", "clock.step", "settle", "config.reload", "event.*", "exchange.req", "llm.call")

	deps := func() inspect.Deps { return inspect.Deps{SettleWait: l.SettleWait, Snapshot: l.Quiesce.Snapshot} }
	groups := []lab.Group{
		inspect.Group(j, deps), l.ClockGroup(),
		recorder(l, j, "chat", "send", "press", "react", "outbox", "set_state"),
		recorder(l, j, "call_tool", ""), recorder(l, j, "market", "set", "path", "shock", "walk", "replay", "delist", "relist"),
		recorder(l, j, "account", "seed", "get"), recorder(l, j, "llm", "script_put"), recorder(l, j, "manage", ""),
	}
	for _, g := range groups {
		if err := l.AddGroup(g); err != nil {
			f.t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	guard := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fakeToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	labapi.Register(mux, l, guard)
	mux.Handle("GET /api/sandbox/status", guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "locked": !f.unlocked})
	})))
	srv := httptest.NewServer(mux)
	in := &fakeInstance{name: name, srv: srv, lab: l, j: j, busy: busy, home: home}
	in.info = supervisor.Info{Name: name, Home: home, GatewayURL: srv.URL, State: supervisor.StateRunning}
	f.t.Cleanup(func() { srv.Close(); _ = j.Close(); _ = l.Clock.Close() })

	f.mu.Lock()
	f.insts[name] = in
	f.order = append(f.order, in)
	f.mu.Unlock()
	if f.onUp != nil {
		f.onUp(n, in)
	}
	return in.info, nil
}

func (f *fakeSup) get(name string) (*fakeInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.insts[name]
	if !ok {
		return nil, fmt.Errorf("no instance %q", name)
	}
	return in, nil
}

func (f *fakeSup) ups() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.order) }

func (f *fakeSup) Down(_ context.Context, name string, keep bool) error {
	f.mu.Lock()
	f.downs[name] = keep
	f.mu.Unlock()
	return nil
}

func (f *fakeSup) Get(name string) (supervisor.Info, error) {
	in, err := f.get(name)
	if err != nil {
		return supervisor.Info{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return in.info, nil
}

func (f *fakeSup) Client(name string) (*labclient.Client, error) {
	in, err := f.get(name)
	if err != nil {
		return nil, err
	}
	return labclient.New(in.srv.URL, fakeToken)
}

func (f *fakeSup) PatchConfig(_ context.Context, name string, patch map[string]any) (bool, error) {
	in, err := f.get(name)
	if err != nil {
		return false, err
	}
	in.patches = append(in.patches, patch)
	if f.onPatch == nil {
		return false, errors.New("no onPatch")
	}
	return f.onPatch(in, patch)
}

func (f *fakeSup) Restart(_ context.Context, name string, _ supervisor.RestartMode) (supervisor.Info, error) {
	return f.Get(name)
}

func (f *fakeSup) WaitRunning(_ context.Context, name string) (supervisor.Info, error) {
	return f.Get(name)
}

func (f *fakeSup) Logs(string, int, string) ([]string, error) { return f.logs, nil }

// exit makes the instance look like a gateway that ended with the lock's exit code.
func (f *fakeSup) exit(in *fakeInstance, code int, reason supervisor.ExitReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in.info.State, in.info.LastExit, in.info.ExitReason = supervisor.StateExited, &code, reason
}

// scenarioOf parses a scenario made of the standard head and body.
func scenarioOf(t *testing.T, body string) *File {
	t.Helper()
	return parseString(t, "version: 1\nscenario: t\nclock: {start: \"2026-10-20T09:00:00+07:00\"}\n"+body)
}
