package simclock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what a restart needs to resume a Sim at the same sim time.
type State struct {
	Now        time.Time     `json:"now"`
	Mode       Mode          `json:"mode"`
	Scale      float64       `json:"scale,omitempty"`
	Resolution time.Duration `json:"resolution"`
}

// State is the current state.
func (s *Sim) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{Now: s.nowLocked(), Mode: s.mode, Scale: s.scale, Resolution: s.res}
}

// RestoreSim rebuilds a Sim from st. A scaled or realtime state resumes ticking from
// st.Now; an unknown mode or a negative resolution falls back to the defaults.
func RestoreSim(st State) *Sim {
	s := NewSim(st.Now)
	if st.Resolution >= 0 {
		s.res = st.Resolution
	}
	if st.Scale > 0 {
		s.scale = st.Scale
	}
	if st.Mode == ModeScaled || st.Mode == ModeRealtime {
		_ = s.SetMode(st.Mode)
	}
	return s
}

// SaveState writes st to path atomically (temp file in the same directory, then rename).
func SaveState(path string, st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("simclock: create state dir: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".clock-*.tmp")
	if err != nil {
		return fmt.Errorf("simclock: save state: %w", err)
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("simclock: save state: %w", firstErr(werr, cerr))
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("simclock: save state: %w", err)
	}
	return nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// LoadState reads a state file. A missing file is an error satisfying os.ErrNotExist.
func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("simclock: read state %s: %w", path, err)
	}
	if st.Now.IsZero() {
		return State{}, fmt.Errorf("simclock: state %s has no time", path)
	}
	return st, nil
}
