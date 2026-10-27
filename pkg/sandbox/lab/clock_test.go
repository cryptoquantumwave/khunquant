package lab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

var clockT0 = time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// manualLab is a lab whose clock is manual at clockT0, with its journal open.
func manualLab(t *testing.T) *Lab {
	t.Helper()
	l := newTestLab(t)
	env := envOf(map[string]string{EnvClockStart: clockT0.Format(time.RFC3339), EnvClockMode: "manual"})
	if err := l.InitClock(env); err != nil {
		t.Fatal(err)
	}
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(l.Home, "lab", "journal.db"), Now: func() time.Time { return l.Now() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	l.Journal = j
	if err := l.AddGroup(l.ClockGroup()); err != nil {
		t.Fatal(err)
	}
	return l
}

func call(t *testing.T, l *Lab, args string) (map[string]any, error) {
	t.Helper()
	res, err := l.Call(context.Background(), "clock", OpOf(json.RawMessage(args)), json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, nil
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var le *Error
	if !errors.As(err, &le) || le.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestLabClock_ResumesFromStateFile(t *testing.T) {
	l := newTestLab(t)
	saved := simclock.State{Now: clockT0.Add(3 * time.Hour), Mode: simclock.ModeManual, Resolution: 30 * time.Second}
	if err := simclock.SaveState(l.ClockPath(), saved); err != nil {
		t.Fatal(err)
	}
	// The env names another start and mode; the file wins.
	env := envOf(map[string]string{EnvClockStart: clockT0.Format(time.RFC3339), EnvClockMode: "realtime"})
	if err := l.InitClock(env); err != nil {
		t.Fatal(err)
	}
	st := l.Clock.State()
	if !st.Now.Equal(saved.Now) || st.Mode != simclock.ModeManual || st.Resolution != 30*time.Second {
		t.Fatalf("clock = %+v, want the saved state %+v", st, saved)
	}
	if !l.Now().Equal(saved.Now) {
		t.Fatalf("Lab.Now = %v, want the sim time", l.Now())
	}
}

func TestLabClock_StartsFromEnvThenWallRealtime(t *testing.T) {
	l := newTestLab(t)
	if err := l.InitClock(envOf(map[string]string{EnvClockStart: "2026-01-01T00:00:00Z", EnvClockMode: "manual", EnvClockResolution: "10s"})); err != nil {
		t.Fatal(err)
	}
	if st := l.Clock.State(); st.Now.Year() != 2026 || st.Mode != simclock.ModeManual || st.Resolution != 10*time.Second {
		t.Fatalf("env clock = %+v", st)
	}

	// No env and no file: realtime at wall now, as an instance without the sim clock.
	l2 := newTestLab(t)
	if err := l2.InitClock(envOf(nil)); err != nil {
		t.Fatal(err)
	}
	if st := l2.Clock.State(); st.Mode != simclock.ModeRealtime || time.Since(st.Now) > time.Minute || time.Since(st.Now) < -time.Minute {
		t.Fatalf("default clock = %+v", st)
	}

	for name, env := range map[string]map[string]string{
		"bad start":      {EnvClockStart: "yesterday"},
		"bad mode":       {EnvClockMode: "fast"},
		"bad resolution": {EnvClockResolution: "-1m"},
	} {
		if err := newTestLab(t).InitClock(envOf(env)); err == nil {
			t.Errorf("%s: InitClock accepted %v", name, env)
		}
	}
}

func TestJournalStampsSimTime(t *testing.T) {
	l := manualLab(t)
	l.Journal.Append(labjournal.Entry{Kind: "event.test"})
	got, err := l.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{"event.test"}})
	if err != nil || len(got) != 1 {
		t.Fatalf("entries %v, err %v", got, err)
	}
	if !got[0].SimTime.Equal(clockT0) {
		t.Fatalf("sim time = %v, want the sim clock's %v (wall time %v)", got[0].SimTime, clockT0, got[0].WallTime)
	}
}

func TestClockGroup_Ops(t *testing.T) {
	l := manualLab(t)

	got, err := call(t, l, `{"op":"get"}`)
	if err != nil || got["mode"] != "manual" || got["now"] != "2026-03-10T09:00:00Z" || got["pending_timers"].(float64) != 0 {
		t.Fatalf("get = %v, %v", got, err)
	}

	got, err = call(t, l, `{"op":"advance","by":"30m"}`)
	if err != nil || got["done"] != true || got["reached"] != "2026-03-10T09:30:00Z" || got["settle"] != "engine-only" {
		t.Fatalf("advance by = %v, %v", got, err)
	}
	got, err = call(t, l, `{"op":"advance","to":"2026-03-10T12:00:00Z"}`)
	if err != nil || got["done"] != true || got["reached"] != "2026-03-10T12:00:00Z" {
		t.Fatalf("advance to = %v, %v", got, err)
	}
	// Each advance left a clock.step entry stamped with sim time, and clock.json follows.
	steps, err := l.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{KindClockStep}})
	if err != nil || len(steps) != 2 || steps[0].Source != "simclock" || !steps[1].SimTime.Equal(clockT0.Add(3*time.Hour)) {
		t.Fatalf("clock.step entries = %+v, err %v", steps, err)
	}
	if st, err := simclock.LoadState(l.ClockPath()); err != nil || !st.Now.Equal(clockT0.Add(3*time.Hour)) {
		t.Fatalf("clock.json = %+v, %v", st, err)
	}

	for _, bad := range []string{`{"op":"advance"}`, `{"op":"advance","by":"1m","to":"2026-03-11T00:00:00Z"}`,
		`{"op":"advance","by":"soon"}`, `{"op":"advance","to":"noon"}`, `{"op":"advance","by":"1m","wall_budget":"0s"}`,
		`{"op":"advance","by":"1m","nope":1}`, `{"op":"set_mode"}`, `{"op":"set_mode","mode":"warp"}`,
		`{"op":"set_mode","mode":"realtime","scale":2}`} {
		if _, err := call(t, l, bad); err == nil {
			t.Errorf("%s was accepted", bad)
		} else {
			wantCode(t, err, CodeBadArgs)
		}
	}
	_, err = call(t, l, `{"op":"advance","by":"-1h"}`)
	wantCode(t, err, CodeFailed) // the clock does not run backwards
	if !strings.Contains(err.Error(), simclock.ErrPast.Error()) {
		t.Fatalf("err = %v, want ErrPast", err)
	}

	// Scaled and realtime refuse an advance with the clock's own error.
	got, err = call(t, l, `{"op":"set_mode","mode":"scaled","scale":60,"resolution":"0s"}`)
	if err != nil || got["mode"] != "scaled" || got["scale"].(float64) != 60 || got["resolution"] != "0s" {
		t.Fatalf("set_mode scaled = %v, %v", got, err)
	}
	_, err = call(t, l, `{"op":"advance","by":"1m"}`)
	wantCode(t, err, CodeFailed)
	if !strings.Contains(err.Error(), simclock.ErrNotManual.Error()) {
		t.Fatalf("err = %v, want ErrNotManual", err)
	}
	if got, err = call(t, l, `{"op":"set_mode","mode":"manual"}`); err != nil || got["mode"] != "manual" {
		t.Fatalf("set_mode manual = %v, %v", got, err)
	}
	if st, _ := simclock.LoadState(l.ClockPath()); st.Mode != simclock.ModeManual {
		t.Fatalf("set_mode was not saved: %+v", st)
	}
}

func TestClockGroup_WallBudgetIsClamped(t *testing.T) {
	l := manualLab(t)
	var left time.Duration
	l.Settle = func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("advance runs with no wall deadline")
		}
		left = time.Until(dl)
		return nil
	}
	// Called directly, so only the op's own budget is on the context.
	for args, max := range map[string]time.Duration{
		`{"by":"1m"}`:                       DefaultWallBudget,
		`{"by":"1m","wall_budget":"1h"}`:    MaxWallBudget,
		`{"by":"1m","wall_budget":"500ms"}`: 500 * time.Millisecond,
	} {
		if _, err := l.clockAdvance(context.Background(), json.RawMessage(args)); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if left <= 0 || left > max {
			t.Errorf("%s: %v of budget left, want in (0, %v]", args, left, max)
		}
	}
	if MaxWallBudget >= 30*time.Second {
		t.Fatalf("MaxWallBudget %v is not under the 30s write timeout", MaxWallBudget)
	}
}

func TestClockGroup_AdvanceFiresTimersAndReportsWallBudget(t *testing.T) {
	l := manualLab(t)
	ch := l.Clock.After(time.Hour)
	if got, err := call(t, l, `{"op":"get"}`); err != nil || got["pending_timers"].(float64) != 1 || got["next_timer_at"] != "2026-03-10T10:00:00Z" {
		t.Fatalf("get = %v, %v", got, err)
	}
	settleErr := errors.New("engine: stuck")
	l.Settle = func(context.Context) error { return settleErr }
	_, err := call(t, l, `{"op":"advance","by":"2h"}`)
	wantCode(t, err, CodeFailed)
	if !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("a settle error was lost: %v", err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("the hour timer did not fire in the first step")
	}
}

// With Options.Getenv the clock exists before the server starts: Now is the Sim's from
// the first request, and requests served concurrently with reads of Now are race-free
// (run with -race).
func TestNew_ClockIsBuiltBeforeTheServerStarts(t *testing.T) {
	home := t.TempDir()
	fixtures := filepath.Join(home, "workspace", "sandbox")
	copyFixtures(t, fixtures)
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	env := envOf(map[string]string{EnvClockStart: clockT0.Format(time.RFC3339), EnvClockMode: "manual"})
	l, err := New(context.Background(), Options{Home: home, FixturesDir: fixtures, Token: testToken, Getenv: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if l.Clock == nil || !l.Now().Equal(clockT0) {
		t.Fatalf("Lab.Now = %v, want the Sim's %v from the first request", l.Now(), clockT0)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				resp, err := http.Get(l.Server.BaseURL() + "/binance/api.binance.com/api/v3/ticker/price?symbol=BTCUSDT")
				if err != nil {
					t.Error(err)
					return
				}
				_ = resp.Body.Close()
				_ = l.Now()
			}
		}()
	}
	wg.Wait()

	// A bad clock environment fails New, and leaves nothing serving.
	bad := envOf(map[string]string{EnvClockStart: "yesterday"})
	if _, err := New(context.Background(), Options{Home: home, FixturesDir: fixtures, Token: testToken, Getenv: bad}); err == nil {
		t.Fatal("New accepted a bad clock start")
	}
}
