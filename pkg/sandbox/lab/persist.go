package lab

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Persistent is a part of the simulated world that outlives the gateway process, as a
// real venue outlives its client: the exchange state, the market book and the fault rules.
// The state is one JSON document, saved under <home>/lab/world/<StateKey>.json.
type Persistent interface {
	StateKey() string // ^[a-z][a-z_]*$: the file name
	MarshalState() ([]byte, error)
	RestoreState(data []byte) error
}

// keyedPersistent is a part with bookkeeping that moves on its own (a clock mark that
// advances with every step but means nothing a client can see). MarshalStateKeyed returns the
// whole state, which is what is written, and a key that leaves that bookkeeping out, which is
// what decides whether anything changed. Both come from one instant, so the key always
// describes the bytes next to it.
type keyedPersistent interface {
	MarshalStateKeyed() (state, key []byte, err error)
}

// errNotReady: the part is not registered or not restored yet (the lab is being built).
var errNotReady = errors.New("lab: the part is not registered or not restored yet")

// persisted is one part of the saved world. Its mutex orders that part's saves and its restore
// and is never held together with another part's or with pmu: a save marshals under it, and
// marshalling the exchange takes the simulator's lock, which a request that is firing a fault
// rule holds while it saves the fault rules. One lock for all parts would make those two wait
// for each other.
type persisted struct {
	p        Persistent
	mu       sync.Mutex
	last     []byte // what the file holds (or, for a keyed part, its key), so an unchanged state is not written again
	restored bool
}

// WorldDir is where the saved world lives: inside <home>/lab, which the instance's own
// file tools cannot reach (they are confined to <home>/workspace).
func (l *Lab) WorldDir() string { return filepath.Join(l.Home, "lab", "world") }

// Register adds p to the saved world. It does not read its file: Restore does.
func (l *Lab) Register(p Persistent) {
	l.pmu.Lock()
	defer l.pmu.Unlock()
	if !nameRE.MatchString(p.StateKey()) {
		panic(fmt.Sprintf("lab: persistent key %q is not a plain name", p.StateKey()))
	}
	l.world = append(l.world, &persisted{p: p})
}

func (l *Lab) worldFile(key string) string { return filepath.Join(l.WorldDir(), key+".json") }

// Persist writes the current state of the part called key, durably (temp file, fsync,
// rename, fsync of the directory). It marshals under the lock that orders writes, so the
// file always ends up holding the newest state; a state equal to the file is not written.
func (l *Lab) Persist(key string) error {
	e := l.part(key)
	if e == nil {
		return fmt.Errorf("lab: no persistent part %q: %w", key, errNotReady)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.restored { // saving the fresh state would erase the saved one
		return fmt.Errorf("lab: %s is saved before it was restored: %w", key, errNotReady)
	}
	b, cmp, err := marshalPart(e.p)
	if err != nil {
		return fmt.Errorf("lab: saving %s: %w", key, err)
	}
	if bytes.Equal(cmp, e.last) {
		return nil
	}
	if err := os.MkdirAll(l.WorldDir(), 0o700); err != nil {
		return fmt.Errorf("lab: saving %s: %w", key, err)
	}
	if err := fileutil.WriteFileAtomic(l.worldFile(key), b, 0o600); err != nil {
		return fmt.Errorf("lab: saving %s: %w", key, err)
	}
	e.last = cmp
	return nil
}

// marshalPart is the bytes to write and the bytes that decide whether to write them.
func marshalPart(p Persistent) (state, cmp []byte, err error) {
	if k, ok := p.(keyedPersistent); ok {
		return k.MarshalStateKeyed()
	}
	b, err := p.MarshalState()
	return b, b, err
}

// part is the registered part called key (nil if none).
func (l *Lab) part(key string) *persisted {
	l.pmu.Lock()
	defer l.pmu.Unlock()
	for _, e := range l.world {
		if e.p.StateKey() == key {
			return e
		}
	}
	return nil
}

// Restore loads every registered part that has not been restored yet. A part with no file
// keeps its fresh state; one whose file cannot be read or does not fit it is an error,
// never a silent reset: the lab then does not start on a half-remembered world.
func (l *Lab) Restore() error {
	l.pmu.Lock()
	parts := slices.Clone(l.world)
	l.pmu.Unlock()
	for _, e := range parts {
		if err := l.restore(e); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lab) restore(e *persisted) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.restored {
		return nil
	}
	key := e.p.StateKey()
	b, err := os.ReadFile(l.worldFile(key))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("lab: reading the saved %s: %w", key, err)
	default:
		if err := e.p.RestoreState(b); err != nil {
			return fmt.Errorf("lab: the saved %s (%s) is not usable: %w", key, l.worldFile(key), err)
		}
		// What the part would save now decides what counts as unchanged. Failing to
		// marshal it just means the first save writes.
		if _, cmp, err := marshalPart(e.p); err == nil {
			e.last = cmp
		}
	}
	e.restored = true
	return nil
}

// persistAll saves every part that changed. Marshalling a part is cheap and an unchanged
// one is not written, so a group op does not need to know which parts it touched.
func (l *Lab) persistAll() {
	l.pmu.Lock()
	keys := make([]string, 0, len(l.world))
	for _, e := range l.world {
		keys = append(keys, e.p.StateKey())
	}
	l.pmu.Unlock()
	for _, k := range keys { // one at a time: no part's lock is held while another is taken
		l.PersistQuiet(k)
	}
}

// PersistQuiet is Persist for a caller that has nowhere to return the error (a clock step,
// a fault firing): a failure is logged. A part not registered or not restored yet is not
// worth a line: that happens only while the lab is being built.
func (l *Lab) PersistQuiet(key string) {
	if err := l.Persist(key); err != nil && !errors.Is(err, errNotReady) {
		logger.WarnCF("sandbox", "Saving the simulated world failed", map[string]any{"part": key, "error": err.Error()})
	}
}

// persistingSim is the simulator as the orders stage: whatever it answers is saved before
// the answer leaves, so a client that got a response can rely on the venue remembering it
// across a kill. Every answer counts, reads too: Webull's listings and a fault's firing
// count change state on a GET.
type persistingSim struct {
	sim *sandbox.StatefulSimulator
	l   *Lab
}

func (p persistingSim) Respond(venue, method, path string, r *http.Request) (*sandbox.Response, bool) {
	resp, ok := p.sim.Respond(venue, method, path, r)
	if ok {
		p.l.PersistQuiet("exchange")
	}
	return resp, ok
}
