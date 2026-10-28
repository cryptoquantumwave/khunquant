// Package replay records the events the proactive pipeline takes and replays them
// through the real world model, triggers and attention under a fake clock, so that a
// change to a trigger, a threshold or an attention rule is tested on recorded data,
// deterministically, before it reaches the owner.
//
// Recording (this file) is opt-in (proactive.record_events) and keeps a deliberately
// small slice of what the pipeline sees: see Recordable for what is written and what
// is withheld. Replaying (harness.go) builds the pipeline with an in-memory database,
// a clock that only moves with the events and a notify that collects what it would
// have sent; it cannot place an order, call a tool or reach the network, and the
// import boundary test keeps it so.
package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// DirName is the recorder's directory under the workspace.
	DirName = "replay"
	// KeepFiles is how many daily files are kept.
	KeepFiles = 30

	dirMode  = 0o700
	fileMode = 0o600

	queueSize = 4096
	// subjectRunes bounds the one attribute that is kept.
	subjectRunes = 128
	warnEvery    = time.Minute
)

var fileRe = regexp.MustCompile(`^\d{8}\.jsonl$`)

// Line is one recorded event: the event as the pipeline took it and the engine clock
// at that moment. The day a line belongs to is the UTC date of Clock.
type Line struct {
	Clock time.Time    `json:"clock"`
	Event events.Event `json:"event"`
}

// recordable is the allowlist of event kinds whose payload is kept: structured market
// and portfolio data that an exchange returned or that code computed from it. Anything
// else (a placed or filled order, which names exchange orders; a DCA plan, which holds
// the owner's label; a device; a timer; an external trigger; news) is not recorded.
var recordable = map[events.Kind]bool{
	events.KindPerceptionMarketTick:         true,
	events.KindPerceptionMarketCandleClosed: true,
	events.KindPerceptionPortfolioSnapshot:  true,
	events.KindPerceptionFuturesPositions:   true,
	events.KindPerceptionPlanHealth:         true,
	events.KindPerceptionSourceHealth:       true,
	events.KindPerceptionClockDayStart:      true,
	events.KindPerceptionClockSession:       true,
}

// Recordable returns the form of ev that is written, and whether anything is.
//
// Withheld, so that no secret and no text from outside reaches the file:
//   - an event whose payload is private to the pipeline (an external trigger's
//     message, a news item) and an event below exchange trust (an unset trust counts
//     as untrusted): skipped whole, rather than written as an envelope that would
//     replay as an event with no payload;
//   - an event of a kind outside the allowlist: skipped;
//   - of the rest, the scope (sender, chat and message IDs), the correlation and every
//     attribute but the subject. The payload is what the event already carries for the
//     bus.
func Recordable(ev events.Event) (events.Event, bool) {
	if _, private := ev.Payload.(perception.PrivatePayload); private {
		return events.Event{}, false
	}
	if !recordable[ev.Kind] || ev.TrustLevel() < proactive.TrustExchange {
		return events.Event{}, false
	}
	out := events.Event{
		ID: ev.ID, Kind: ev.Kind, Time: ev.Time, Source: ev.Source, Severity: ev.Severity,
		Payload: ev.Payload, Trust: ev.Trust,
	}
	if s, ok := ev.Attrs["subject"].(string); ok && s != "" {
		if r := []rune(s); len(r) > subjectRunes {
			s = string(r[:subjectRunes])
		}
		out.Attrs = map[string]any{"subject": s}
	}
	return out, true
}

// Stats are the recorder's counters.
type Stats struct {
	Written, Skipped, Dropped, Failed uint64
}

// Recorder appends the recordable events to <dir>/<YYYYMMDD>.jsonl, one file per UTC
// day of the engine clock, keeping the newest KeepFiles. Record never blocks and
// never fails the event: a full queue drops the line (counted), a write error is
// counted and logged at most once a minute. Files are 0600, the directory 0700.
//
// The files hold market and portfolio data. They are not in any backup unless the
// owner copies them.
type Recorder struct {
	dir  string
	keep int
	ch   chan Line
	done chan struct{}

	written, skipped, dropped, failed atomic.Uint64
	pending                           atomic.Int32 // lines queued or being written (for Idle)

	closeOnce sync.Once
	mu        sync.RWMutex // guards closed against Record sending on ch
	closed    bool

	// The writer goroutine's own state.
	day      string
	f        *os.File
	lastWarn time.Time
}

// NewRecorder creates dir (0700, and chmod to 0700 when it already exists) and starts
// the writer.
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	r := &Recorder{dir: dir, keep: KeepFiles, ch: make(chan Line, queueSize), done: make(chan struct{})}
	go r.run()
	return r, nil
}

// Record implements engine.Recorder.
func (r *Recorder) Record(ev events.Event, at time.Time) {
	out, ok := Recordable(ev)
	if !ok {
		r.skipped.Add(1)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.dropped.Add(1)
		return
	}
	r.pending.Add(1) // before the send: a line the writer has taken is still counted
	select {
	case r.ch <- Line{Clock: at, Event: out}:
	default:
		r.pending.Add(-1)
		r.dropped.Add(1)
	}
}

// Idle is true when no line is queued or being written. It changes nothing; only the
// sandbox lab calls it.
func (r *Recorder) Idle() bool { return r == nil || r.pending.Load() == 0 }

// Stats returns the counters.
func (r *Recorder) Stats() Stats {
	return Stats{Written: r.written.Load(), Skipped: r.skipped.Load(), Dropped: r.dropped.Load(), Failed: r.failed.Load()}
}

// Close writes what is queued and closes the file. It is safe to call twice, and on
// a nil Recorder.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.ch)
		r.mu.Unlock()
	})
	<-r.done
}

func (r *Recorder) run() {
	defer close(r.done)
	for l := range r.ch {
		err := r.write(l)
		r.pending.Add(-1)
		if err != nil {
			r.failed.Add(1)
			r.warn(err)
			continue
		}
		r.written.Add(1)
	}
	if r.f != nil {
		_ = r.f.Close()
	}
}

func (r *Recorder) write(l Line) error {
	b, err := json.Marshal(l) // a NaN in a payload fails here, and costs only this line
	if err != nil {
		return err
	}
	if day := l.Clock.UTC().Format("20060102"); day != r.day || r.f == nil {
		if err := r.rotate(day); err != nil {
			return err
		}
	}
	_, err = r.f.Write(append(b, '\n'))
	return err
}

func (r *Recorder) rotate(day string) error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	f, err := os.OpenFile(filepath.Join(r.dir, day+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	r.f, r.day = f, day
	r.prune()
	return nil
}

// prune removes the oldest day files beyond keep. Only files named like a day file
// are touched.
func (r *Recorder) prune() {
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && fileRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > r.keep {
		_ = os.Remove(filepath.Join(r.dir, names[0]))
		names = names[1:]
	}
}

func (r *Recorder) warn(err error) {
	if now := time.Now(); now.Sub(r.lastWarn) >= warnEvery {
		r.lastWarn = now
		logger.WarnCF("replay", "Recording an event failed", map[string]any{"error": err.Error()})
	}
}
