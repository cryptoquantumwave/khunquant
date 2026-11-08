// Package crash makes a locked sandbox process die the way kill -9 kills it, at a named
// step of a workflow. The journal engine reports each crash point to the hook (Hook); when
// an armed point matches, fire writes a marker and kills the process: no deferred function
// runs and nothing is flushed, so SQLite keeps its last committed transaction, as after a
// real kill. The next process turns the marker into a `crash` journal entry.
//
// Two guards keep this out of production: journal.SetCrashHook refuses outside a locked
// process, and Arm refuses too. Arms are one-shot and never saved, so a restarted process
// has none and cannot crash again by itself.
package crash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// KindCrash is the journal kind of a crash the previous process died of.
const KindCrash = "crash"

// MarkerName is the crash marker, <home>/lab/crash.json.
const MarkerName = "crash.json"

// flushWait bounds the journal flush before the kill (wall time: the process is about to die).
const flushWait = 3 * time.Second

// Arm names one crash point: the workflow kind, the step and the point of the step.
type Arm struct {
	ID       string `json:"id"`
	Workflow string `json:"workflow"`          // kind, e.g. dn.open
	Step     string `json:"step"`              // e.g. place_second_leg
	Point    string `json:"point"`             // before_started | after_started | after_fn | after_output
	Attempt  int    `json:"attempt,omitempty"` // 0: any attempt
}

// Marker is what the dying process leaves: which arm fired, where, and when.
type Marker struct {
	ArmID      string    `json:"arm_id"`
	Point      string    `json:"point"`
	Kind       string    `json:"kind"`
	WorkflowID string    `json:"workflow_id"`
	Step       string    `json:"step"`
	Attempt    int       `json:"attempt"`
	SimTime    time.Time `json:"sim_time"`
	WallTime   time.Time `json:"wall_time"`
}

// Journal is the lab journal as the arms use it.
type Journal interface {
	Append(e labjournal.Entry)
	Flush(ctx context.Context) error
	Query(ctx context.Context, q labjournal.Query) ([]labjournal.Entry, error)
}

// Options configures New.
type Options struct {
	Home    string
	Journal Journal
	Now     func() time.Time // sim time
}

// Arms is the table of armed crash points of one process.
type Arms struct {
	home    string
	journal Journal
	now     func() time.Time

	mu   sync.Mutex
	arms []Arm
	seq  int
}

var (
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	pointRE = map[string]bool{journal.CrashBeforeStarted: true, journal.CrashAfterStarted: true, journal.CrashAfterFn: true, journal.CrashAfterOutput: true}
)

// New builds an empty table.
func New(o Options) *Arms {
	if o.Now == nil {
		o.Now = time.Now //simclock:wall no injected clock: the caller did not give one
	}
	return &Arms{home: o.Home, journal: o.Journal, now: o.Now}
}

// Arm arms one crash point and returns its ID. It refuses unless the process is locked.
// The arm fires once and is not saved.
func (a *Arms) Arm(x Arm) (string, error) {
	if !sandbox.Locked() {
		return "", errors.New("crash: arming needs a locked sandbox process")
	}
	switch {
	case !nameRE.MatchString(x.Workflow) || !nameRE.MatchString(x.Step):
		return "", errors.New("crash: workflow and step are required names (for example dn.open and place_second_leg)")
	case !pointRE[x.Point]:
		return "", fmt.Errorf("crash: point %q is not before_started, after_started, after_fn or after_output", x.Point)
	case x.Attempt < 0:
		return "", errors.New("crash: attempt cannot be negative")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	x.ID = fmt.Sprintf("c%d", a.seq)
	a.arms = append(a.arms, x)
	return x.ID, nil
}

// Disarm removes an arm and says whether it was there.
func (a *Arms) Disarm(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, x := range a.arms {
		if x.ID == id {
			a.arms = append(a.arms[:i:i], a.arms[i+1:]...)
			return true
		}
	}
	return false
}

// Fired is a crash an earlier process died of, as the journal records it.
type Fired struct {
	Marker
	Seq int64 `json:"seq"`
}

// List returns the armed points and the crashes the journal holds (they survive restarts).
func (a *Arms) List(ctx context.Context) (armed []Arm, fired []Fired, err error) {
	a.mu.Lock()
	armed = append([]Arm{}, a.arms...)
	a.mu.Unlock()
	fired = []Fired{}
	if a.journal == nil {
		return armed, fired, nil
	}
	es, err := a.journal.Query(ctx, labjournal.Query{Kinds: []string{KindCrash}, Limit: labjournal.MaxLimit})
	if err != nil {
		return nil, nil, err
	}
	for _, e := range es {
		f := Fired{Seq: e.Seq}
		if json.Unmarshal(e.Payload, &f.Marker) == nil {
			fired = append(fired, f)
		}
	}
	return armed, fired, nil
}

// Hook is the journal's crash hook: it matches the point against the armed set, removes
// the arm that matched, and fires it. It never returns on a match.
func (a *Arms) Hook() journal.CrashHook {
	return func(p journal.CrashPoint) {
		a.mu.Lock()
		var hit *Arm
		for i, x := range a.arms {
			if x.Workflow == p.Kind && x.Step == p.Step && x.Point == p.Point && (x.Attempt == 0 || x.Attempt == p.Attempt) {
				hit = &x
				a.arms = append(a.arms[:i:i], a.arms[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
		if hit != nil {
			a.fire(*hit, p)
		}
	}
}

// fire leaves the marker and kills the process. It is the only caller of selfKill
// (TestSelfKillOnlyFromFire). The lab journal is flushed first: it is the instrument that
// records what the simulated venue did, not state of the product under test, and an
// entry still queued when the process dies would make the venue's history look shorter
// than it was.
func (a *Arms) fire(x Arm, p journal.CrashPoint) {
	if !sandbox.Locked() {
		return // belt and braces: Arm refuses, and the hook is installed only when locked
	}
	if a.journal != nil {
		ctx, cancel := context.WithTimeout(context.Background(), flushWait)
		_ = a.journal.Flush(ctx)
		cancel()
	}
	m := Marker{ArmID: x.ID, Point: p.Point, Kind: p.Kind, WorkflowID: p.WorkflowID, Step: p.Step, Attempt: p.Attempt,
		SimTime: a.now(), WallTime: time.Now()} //simclock:wall the marker records both clocks
	if b, err := json.Marshal(m); err == nil {
		if err = os.MkdirAll(filepath.Join(a.home, "lab"), 0o700); err == nil {
			err = fileutil.WriteFileAtomic(filepath.Join(a.home, "lab", MarkerName), b, 0o600)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "crash: the marker was not written: %v\n", err)
		}
	}
	selfKill()
}

// ReportPrevious turns the marker a killed process left into a `crash` journal entry
// and renames it to crash-<n>.json, so it is reported once. No marker is no error.
func (a *Arms) ReportPrevious() error {
	path := filepath.Join(a.home, "lab", MarkerName)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("crash: reading the marker: %w", err)
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("crash: the marker %s is not valid: %w", path, err)
	}
	if a.journal != nil {
		a.journal.Append(labjournal.Entry{SimTime: m.SimTime, WallTime: m.WallTime, Kind: KindCrash, Source: "crash", Subject: m.Kind, Payload: b})
	}
	for n := 1; ; n++ {
		dst := filepath.Join(a.home, "lab", fmt.Sprintf("crash-%d.json", n))
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			return os.Rename(path, dst)
		}
	}
}
