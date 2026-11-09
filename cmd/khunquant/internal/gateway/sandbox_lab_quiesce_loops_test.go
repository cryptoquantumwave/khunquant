package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// settleNames runs `inspect settle` with a short cap and returns whether it passed and
// the components it found busy.
func settleNames(t *testing.T, f *lockedFixture) (settled bool, busy map[string]bool) {
	t.Helper()
	code, out := labPost(t, f, "inspect", `{"op":"settle","wall_cap":"100ms"}`)
	if code != http.StatusOK {
		t.Fatalf("inspect settle = %d %v", code, out)
	}
	busy = map[string]bool{}
	names, _ := out["busy"].([]any)
	for _, n := range names {
		busy[n.(string)] = true
	}
	return out["settled"] == true, busy
}

// A heartbeat run fired by a manual advance reads HEARTBEAT.md and saves its start before
// any agent turn registers, and nothing else counts that: the step's settle used to pass
// at once and the advance returned with the run still going. The advance waits for the
// run now, and a settle meanwhile names the heartbeat.
func TestLabSettle_AdvanceWaitsForAHeartbeatRun(t *testing.T) {
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:00:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t, func(c *config.Config) { c.Heartbeat.Enabled = true })
	if err := os.WriteFile(filepath.Join(f.workspace, "HEARTBEAT.md"), []byte("- check the portfolio\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	releaseRun := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseRun) // before the fixture's own cleanup stops the services
	f.services.HeartbeatService.SetContextHandler(func(context.Context, string, string, string) *tools.ToolResult {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return tools.SilentResult("ok")
	})

	advanced := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, f.url("/api/sandbox/lab/clock"), strings.NewReader(`{"op":"advance","by":"1m"}`))
		if err != nil {
			advanced <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+lockTestToken)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			advanced <- err
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"done":true`) {
			advanced <- fmt.Errorf("advance = %d %s", resp.StatusCode, b)
			return
		}
		advanced <- nil
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the advance did not fire the heartbeat")
	}

	if settled, busy := settleNames(t, f); settled || !busy["heartbeat"] {
		t.Fatalf("settle during a heartbeat run: settled %v, busy %v; want heartbeat busy", settled, busy)
	}
	releaseRun()
	select {
	case err := <-advanced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the advance did not end after the heartbeat run")
	}
}

// The daily loss guard's equity sample is work no other quiescer sees (the exchange
// server is busy only while a request is inside it): a settle waits for it.
func TestLabSettle_WaitsForTheLossGuardSample(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	releaseSample := func() { once.Do(func() { close(release) }) }
	prev := lossGuardStarter
	lossGuardStarter = func(*config.Config) *lossGuardSampler {
		collect := func(context.Context, *config.Config, snapshot.CollectOptions) (*snapshot.CollectResult, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil, errors.New("exchange down")
		}
		return startLossGuardWith(lossGuardCfg(50), newTestTracker(), collect, time.Now)
	}
	t.Cleanup(func() { lossGuardStarter = prev })
	f := newLockedFixture(t)
	t.Cleanup(releaseSample) // before the fixture's own cleanup stops the sampler
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler took no sample")
	}

	if isIdle, ok := labIdle(t, f)["lossguard"]; !ok || isIdle {
		t.Fatalf("lossguard quiescer present %v, idle %v; want it busy while a sample runs", ok, isIdle)
	}
	if settled, busy := settleNames(t, f); settled || !busy["lossguard"] {
		t.Fatalf("settle during an equity sample: settled %v, busy %v; want lossguard busy", settled, busy)
	}
	releaseSample()
	if code, out := labPost(t, f, "inspect", `{"op":"settle"}`); code != http.StatusOK || out["settled"] != true {
		t.Fatalf("settle after the sample = %d %v", code, out)
	}
}
