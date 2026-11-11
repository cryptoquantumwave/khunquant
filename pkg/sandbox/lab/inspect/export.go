package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/replay"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// ExportDir is where export_replay writes, under the instance home: <home>/lab/export/<case>/.
var ExportDir = filepath.Join("lab", "export")

var caseRE = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)

// ExportFiles are the four files of a replay case (testdata/replay/<case>/).
var ExportFiles = []string{"events.jsonl", "policy.json", "labels.json", "golden.json"}

type exportArgs struct {
	Op           string         `json:"op"` // the lab adds it to every call
	Case         string         `json:"case"`
	Labels       []replay.Label `json:"labels"`
	Since        time.Time      `json:"since"`
	Until        time.Time      `json:"until"`
	AllowFailing bool           `json:"allow_failing"`
}

// exportReplayOp turns the journal into a replay case: the recordable events (the flag
// the observer set on the live event; nothing is re-derived from decoded JSON), the
// policy this instance runs, the given labels and the golden summary of replaying them.
// It writes only under <home>/lab/export/<case>/, validated with the strict loaders and
// replayed twice, and refuses a case TestReplay_GoldenScenarios would fail.
func exportReplayOp(j *labjournal.Journal, deps func() Deps) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var a exportArgs
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		d := deps()
		switch {
		case d.Err != nil:
			return nil, d.Err
		case !caseRE.MatchString(a.Case):
			return nil, lab.Errorf(lab.CodeBadArgs, "case must match %s", caseRE)
		case len(a.Labels) == 0:
			return nil, lab.Errorf(lab.CodeBadArgs, "at least one label is required: a case with none fails the golden test")
		case d.Home == "" || d.Specs == nil || d.Attention == nil || d.Profile == nil:
			return nil, unavailable("the trigger registry, attention configuration or profile")
		}
		lines, accounts, err := recordable(ctx, j, a.Since, a.Until)
		if err != nil {
			return nil, err
		}
		pol, skipped, err := policyOf(ctx, d, accounts)
		if err != nil {
			return nil, err
		}
		root := filepath.Join(d.Home, ExportDir)
		if err := os.MkdirAll(root, 0o700); err != nil || !sandbox.PathInside(d.Home, root) {
			return nil, errors.New("export: the export directory is not inside the instance home")
		}
		stage, err := os.MkdirTemp(root, ".stage-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(stage)
		rep, err := writeCase(ctx, stage, lines, a.Labels, pol, a.AllowFailing)
		if err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		final := filepath.Join(root, a.Case)
		if err := os.RemoveAll(final); err != nil {
			return nil, err
		}
		if err := os.Rename(stage, final); err != nil {
			return nil, err
		}
		out := map[string]any{"dir": filepath.ToSlash(filepath.Join(ExportDir, a.Case)), "files": ExportFiles,
			"events": len(lines), "skipped": skipped, "golden": rep.Golden(), "critical_recall_ok": rep.CriticalRecallOK()}
		if w := labelWarning(rep); w != "" {
			out["warning"] = w
		}
		return out, nil
	}
}

// labelWarning says so when the replay tier does not reproduce every label of the case: a
// missed should-fire label (recall below 1), a should-not-fire label that fired (precision
// below 1) or a missed critical label. The case is still written, and its golden pins these
// figures, so an export is never silent about them (217-3).
func labelWarning(rep *replay.Report) string {
	l := rep.Labels
	if l == nil || (l.Recall >= 1 && l.Precision >= 1 && rep.CriticalRecallOK()) {
		return ""
	}
	return fmt.Sprintf("the replay tier does not reproduce every label (recall %.2f, precision %.2f, critical recall ok: %v); the case is exported and golden.json pins these figures",
		l.Recall, l.Precision, rep.CriticalRecallOK())
}

// recordable reads the journaled perception events that carry recordable:true, oldest
// first, as replay lines on sim time, and the provider/account pairs of the futures
// positions among them.
func recordable(ctx context.Context, j *labjournal.Journal, since, until time.Time) (lines []replay.Line, accounts []string, err error) {
	for after := int64(0); ; {
		es, err := j.Query(ctx, labjournal.Query{Kinds: []string{"event.perception.*"}, Since: since, Until: until, AfterSeq: after, Limit: labjournal.MaxLimit})
		if err != nil {
			return nil, nil, err
		}
		for _, e := range es {
			var p struct {
				Recordable bool            `json:"recordable"`
				Event      json.RawMessage `json:"event"`
			}
			if json.Unmarshal(e.Payload, &p) != nil || !p.Recordable {
				continue
			}
			l := replay.Line{Clock: e.SimTime.UTC()}
			if json.Unmarshal(p.Event, &l.Event) != nil {
				continue
			}
			lines = append(lines, l)
			if m, ok := l.Event.Payload.(map[string]any); ok && l.Event.Kind == "perception.futures.positions" {
				pa, _ := m["provider"].(string)
				ac, _ := m["account"].(string)
				if k := pa + "/" + ac; pa != "" && ac != "" && !slices.Contains(accounts, k) {
					accounts = append(accounts, k)
				}
			}
		}
		if len(es) < labjournal.MaxLimit {
			break
		}
		after = es[len(es)-1].Seq
	}
	if len(lines) == 0 {
		return nil, nil, lab.Errorf(lab.CodeBadArgs, "the journal holds no recordable event in the window")
	}
	slices.Sort(accounts)
	return lines, accounts, nil
}

// policyOf is the replay policy of this instance: its trigger specs (the kinds a replay
// can run; the others are returned in skipped), attention configuration and profile.
func policyOf(ctx context.Context, d Deps, accounts []string) (replay.Policy, []string, error) {
	specs, err := d.Specs(ctx)
	if err != nil {
		return replay.Policy{}, nil, err
	}
	prof, err := d.Profile(ctx)
	if err != nil {
		return replay.Policy{}, nil, err
	}
	att := d.Attention()
	pol := replay.Policy{Attention: &att, Profile: &prof, FuturesAccounts: accounts}
	skipped := []string{}
	for _, s := range specs {
		switch {
		case !replay.CanRun(s.Kind):
			skipped = append(skipped, fmt.Sprintf("spec %s (%s): replay has no factory for this kind", s.ID, s.Kind))
		case s.Origin == triggers.PortfolioSpecOrigin:
			pol.BuiltinPortfolio = true
		default:
			pol.Specs = append(pol.Specs, replay.SpecDef{ID: s.ID, Kind: s.Kind, Mode: s.Mode, Params: s.Params, Origin: s.Origin})
		}
	}
	return pol, skipped, nil
}

// writeCase writes the four files into dir and returns the report of replaying them: the
// files are read back with the strict loaders, replayed twice (the reports must be
// identical), and refused when the golden test would fail on them.
func writeCase(ctx context.Context, dir string, lines []replay.Line, labels []replay.Label, pol replay.Policy, allowFailing bool) (*replay.Report, error) {
	var ev bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}
		ev.Write(append(b, '\n'))
	}
	put := func(name string, v any) error {
		b, ok := v.([]byte)
		if !ok {
			var err error
			if b, err = json.MarshalIndent(v, "", "  "); err != nil {
				return err
			}
			b = append(b, '\n')
		}
		return os.WriteFile(filepath.Join(dir, name), b, 0o600)
	}
	for name, v := range map[string]any{"events.jsonl": ev.Bytes(), "labels.json": labels, "policy.json": pol} {
		if err := put(name, v); err != nil {
			return nil, err
		}
	}
	in := replay.Input{Policy: pol}
	var err error
	if in.Lines, err = replay.LoadEvents(filepath.Join(dir, "events.jsonl")); err != nil {
		return nil, err
	}
	if in.Labels, err = replay.LoadLabels(filepath.Join(dir, "labels.json")); err != nil {
		return nil, err
	}
	if in.Policy, err = replay.LoadPolicy(filepath.Join(dir, "policy.json")); err != nil {
		return nil, err
	}
	rep, err := replay.Repeat(2, func() (*replay.Report, error) { return replay.Run(ctx, in) })
	if err != nil {
		return nil, err
	}
	if e := rep.Engine; !allowFailing && (e.StageErrors != 0 || e.StagePanics != 0 || e.TriggerErrors != 0 || e.DeliveryErrors != 0 || e.DispatchFailures != 0 || !rep.CriticalRecallOK()) {
		return nil, fmt.Errorf("the case would fail TestReplay_GoldenScenarios (engine %+v, critical recall ok: %v); allow_failing exports it anyway", e, rep.CriticalRecallOK())
	}
	g, err := rep.Golden().JSON()
	if err != nil {
		return nil, err
	}
	return rep, put("golden.json", g)
}
