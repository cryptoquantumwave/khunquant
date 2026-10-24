package calendar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	// holidayDir is the workspace directory the holiday files live in.
	holidayDir = "calendar"

	// MaxHolidayFileBytes caps a holiday file. A real one is about a kilobyte; the
	// cap keeps a planted or runaway file from being read into memory.
	MaxHolidayFileBytes = 64 << 10

	maxHolidayYears   = 20
	maxHolidayEntries = 500
	minHolidayYear    = 2000
	maxHolidayYear    = 2100
)

// HolidayFile is the name of market m's holiday file under <workspace>/calendar/,
// or "" for a market that has none (crypto never closes).
func HolidayFile(m Market) string {
	switch m {
	case SET:
		return "set_holidays.json"
	case US:
		return "us_holidays.json"
	}
	return ""
}

// holidayList is one market's holidays: the years it covers and the closed days.
type holidayList struct {
	years map[int]struct{}
	days  map[int]struct{} // y*10000 + m*100 + d
}

type tableSet map[Market]*holidayList

func dayKey(y int, mo time.Month, d int) int { return y*10000 + int(mo)*100 + d }

// holidayFile is the on-disk format:
//
//	{
//	  "market": "set",                       optional; when present it must match the file
//	  "source": "https://...",               where the dates came from
//	  "retrieved": "2026-10-12",             when they were read there
//	  "years": [2026],                       the years the list covers: REQUIRED
//	  "note": "free text",
//	  "holidays": ["2026-01-01", ...]        REQUIRED; every date inside "years"
//	}
//
// "years" is explicit because coverage cannot be inferred from the dates: a year
// with no holiday on the list would look uncovered. Unknown fields are refused
// so that a misspelt "holidays" cannot read as "no holidays this year".
type holidayFile struct {
	Market    *string   `json:"market"`
	Source    string    `json:"source"`
	Retrieved string    `json:"retrieved"`
	Years     []int     `json:"years"`
	Note      string    `json:"note"`
	Holidays  *[]string `json:"holidays"`
}

// LoadHolidays reads <workspace>/calendar/set_holidays.json and us_holidays.json
// into the calendar, replacing whatever it held (it is how a reload re-arms it).
// Each market is loaded on its own: a bad SET file leaves the US list untouched.
//
// Nothing a file contains makes this fail. A missing file means no holidays for
// that market and a WARN that says how to get it; an oversized, malformed,
// duplicate-bearing or inconsistent file means the same, with the reason. The only
// error is an empty workspace path, which is a caller bug.
func (c *Calendar) LoadHolidays(workspace string) error {
	if workspace == "" {
		return errors.New("calendar: no workspace path")
	}
	next := tableSet{}
	for _, m := range []Market{SET, US} {
		path := filepath.Join(workspace, holidayDir, HolidayFile(m))
		data, err := readHolidayFile(path)
		if errors.Is(err, os.ErrNotExist) {
			logger.WarnCF("perception", "Holiday file is missing: no holidays are applied for this market, so it is "+
				"treated as open on every weekday. Copy "+filepath.ToSlash(filepath.Join("workspace", holidayDir, HolidayFile(m)))+
				" from the KhunQuant source or release into that path (nothing here overwrites a file you edited)",
				map[string]any{"market": string(m), "path": path})
			continue
		}
		var list *holidayList
		if err == nil {
			list, err = parseHolidays(m, data)
		}
		if err != nil {
			logger.WarnCF("perception", "Holiday file is unusable: no holidays are applied for this market",
				map[string]any{"market": string(m), "path": path, "error": err.Error()})
			continue
		}
		next[m] = list
	}
	c.tables.Store(&next)
	return nil
}

// readHolidayFile reads at most MaxHolidayFileBytes of a regular file. It does
// not open anything else (a FIFO or device planted at the path would block or
// stream forever) and does not trust the size it was told: the read itself is
// bounded.
func readHolidayFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > MaxHolidayFileBytes {
		return nil, fmt.Errorf("file is larger than %d bytes", MaxHolidayFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxHolidayFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxHolidayFileBytes {
		return nil, fmt.Errorf("file is larger than %d bytes", MaxHolidayFileBytes)
	}
	return data, nil
}

// parseHolidays validates a holiday file for market m. It never panics on any
// input.
func parseHolidays(m Market, data []byte) (*holidayList, error) {
	var f holidayFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not valid JSON for the holiday format: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON object")
	}
	if f.Market != nil && *f.Market != string(m) {
		return nil, fmt.Errorf("file is for market %q, not %q", *f.Market, m)
	}
	if len(f.Years) == 0 {
		return nil, errors.New(`"years" is missing or empty`)
	}
	if len(f.Years) > maxHolidayYears {
		return nil, fmt.Errorf(`"years" has more than %d entries`, maxHolidayYears)
	}
	if f.Holidays == nil {
		return nil, errors.New(`"holidays" is missing`)
	}
	if len(*f.Holidays) > maxHolidayEntries {
		return nil, fmt.Errorf(`"holidays" has more than %d entries`, maxHolidayEntries)
	}

	list := &holidayList{years: map[int]struct{}{}, days: map[int]struct{}{}}
	for _, y := range f.Years {
		if y < minHolidayYear || y > maxHolidayYear {
			return nil, fmt.Errorf("year %d is outside %d-%d", y, minHolidayYear, maxHolidayYear)
		}
		if _, dup := list.years[y]; dup {
			return nil, fmt.Errorf("year %d is listed twice", y)
		}
		list.years[y] = struct{}{}
	}
	for _, s := range *f.Holidays {
		t, err := time.Parse("2006-01-02", s)
		if err != nil || t.Format("2006-01-02") != s {
			return nil, fmt.Errorf("%s is not a YYYY-MM-DD date", strconv.Quote(s))
		}
		if _, ok := list.years[t.Year()]; !ok {
			return nil, fmt.Errorf("%s is outside the years the file says it covers", s)
		}
		k := dayKey(t.Year(), t.Month(), t.Day())
		if _, dup := list.days[k]; dup {
			return nil, fmt.Errorf("%s is listed twice", s)
		}
		list.days[k] = struct{}{}
	}
	return list, nil
}

// isHoliday reports whether the local calendar day is a listed holiday of m. A day
// in a year the list does not cover is not one, and says so once.
func (c *Calendar) isHoliday(m Market, y int, mo time.Month, d int) bool {
	tables := c.tables.Load()
	if tables == nil {
		return false
	}
	list := (*tables)[m]
	if list == nil {
		return false // no list for this market: the load already said why
	}
	if _, covered := list.years[y]; !covered {
		key := string(m) + "/" + strconv.Itoa(y)
		if _, seen := c.warned.LoadOrStore(key, struct{}{}); !seen {
			logger.WarnCF("perception", fmt.Sprintf("holiday list does not cover %d: its days are treated as normal trading days", y),
				map[string]any{"market": string(m), "year": y, "file": HolidayFile(m)})
		}
		return false
	}
	_, ok := list.days[dayKey(y, mo, d)]
	return ok
}
