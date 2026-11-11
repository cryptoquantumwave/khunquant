package inspect_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/replay"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

var exportT0 = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

// exportRig is a lab over a journal that a real bus feeds through the real observer, so
// the recordable flag is the one set on the live event.
type exportRig struct {
	*rig
	home  string
	bus   *events.EventBus
	specs []triggers.Spec
	n     atomic.Int64
}

func newExportRig(t *testing.T) *exportRig {
	t.Helper()
	x := &exportRig{home: t.TempDir(), bus: events.NewBus()}
	t.Cleanup(func() { _ = x.bus.Close() })
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(x.home, "lab", "journal.db"),
		Now: func() time.Time { return exportT0.Add(time.Duration(x.n.Add(1)) * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	obs, err := labjournal.StartObserver(context.Background(), x.bus, j)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	deps := func() inspect.Deps {
		return inspect.Deps{
			Home:      x.home,
			Specs:     func(context.Context) ([]triggers.Spec, error) { return x.specs, nil },
			Attention: func() config.AttentionConfig { return config.DefaultConfig().Proactive.Attention },
			Profile:   func(context.Context) (intent.Profile, error) { return intent.DefaultProfile(), nil },
		}
	}
	l := &lab.Lab{}
	if err := l.AddGroup(inspect.Group(j, deps)); err != nil {
		t.Fatal(err)
	}
	x.rig = &rig{t: t, l: l, j: j}
	return x
}

// publish sends events and waits until the journal holds them all.
func (x *exportRig) publish(evs ...events.Event) {
	x.t.Helper()
	for _, ev := range evs {
		x.bus.PublishNonBlocking(ev)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		es, err := x.j.Query(context.Background(), labjournal.Query{Kinds: []string{"event.perception.*"}})
		if err != nil {
			x.t.Fatal(err)
		}
		if len(es) >= len(evs) {
			return
		}
	}
	x.t.Fatal("the observer did not journal the events")
}

func tick(at time.Time, last float64) events.Event {
	return events.Event{ID: at.Format("150405"), Kind: events.KindPerceptionMarketTick, Time: at, Trust: events.TrustPtr(proactive.TrustExchange),
		Source: events.Source{Component: "perception", Name: "market"}, Attrs: map[string]any{"subject": "price:okx:BTC/USDT"},
		Payload: map[string]any{"provider": "okx", "symbol": "BTC/USDT", "last": last, "at": at}}
}

func futures(at time.Time) events.Event {
	return events.Event{ID: "f" + at.Format("150405"), Kind: events.KindPerceptionFuturesPositions, Time: at, Trust: events.TrustPtr(proactive.TrustExchange),
		Payload: map[string]any{"provider": "okx", "account": "main", "at": at, "positions": []any{}}}
}

var notFire = `[{"match":"price_cross","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z","should_fire":false}]`

func (x *exportRig) export(args string) (map[string]any, error) {
	x.t.Helper()
	return x.call("export_replay", args)
}

func readCase(t *testing.T, home, name, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "lab", "export", name, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExportOnlyRecordable(t *testing.T) {
	x := newExportRig(t)
	exch := events.TrustPtr(proactive.TrustExchange)
	x.publish(
		tick(exportT0, 70000),
		events.Event{ID: "n1", Kind: events.KindPerceptionNewsItem, Time: exportT0, Trust: exch,
			Payload: perception.NewsItem{Asset: "BTC", Summary: "SECRET-NEWS-TEXT"}},
		events.Event{ID: "x1", Kind: events.KindPerceptionExternalTrigger, Time: exportT0, Trust: exch,
			Payload: perception.ExternalTrigger{Name: "n", Message: "SECRET-EXTERNAL-TEXT"}},
		events.Event{ID: "low", Kind: events.KindPerceptionMarketTick, Time: exportT0, // no trust set: below exchange
			Payload: map[string]any{"provider": "okx", "symbol": "BTC/USDT", "last": 1, "note": "SECRET-LOW-TRUST"}},
		futures(exportT0.Add(time.Minute)),
	)
	out, err := x.export(`{"case":"c1","labels":` + notFire + `}`)
	if err != nil {
		t.Fatal(err)
	}
	ev := readCase(t, x.home, "c1", "events.jsonl")
	for _, secret := range []string{"SECRET-NEWS-TEXT", "SECRET-EXTERNAL-TEXT", "SECRET-LOW-TRUST"} {
		if strings.Contains(ev, secret) {
			t.Errorf("events.jsonl holds %s", secret)
		}
	}
	if n := strings.Count(ev, "\n"); n != 2 || out["events"].(float64) != 2 {
		t.Errorf("events.jsonl has %d lines (reported %v), want the tick and the futures positions only:\n%s", n, out["events"], ev)
	}
}

func TestExportPolicyDropsUnreplayableKinds(t *testing.T) {
	x := newExportRig(t)
	x.specs = []triggers.Spec{
		{ID: "dca1", Kind: triggers.KindDCADue, Mode: "live", Origin: "tool:x"},
		{ID: "px", Kind: triggers.KindPriceCross, Mode: "live", Origin: "tool:set_price_alert",
			Params: json.RawMessage(`{"provider":"okx","symbol":"BTC/USDT","direction":"below","threshold":60000,"recurring":true}`)},
		{ID: "liq", Kind: triggers.KindLiquidationDistance, Mode: "live", Origin: triggers.PortfolioSpecOrigin},
	}
	x.publish(tick(exportT0, 70000), futures(exportT0))
	out, err := x.export(`{"case":"c2","labels":` + notFire + `}`)
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := json.Marshal(out["skipped"])
	if !strings.Contains(string(sk), "dca1") || !strings.Contains(string(sk), "dca_due") {
		t.Errorf("skipped = %s, want the dca_due spec named", sk)
	}
	pol, err := replay.LoadPolicy(filepath.Join(x.home, "lab", "export", "c2", "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !pol.BuiltinPortfolio || len(pol.Specs) != 1 || pol.Specs[0].Kind != triggers.KindPriceCross || len(pol.FuturesAccounts) != 1 || pol.FuturesAccounts[0] != "okx/main" {
		t.Errorf("policy = %+v, want builtin portfolio, the price_cross spec only, futures account okx/main", pol)
	}
}

// The three files load with the strict loaders, and the golden is what replaying them gives.
func TestExportRoundTripLoads(t *testing.T) {
	x := newExportRig(t)
	x.publish(tick(exportT0, 70000), futures(exportT0))
	if _, err := x.export(`{"case":"c3","labels":` + notFire + `}`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(x.home, "lab", "export", "c3")
	lines, err := replay.LoadEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil || len(lines) != 2 {
		t.Fatalf("events: %d lines, %v", len(lines), err)
	}
	labels, err := replay.LoadLabels(filepath.Join(dir, "labels.json"))
	if err != nil || len(labels) != 1 {
		t.Fatalf("labels: %v, %v", labels, err)
	}
	pol, err := replay.LoadPolicy(filepath.Join(dir, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := replay.Run(context.Background(), replay.Input{Lines: lines, Labels: labels, Policy: pol})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := rep.Golden().JSON()
	if got := readCase(t, x.home, "c3", "golden.json"); got != string(want) {
		t.Errorf("golden.json = %s, want %s", got, want)
	}
	if entries, _ := os.ReadDir(filepath.Join(x.home, "lab", "export")); len(entries) != 1 {
		t.Errorf("the export directory holds %v, want the case only (no staging leftovers)", entries)
	}
}

func TestExportWritesGolden(t *testing.T) {
	x := newExportRig(t)
	x.publish(tick(exportT0, 70000), futures(exportT0))
	if _, err := x.export(`{"case":"c4","labels":[]}`); err == nil {
		t.Error("a case with no label must be refused")
	}
	// A critical label that never fires would fail TestReplay_GoldenScenarios.
	critical := `[{"match":"price_cross","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z","should_fire":true,"critical":true}]`
	if _, err := x.export(`{"case":"c4","labels":` + critical + `}`); err == nil || !strings.Contains(err.Error(), "TestReplay_GoldenScenarios") {
		t.Errorf("a case that fails critical recall must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.home, "lab", "export", "c4")); err == nil {
		t.Error("a refused case left files behind")
	}
	if _, err := x.export(`{"case":"c4","allow_failing":true,"labels":` + critical + `}`); err != nil {
		t.Errorf("allow_failing must export it: %v", err)
	}
	for _, bad := range []string{
		`{"case":"c5","labels":[{"match":"","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z"}]}`,
		`{"case":"c5","labels":[{"match":"a","from":"2026-03-10T06:00:00Z","to":"2026-03-10T05:00:00Z"}]}`,
		`{"case":"c5","labels":[{"match":"a","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z","critical":true}]}`,
		`{"case":"c5","labels":[{"match":"a","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z","shuld_fire":true}]}`,
	} {
		if _, err := x.export(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

func TestExportStaysInHome(t *testing.T) {
	x := newExportRig(t)
	x.publish(tick(exportT0, 70000), futures(exportT0))
	parent := filepath.Dir(x.home)
	before, _ := os.ReadDir(parent)
	for _, c := range []string{"../x", "a/b", `a\b`, "..", ".", "", "UPPER", strings.Repeat("a", 49), "/abs"} {
		args, _ := json.Marshal(map[string]any{"case": c, "labels": json.RawMessage(notFire)})
		if _, err := x.export(string(args)); err == nil {
			t.Errorf("case %q was accepted", c)
		}
	}
	if after, _ := os.ReadDir(parent); len(after) != len(before) {
		t.Errorf("a refused case changed %s", parent)
	}
	if es, _ := os.ReadDir(filepath.Join(x.home, "lab", "export")); len(es) != 0 {
		t.Errorf("a refused case left %v under lab/export", es)
	}
	if _, err := x.export(`{"case":"ok_1","labels":` + notFire + `}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.home, "lab", "export", "ok_1", "golden.json")); err != nil {
		t.Error(err)
	}
}

// An export says so when the replay tier misses a label (217-3): the case is still written
// with allow_failing, and golden.json pins the miss, so the answer must name it.
func TestExportWarnsWhenLabelsAreNotReproduced(t *testing.T) {
	x := newExportRig(t)
	x.publish(tick(exportT0, 70000), futures(exportT0))
	missed := `[{"match":"price_cross","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z","should_fire":true,"critical":true}]`
	out, err := x.export(`{"case":"w1","allow_failing":true,"labels":` + missed + `}`)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := out["warning"].(string); !strings.Contains(w, "does not reproduce every label") || !strings.Contains(w, "recall 0.00") || !strings.Contains(w, "critical recall ok: false") {
		t.Errorf("warning = %q, want the missed label named with its recall", w)
	}
	if out, err = x.export(`{"case":"w2","labels":` + notFire + `}`); err != nil {
		t.Fatal(err)
	}
	if w, ok := out["warning"]; ok {
		t.Errorf("a case whose labels all hold got a warning: %v", w)
	}
}
