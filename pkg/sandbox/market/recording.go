package market

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/replay"
)

// Recording plays the prices of a recorded events.jsonl (pkg/replay's input) against sim
// time: the market ticks of one provider:symbol become a step or linear price, and its
// closed candles, when the recording has them, are served as the bars themselves.

const (
	kindTick   = "perception.market.tick"
	kindCandle = "perception.market.candle_closed"
)

// RecordingOptions say how a recording is played. Align is the sim time of the first
// line of the file; a sim time t reads the recording at first + (t - Align) * Speed.
type RecordingOptions struct {
	Source      string    `json:"source"`                // "provider:symbol" as recorded
	Align       time.Time `json:"align"`                 // sim time of the first line
	Speed       float64   `json:"speed,omitempty"`       // 1 (default) = the recorded pace
	Interpolate string    `json:"interpolate,omitempty"` // "step" (default) | "linear"
}

type recTick struct {
	t  time.Time
	px float64
}

// recordingModel is one provider:symbol of a recording. file is its path relative to the
// instance home, which is how the saved world finds it again.
type recordingModel struct {
	file  string
	opt   RecordingOptions
	first time.Time
	ticks []recTick
	bars  map[time.Duration][]Bar // on the recording's own axis
}

// loadRecording reads path (inside the home; the caller checked) and builds the model.
func loadRecording(path, rel, symbol string, o RecordingOptions) (*recordingModel, error) {
	lines, err := replay.LoadEvents(path)
	if err != nil {
		return nil, err
	}
	return newRecording(lines, rel, symbol, o)
}

// newRecording builds the model of o.Source, or of the one provider that recorded symbol
// when Source is empty. rel names the file for the saved world.
func newRecording(lines []replay.Line, rel, symbol string, o RecordingOptions) (*recordingModel, error) {
	at := func(l replay.Line) time.Time {
		if l.Clock.IsZero() {
			return l.Event.Time
		}
		return l.Clock
	}
	series := map[string]*recordingModel{}
	get := func(provider, sym string) *recordingModel {
		k := provider + ":" + sym
		if series[k] == nil {
			series[k] = &recordingModel{bars: map[time.Duration][]Bar{}}
		}
		return series[k]
	}
	for _, l := range lines {
		raw, _ := json.Marshal(l.Event.Payload)
		switch string(l.Event.Kind) {
		case kindTick:
			var p struct {
				Provider, Symbol string
				Last             float64
			}
			if json.Unmarshal(raw, &p) == nil && positive(p.Last) {
				m := get(p.Provider, p.Symbol)
				m.ticks = append(m.ticks, recTick{at(l), p.Last})
			}
		case kindCandle:
			var p struct {
				Provider, Symbol, Timeframe string
				Bar                         struct {
					OpenTime               int64 `json:"open_time"`
					Open, High, Low, Close float64
				}
			}
			if json.Unmarshal(raw, &p) != nil || !positive(p.Bar.Close) {
				continue
			}
			if tf, err := time.ParseDuration(p.Timeframe); err == nil && tf > 0 {
				m := get(p.Provider, p.Symbol)
				m.bars[tf] = append(m.bars[tf], Bar{Open: time.UnixMilli(p.Bar.OpenTime).UTC(), O: p.Bar.Open, H: p.Bar.High, L: p.Bar.Low, C: p.Bar.Close, V: 1})
			}
		}
	}
	src := o.Source
	if src == "" {
		var keys []string
		for k := range series {
			if strings.HasSuffix(k, ":"+symbol) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) != 1 {
			return nil, fmt.Errorf("%s records %d providers of %s (%v): name one with source", rel, len(keys), symbol, keys)
		}
		src = keys[0]
	}
	m := series[src]
	if m == nil || len(m.ticks) == 0 {
		return nil, fmt.Errorf("%s holds no market tick of %s", rel, src)
	}
	if o.Interpolate != "" && o.Interpolate != "step" && o.Interpolate != "linear" {
		return nil, fmt.Errorf("interpolate must be step or linear, not %q", o.Interpolate)
	}
	if o.Speed < 0 || o.Speed > 1e6 {
		return nil, fmt.Errorf("speed %v is out of range", o.Speed)
	}
	o.Source = src
	m.file, m.opt, m.first = filepath.ToSlash(rel), o, at(lines[0])
	sort.SliceStable(m.ticks, func(i, j int) bool { return m.ticks[i].t.Before(m.ticks[j].t) })
	return m, nil
}

func (m *recordingModel) speed() float64 {
	if m.opt.Speed == 0 {
		return 1
	}
	return m.opt.Speed
}

// Price is the last tick at or before the recording's time for t; before the first tick
// the first, after the last the last (the recording has ended). linear interpolates.
func (m *recordingModel) Price(t time.Time) (float64, bool) {
	rt := m.first.Add(time.Duration(float64(t.Sub(m.opt.Align)) * m.speed()))
	i := sort.Search(len(m.ticks), func(i int) bool { return m.ticks[i].t.After(rt) })
	switch {
	case i == 0:
		return m.ticks[0].px, true
	case i == len(m.ticks) || m.opt.Interpolate != "linear":
		return m.ticks[i-1].px, true
	}
	a, b := m.ticks[i-1], m.ticks[i]
	return a.px + (b.px-a.px)*float64(rt.Sub(a.t))/float64(b.t.Sub(a.t)), true
}

// Bars returns the recorded closed bars of timeframe tf that open in [from, to]. It
// answers only at the recorded pace; any other case is sampled from Price.
func (m *recordingModel) Bars(tf time.Duration, from, to time.Time) ([]Bar, bool) {
	if m.speed() != 1 || len(m.bars[tf]) == 0 {
		return nil, false
	}
	var out []Bar
	for _, b := range m.bars[tf] {
		b.Open = b.Open.Add(m.opt.Align.Sub(m.first))
		if !b.Open.Before(from) && !b.Open.After(to) {
			out = append(out, b)
		}
	}
	return out, true
}

// SetRecording plays the recording at rel (a path under the book's home) for symbol on
// venue from now. The caller has confined rel to the home.
func (b *Book) SetRecording(venue, symbol, rel string, o RecordingOptions) error {
	p, err := ParsePair(symbol)
	if err != nil {
		return err
	}
	if venue, err = checkVenue(venue); err != nil {
		return err
	}
	if b.home == "" {
		return fmt.Errorf("recording needs an instance home")
	}
	if o.Align.IsZero() {
		o.Align = b.now()
	}
	m, err := loadRecording(filepath.Join(b.home, rel), rel, symbol, o)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.add(venue, p, m)
	return nil
}
