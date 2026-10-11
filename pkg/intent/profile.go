// Package intent holds what the owner has told KhunQuant about who they are
// and how they want to be treated, as typed values the rest of the proactive
// agent can act on: the time zone and active hours that quiet hours are judged
// in, the daily notification budget, the digest time, language, risk tolerance,
// allocation targets and assets to avoid. Later changes add intents and
// mandates to this package.
//
// # Time zones
//
// The binary does not embed the time zone database (no time/tzdata import), so
// time.LoadLocation relies on the host: $ZONEINFO, the system zoneinfo
// directories, or the Go toolchain's zoneinfo.zip. A host without any of them
// (a minimal container, a bare embedded image) cannot load "Asia/Bangkok", and
// Validate then reports an error naming the zone instead of silently using
// another one. Profile.Location falls back to UTC with ok=false in that case.
//
// # Zero versus unset
//
// A field is "unset" when its zero value means nothing. Defaults fill unset
// fields when a profile is read or normalised:
//
//   - Timezone "" is unset: Asia/Bangkok.
//   - ActiveHours with both Start and End empty is unset: 07:00-22:00. Setting
//     only one of the two is an error.
//   - DigestTime "" is unset: 08:00. There is no way to switch the digest off
//     yet; a later change that adds one must add an explicit marker.
//   - NotificationBudgetPerDay: 0 is a real value, "send me nothing that is not
//     critical". Whether it is set is recorded in the stored JSON (the field is
//     written as a number, absent means unset and reads as 3); PutProfile always
//     writes it, so a stored 0 stays 0.
//   - Language, RiskTolerance, Horizon, PrimaryGoal, AllocationTargets and
//     AssetsToAvoid have no default: empty means "not said".
//
// Because Profile.NotificationBudgetPerDay is a plain int, a Profile literal
// handed to PutProfile is taken as complete and its 0 is a real 0. Start from
// DefaultProfile() or from Store.Profile when you mean "the defaults, but".
//
// # Untrusted text
//
// Horizon and PrimaryGoal are free text and may end up in prompts, so Normalize
// strips control characters, joins the text onto one line and caps it. The
// prompt line (Profile.PromptLine) deliberately leaves them out.
package intent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Defaults and limits.
const (
	DefaultTimezone     = "Asia/Bangkok"
	DefaultActiveStart  = "07:00"
	DefaultActiveEnd    = "22:00"
	DefaultBudgetPerDay = 3
	DefaultDigestTime   = "08:00"

	MaxBudgetPerDay  = 20
	MaxFreeTextRunes = 200
	MaxAssetsToAvoid = 100
	MaxAssetRunes    = 20
	MaxTargets       = 20
	MaxTargetKeyLen  = 32
	maxTimezoneLen   = 64

	// targetEpsilon absorbs float error when targets that add up to 100 are
	// summed (33.3 + 33.3 + 33.4).
	targetEpsilon = 1e-6
)

// Language and risk vocabulary.
const (
	LanguageThai    = "th"
	LanguageEnglish = "en"

	RiskConservative = "conservative"
	RiskModerate     = "moderate"
	RiskAggressive   = "aggressive"
)

// HoursRange is a local-time window in zero-padded 24-hour "HH:MM". When Start
// is later than End the window runs overnight (22:00-06:00): it contains the
// minutes from Start to midnight and from midnight to End. Start is inclusive,
// End exclusive.
type HoursRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Profile is who the owner is and how they want to be treated.
type Profile struct {
	Timezone                 string             `json:"timezone"`                    // IANA, default Asia/Bangkok
	ActiveHours              HoursRange         `json:"active_hours"`                // default 07:00-22:00 local
	Language                 string             `json:"language"`                    // "th" | "en" | ""
	RiskTolerance            string             `json:"risk_tolerance"`              // conservative|moderate|aggressive|""
	Horizon                  string             `json:"horizon"`                     // free text
	PrimaryGoal              string             `json:"primary_goal"`                // free text
	AllocationTargets        map[string]float64 `json:"allocation_targets"`          // "crypto","th_equity","us_equity","cash" -> %
	AssetsToAvoid            []string           `json:"assets_to_avoid"`             // upper-case symbols
	NotificationBudgetPerDay int                `json:"notification_budget_per_day"` // default 3, 0 allowed
	DigestTime               string             `json:"digest_time"`                 // "08:00" local
}

// DefaultProfile returns the profile of an owner who has said nothing.
func DefaultProfile() Profile {
	return Profile{
		Timezone:                 DefaultTimezone,
		ActiveHours:              HoursRange{Start: DefaultActiveStart, End: DefaultActiveEnd},
		AllocationTargets:        map[string]float64{},
		AssetsToAvoid:            []string{},
		NotificationBudgetPerDay: DefaultBudgetPerDay,
		DigestTime:               DefaultDigestTime,
	}
}

var (
	hhmmRe      = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	assetRe     = regexp.MustCompile(`^[A-Z0-9._/-]+$`)
	targetKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// resolve fills unset fields with their defaults and gives the collection
// fields a non-nil value. It validates nothing.
func (p Profile) resolve() Profile {
	d := DefaultProfile()
	p.Timezone = strings.TrimSpace(p.Timezone)
	if p.Timezone == "" {
		p.Timezone = d.Timezone
	}
	p.ActiveHours.Start = strings.TrimSpace(p.ActiveHours.Start)
	p.ActiveHours.End = strings.TrimSpace(p.ActiveHours.End)
	if p.ActiveHours.Start == "" && p.ActiveHours.End == "" {
		p.ActiveHours = d.ActiveHours
	}
	p.DigestTime = strings.TrimSpace(p.DigestTime)
	if p.DigestTime == "" {
		p.DigestTime = d.DigestTime
	}
	if p.AllocationTargets == nil {
		p.AllocationTargets = map[string]float64{}
	}
	if p.AssetsToAvoid == nil {
		p.AssetsToAvoid = []string{}
	}
	return p
}

// Validate reports whether the profile is acceptable. It is Normalize without
// the result: surrounding whitespace, letter case and duplicate assets are
// tolerated and cleaned up, anything else wrong is an error that names the
// field.
func (p Profile) Validate() error {
	_, err := p.Normalize()
	return err
}

// Normalize returns the cleaned, defaults-filled copy of p that PutProfile
// stores, or the first thing wrong with it. The receiver is not modified.
func (p Profile) Normalize() (Profile, error) {
	p = p.resolve()

	if err := checkTimezone(p.Timezone); err != nil {
		return Profile{}, err
	}
	if err := checkHours(p.ActiveHours); err != nil {
		return Profile{}, err
	}
	if !hhmmRe.MatchString(p.DigestTime) {
		return Profile{}, fmt.Errorf("digest_time %q must be zero-padded 24-hour HH:MM (for example \"08:00\")", p.DigestTime)
	}

	p.Language = strings.ToLower(strings.TrimSpace(p.Language))
	switch p.Language {
	case "", LanguageThai, LanguageEnglish:
	default:
		return Profile{}, fmt.Errorf("language %q must be \"th\", \"en\" or empty", p.Language)
	}
	p.RiskTolerance = strings.ToLower(strings.TrimSpace(p.RiskTolerance))
	switch p.RiskTolerance {
	case "", RiskConservative, RiskModerate, RiskAggressive:
	default:
		return Profile{}, fmt.Errorf("risk_tolerance %q must be conservative, moderate, aggressive or empty", p.RiskTolerance)
	}

	var err error
	if p.Horizon, err = cleanFreeText("horizon", p.Horizon); err != nil {
		return Profile{}, err
	}
	if p.PrimaryGoal, err = cleanFreeText("primary_goal", p.PrimaryGoal); err != nil {
		return Profile{}, err
	}
	if p.AllocationTargets, err = normalizeTargets(p.AllocationTargets); err != nil {
		return Profile{}, err
	}
	if p.AssetsToAvoid, err = normalizeAssets(p.AssetsToAvoid); err != nil {
		return Profile{}, err
	}
	if p.NotificationBudgetPerDay < 0 || p.NotificationBudgetPerDay > MaxBudgetPerDay {
		return Profile{}, fmt.Errorf("notification_budget_per_day %d must be between 0 and %d",
			p.NotificationBudgetPerDay, MaxBudgetPerDay)
	}
	return p, nil
}

func checkTimezone(name string) error {
	if len(name) > maxTimezoneLen {
		return fmt.Errorf("timezone is longer than %d characters", maxTimezoneLen)
	}
	if strings.EqualFold(name, "local") {
		return errors.New(`timezone "Local" depends on the host; use an IANA name such as "Asia/Bangkok"`)
	}
	if _, err := loadLocation(name); err != nil {
		return fmt.Errorf("timezone %q could not be loaded (use an IANA name such as \"Asia/Bangkok\"): %w", name, err)
	}
	return nil
}

func checkHours(h HoursRange) error {
	if !hhmmRe.MatchString(h.Start) {
		return fmt.Errorf("active_hours.start %q must be zero-padded 24-hour HH:MM (for example \"07:00\")", h.Start)
	}
	if !hhmmRe.MatchString(h.End) {
		return fmt.Errorf("active_hours.end %q must be zero-padded 24-hour HH:MM (for example \"22:00\")", h.End)
	}
	if h.Start == h.End {
		return fmt.Errorf("active_hours start and end are both %q: a window needs a start different from its end", h.Start)
	}
	return nil
}

// cleanFreeText makes untrusted text safe to keep: control and invisible
// format characters (newlines, tabs, bidi overrides, zero-width marks) become
// spaces, runs of whitespace collapse, and the result must fit the cap.
func cleanFreeText(field, s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s is not valid UTF-8", field)
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if n := utf8.RuneCountInString(s); n > MaxFreeTextRunes {
		return "", fmt.Errorf("%s is %d characters; the limit is %d", field, n, MaxFreeTextRunes)
	}
	return s, nil
}

func normalizeTargets(in map[string]float64) (map[string]float64, error) {
	out := make(map[string]float64, len(in))
	if len(in) > MaxTargets {
		return nil, fmt.Errorf("allocation_targets has %d entries; the limit is %d", len(in), MaxTargets)
	}
	// Walk in sorted order so the error for a bad profile does not depend on
	// map iteration order.
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sum float64
	for _, raw := range keys {
		v := in[raw]
		key := strings.ToLower(strings.TrimSpace(raw))
		if len(key) > MaxTargetKeyLen || !targetKeyRe.MatchString(key) {
			return nil, fmt.Errorf("allocation_targets key %q must be 1-%d characters of a-z, 0-9, '_', '.' or '-'",
				raw, MaxTargetKeyLen)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("allocation_targets[%q] is not a finite number", key)
		}
		if v < 0 || v > 100 {
			return nil, fmt.Errorf("allocation_targets[%q] = %v must be between 0 and 100", key, v)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("allocation_targets lists %q more than once (keys are case-insensitive)", key)
		}
		out[key] = v
		sum += v
	}
	if sum > 100+targetEpsilon {
		return nil, fmt.Errorf("allocation_targets add up to %v%%; they must not exceed 100%%", sum)
	}
	return out, nil
}

func normalizeAssets(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		a := strings.ToUpper(strings.TrimSpace(raw))
		switch {
		case a == "":
			return nil, errors.New("assets_to_avoid contains an empty entry")
		case utf8.RuneCountInString(a) > MaxAssetRunes:
			return nil, fmt.Errorf("assets_to_avoid entry %q is longer than %d characters", truncateForError(raw), MaxAssetRunes)
		case !assetRe.MatchString(a):
			return nil, fmt.Errorf("assets_to_avoid entry %q may contain only A-Z, 0-9, '.', '_', '/' and '-'", truncateForError(raw))
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) > MaxAssetsToAvoid {
		return nil, fmt.Errorf("assets_to_avoid has %d entries; the limit is %d", len(out), MaxAssetsToAvoid)
	}
	return out, nil
}

func truncateForError(s string) string {
	const max = 40
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "..."
}

// ---- Time helpers ----------------------------------------------------------

var (
	locMu    sync.Mutex
	locCache = map[string]*time.Location{}
)

// loadLocation is time.LoadLocation with a cache: the governor asks on every
// decision and LoadLocation reads zoneinfo from disk each time. Only
// successful loads are cached.
func loadLocation(name string) (*time.Location, error) {
	locMu.Lock()
	defer locMu.Unlock()
	if l, ok := locCache[name]; ok {
		return l, nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	locCache[name] = l
	return l, nil
}

// Location returns the profile's time zone. When the zone is empty, the
// default is used; when it cannot be loaded (a typo in a hand-edited row, a host
// without zoneinfo) the result is UTC with ok=false so callers can log and carry
// on rather than fail.
func (p Profile) Location() (loc *time.Location, ok bool) {
	name := strings.TrimSpace(p.Timezone)
	if name == "" {
		name = DefaultTimezone
	}
	if strings.EqualFold(name, "local") {
		return time.UTC, false
	}
	l, err := loadLocation(name)
	if err != nil {
		return time.UTC, false
	}
	return l, true
}

// minutesOfDay parses a zero-padded HH:MM.
func minutesOfDay(s string) (int, bool) {
	if !hhmmRe.MatchString(s) {
		return 0, false
	}
	return int(s[0]-'0')*600 + int(s[1]-'0')*60 + int(s[3]-'0')*10 + int(s[4]-'0'), true
}

// InActiveHours reports whether t falls inside the active window, judged on the
// wall clock of the profile's time zone. Start is inclusive and End exclusive;
// an overnight window (Start after End) contains the minutes from Start to
// midnight and from midnight to End. Seconds are ignored: 21:59:59 is minute
// 21:59. A profile whose hours do not parse is treated as the default window
// rather than as "always quiet" or "always active".
func (p Profile) InActiveHours(t time.Time) bool {
	loc, _ := p.Location()
	start, ok1 := minutesOfDay(p.ActiveHours.Start)
	end, ok2 := minutesOfDay(p.ActiveHours.End)
	if !ok1 || !ok2 || start == end {
		start, _ = minutesOfDay(DefaultActiveStart)
		end, _ = minutesOfDay(DefaultActiveEnd)
	}
	lt := t.In(loc)
	m := lt.Hour()*60 + lt.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// LocalDay returns the calendar day of t in the profile's time zone as
// "2006-01-02": the key a per-day notification budget is counted against.
func (p Profile) LocalDay(t time.Time) string {
	loc, _ := p.Location()
	return t.In(loc).Format("2006-01-02")
}

// ---- Prompt rendering ------------------------------------------------------

// PromptLine renders the profile as one compact line for the system prompt.
// Only fields with a closed vocabulary or a strict character set are included;
// the free-text fields (horizon, primary goal) are left out, and every value is
// re-checked here rather than trusted, so a hand-edited row cannot put arbitrary
// text into a prompt.
func (p Profile) PromptLine() string {
	p = p.resolve()
	var parts []string
	if tz := p.Timezone; len(tz) <= maxTimezoneLen && !strings.EqualFold(tz, "local") && tzNameRe.MatchString(tz) {
		parts = append(parts, "time zone "+tz)
	}
	if checkHours(p.ActiveHours) == nil {
		parts = append(parts, fmt.Sprintf("active hours %s-%s", p.ActiveHours.Start, p.ActiveHours.End))
	}
	switch p.Language {
	case LanguageThai, LanguageEnglish:
		parts = append(parts, "language "+p.Language)
	}
	switch p.RiskTolerance {
	case RiskConservative, RiskModerate, RiskAggressive:
		parts = append(parts, "risk "+p.RiskTolerance)
	}
	if t := promptTargets(p.AllocationTargets); t != "" {
		parts = append(parts, "targets "+t)
	}
	if p.NotificationBudgetPerDay >= 0 && p.NotificationBudgetPerDay <= MaxBudgetPerDay {
		parts = append(parts, fmt.Sprintf("notification budget %d/day", p.NotificationBudgetPerDay))
	}
	if len(parts) == 0 {
		return ""
	}
	return "User profile: " + strings.Join(parts, "; ")
}

var tzNameRe = regexp.MustCompile(`^[A-Za-z0-9_+/-]+$`)

func promptTargets(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if len(k) <= MaxTargetKeyLen && targetKeyRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > MaxTargets {
		keys = keys[:MaxTargets]
	}
	items := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			continue
		}
		items = append(items, fmt.Sprintf("%s %s%%", k, trimFloat(v)))
	}
	return strings.Join(items, ", ")
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// ---- Patching --------------------------------------------------------------

// patchFields lists the JSON keys ApplyPatch accepts, in display order.
var patchFields = []string{
	"timezone", "active_hours", "language", "risk_tolerance", "horizon",
	"primary_goal", "allocation_targets", "assets_to_avoid",
	"notification_budget_per_day", "digest_time",
}

// FieldNames returns the JSON names of the profile fields a patch may carry, in
// display order.
func FieldNames() []string {
	return append([]string(nil), patchFields...)
}

// ApplyPatch applies a partial JSON update to p and returns the normalised
// result, leaving p untouched. patch must be a JSON object:
//
//   - only the fields present change;
//   - a field set to null goes back to its default (empty where there is none);
//   - active_hours may carry just "start" or just "end";
//   - allocation_targets and assets_to_avoid are replaced as a whole, not merged;
//   - a field that is not part of the profile is an error naming it.
//
// The merged profile is validated; an invalid one is an error and nothing is
// applied.
func (p Profile) ApplyPatch(patch []byte) (Profile, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patch, &fields); err != nil || fields == nil {
		return Profile{}, errors.New("the patch must be a JSON object such as {\"notification_budget_per_day\": 2}")
	}
	if len(fields) == 0 {
		return Profile{}, errors.New("the patch has no fields; valid fields: " + strings.Join(patchFields, ", "))
	}
	known := make(map[string]struct{}, len(patchFields))
	for _, f := range patchFields {
		known[f] = struct{}{}
	}
	var unknown []string
	for k := range fields {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Profile{}, fmt.Errorf("unknown profile field(s): %s; valid fields: %s",
			strings.Join(unknown, ", "), strings.Join(patchFields, ", "))
	}

	d := DefaultProfile()
	out := p.resolve()
	for _, name := range patchFields {
		raw, present := fields[name]
		if !present {
			continue
		}
		isNull := string(bytes.TrimSpace(raw)) == "null"
		var err error
		switch name {
		case "timezone":
			err = patchString(&out.Timezone, d.Timezone, raw, isNull)
		case "language":
			err = patchString(&out.Language, "", raw, isNull)
		case "risk_tolerance":
			err = patchString(&out.RiskTolerance, "", raw, isNull)
		case "horizon":
			err = patchString(&out.Horizon, "", raw, isNull)
		case "primary_goal":
			err = patchString(&out.PrimaryGoal, "", raw, isNull)
		case "digest_time":
			err = patchString(&out.DigestTime, d.DigestTime, raw, isNull)
		case "active_hours":
			err = patchHours(&out.ActiveHours, d.ActiveHours, raw, isNull)
		case "allocation_targets":
			out.AllocationTargets = map[string]float64{}
			if !isNull {
				err = json.Unmarshal(raw, &out.AllocationTargets)
			}
		case "assets_to_avoid":
			out.AssetsToAvoid = []string{}
			if !isNull {
				err = json.Unmarshal(raw, &out.AssetsToAvoid)
			}
		case "notification_budget_per_day":
			if isNull {
				out.NotificationBudgetPerDay = d.NotificationBudgetPerDay
			} else {
				err = json.Unmarshal(raw, &out.NotificationBudgetPerDay)
			}
		}
		if err != nil {
			return Profile{}, fmt.Errorf("field %q has the wrong shape: %s", name, shortJSONError(err))
		}
	}
	return out.Normalize()
}

func patchString(dst *string, def string, raw json.RawMessage, isNull bool) error {
	if isNull {
		*dst = def
		return nil
	}
	return json.Unmarshal(raw, dst)
}

func patchHours(dst *HoursRange, def HoursRange, raw json.RawMessage, isNull bool) error {
	if isNull {
		*dst = def
		return nil
	}
	var in struct {
		Start *string `json:"start"`
		End   *string `json:"end"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return err
	}
	if in.Start == nil && in.End == nil {
		return errors.New(`give "start" and/or "end", for example {"end": "21:00"}`)
	}
	if in.Start != nil {
		dst.Start = strings.TrimSpace(*in.Start)
	}
	if in.End != nil {
		dst.End = strings.TrimSpace(*in.End)
	}
	return nil
}

// shortJSONError trims encoding/json's Go-type wording to something a model can
// act on.
func shortJSONError(err error) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("expected %s but got a JSON %s", ute.Type, ute.Value)
	}
	return err.Error()
}
