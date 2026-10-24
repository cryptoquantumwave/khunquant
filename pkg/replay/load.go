package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxLineBytes bounds one recorded line.
const maxLineBytes = 4 << 20

// ErrNoEvents is returned when the path holds no event: a replay of nothing would
// pass every check, so it is refused.
var ErrNoEvents = errors.New("replay: no events found")

// LoadEvents reads a recording: one file, or every *.jsonl in a directory in name
// order (the recorder names its files by day, so that is time order). Lines keep the
// order they were written in: that is the order the pipeline took them.
func LoadEvents(path string) ([]Line, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("replay: events: %w", err)
	}
	files := []string{path}
	if st.IsDir() {
		files, err = filepath.Glob(filepath.Join(path, "*.jsonl"))
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
	}
	var out []Line
	for _, f := range files {
		ls, err := readLines(f)
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w in %s", ErrNoEvents, path)
	}
	return out, nil
}

func readLines(name string) ([]Line, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("replay: events: %w", err)
	}
	defer f.Close()
	var out []Line
	r := bufio.NewReaderSize(f, 64<<10)
	for n := 1; ; n++ {
		b, err := r.ReadBytes('\n')
		if len(b) > maxLineBytes {
			return nil, fmt.Errorf("replay: %s:%d: line too long", name, n)
		}
		if b = bytes.TrimSpace(b); len(b) > 0 {
			var l Line
			if jerr := json.Unmarshal(b, &l); jerr != nil {
				return nil, fmt.Errorf("replay: %s:%d: %w", name, n, jerr)
			}
			if l.Event.Kind == "" || (l.Clock.IsZero() && l.Event.Time.IsZero()) {
				return nil, fmt.Errorf("replay: %s:%d: an event needs a kind and a time", name, n)
			}
			out = append(out, l)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("replay: events: %w", err)
		}
	}
}

// Label says what the owner should (or should not) have heard of between two times.
//
//	[{"match": "dn_health:12:margin_critical", "from": "2026-03-10T05:30:00Z",
//	  "to": "2026-03-10T05:40:00Z", "should_fire": true, "critical": true, "note": "..."}]
//
// Match is a dedup-key prefix or a subject. A send is a message the owner would have
// received (a notify or an escalate); a digest line is not one. Critical marks a
// should-fire label that must never be missed: critical recall is measured on those.
type Label struct {
	Match      string    `json:"match"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	ShouldFire bool      `json:"should_fire"`
	Critical   bool      `json:"critical,omitempty"`
	Note       string    `json:"note,omitempty"`
}

// LoadLabels reads a labels file.
func LoadLabels(name string) ([]Label, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("replay: labels: %w", err)
	}
	var ls []Label
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ls); err != nil {
		return nil, fmt.Errorf("replay: labels %s: %w", name, err)
	}
	for i, l := range ls {
		switch {
		case strings.TrimSpace(l.Match) == "":
			return nil, fmt.Errorf("replay: label %d has no match", i+1)
		case l.From.IsZero() || l.To.Before(l.From):
			return nil, fmt.Errorf("replay: label %d (%s) needs from <= to", i+1, l.Match)
		case l.Critical && !l.ShouldFire:
			return nil, fmt.Errorf("replay: label %d (%s) is critical but should not fire", i+1, l.Match)
		}
	}
	return ls, nil
}
