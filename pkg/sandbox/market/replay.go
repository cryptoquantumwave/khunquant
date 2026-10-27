package market

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxReplayBars bounds one replay (a year of 1 m bars is about 525k).
const maxReplayBars = 1 << 20

// replayModel plays a candle CSV against sim time: the CSV's time axis is shifted by
// offset so that its start bar opens at the replay's start. Inside a bar the price moves
// linearly from open to close; between bars it holds the last close.
type replayModel struct {
	tf     time.Duration
	bars   []Bar         // CSV bars, Open on the CSV's own axis
	offset time.Duration // sim time = CSV time + offset
}

func (m *replayModel) Price(t time.Time) (float64, bool) {
	ct := t.Add(-m.offset)
	i := sort.Search(len(m.bars), func(i int) bool { return m.bars[i].Open.After(ct) })
	if i == 0 {
		return m.bars[0].O, true
	}
	b := m.bars[i-1]
	if el := ct.Sub(b.Open); el < m.tf {
		return b.O + (b.C-b.O)*float64(el)/float64(m.tf), true
	}
	return b.C, true
}

// Bars returns the CSV bars (shifted to sim time) that open in [from, to], verbatim, when
// tf is the CSV's own timeframe; any other tf is left to the sampled path.
func (m *replayModel) Bars(tf time.Duration, from, to time.Time) ([]Bar, bool) {
	if tf != m.tf {
		return nil, false
	}
	var out []Bar
	for _, b := range m.bars {
		b.Open = b.Open.Add(m.offset)
		if !b.Open.Before(from) && !b.Open.After(to) {
			out = append(out, b)
		}
	}
	return out, true
}

// ParseCSVTime reads unix seconds, unix milliseconds or RFC 3339.
func ParseCSVTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e11 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Parse(time.RFC3339, s)
}

// ParseCandles reads `ts,open,high,low,close[,volume]` rows (a header row is skipped),
// oldest first and strictly increasing. Volume is ignored: the book's is synthetic.
func ParseCandles(r io.Reader) ([]Bar, error) {
	cr := csv.NewReader(io.LimitReader(r, 64<<20))
	cr.FieldsPerRecord = -1
	var bars []Bar
	for row := 1; ; row++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv row %d: %w", row, err)
		}
		if len(rec) < 5 {
			return nil, fmt.Errorf("csv row %d: want ts,open,high,low,close, got %d fields", row, len(rec))
		}
		ts, err := ParseCSVTime(rec[0])
		if err != nil {
			if row == 1 {
				continue // a header
			}
			return nil, fmt.Errorf("csv row %d: bad ts %q", row, rec[0])
		}
		var v [4]float64
		for i := range v {
			if v[i], err = strconv.ParseFloat(strings.TrimSpace(rec[i+1]), 64); err != nil || !positive(v[i]) {
				return nil, fmt.Errorf("csv row %d: %q is not a positive number", row, rec[i+1])
			}
		}
		o, h, l, c := v[0], v[1], v[2], v[3]
		if h < math.Max(o, c) || l > math.Min(o, c) {
			return nil, fmt.Errorf("csv row %d: high/low do not contain open/close", row)
		}
		if n := len(bars); n > 0 && !ts.After(bars[n-1].Open) {
			return nil, fmt.Errorf("csv row %d: ts is not after the previous row", row)
		}
		if len(bars) >= maxReplayBars {
			return nil, fmt.Errorf("csv has more than %d rows", maxReplayBars)
		}
		bars = append(bars, Bar{Open: ts, O: o, H: h, L: l, C: c, V: 1})
	}
	if len(bars) == 0 {
		return nil, errors.New("csv has no candles")
	}
	return bars, nil
}

// Replay plays the CSV's candles for p from now: the bar containing start (zero: the
// first) opens at the current tf boundary and the rest follow. tf is the CSV's bar
// length. Bars past now are never served, and history before now is kept.
func (b *Book) Replay(venue string, p Pair, csv io.Reader, tf time.Duration, start time.Time) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if tf < time.Second {
		return errors.New("replay needs a timeframe of at least 1s")
	}
	bars, err := ParseCandles(csv)
	if err != nil {
		return err
	}
	first := 0
	if !start.IsZero() {
		first = max(sort.Search(len(bars), func(i int) bool { return bars[i].Open.After(start) })-1, 0)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.add(venue, p, &replayModel{tf: tf, bars: bars, offset: alignDown(b.now(), tf).Sub(bars[first].Open)})
	return nil
}
