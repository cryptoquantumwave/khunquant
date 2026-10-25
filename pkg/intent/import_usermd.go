package intent

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

var (
	// userMDLineRe matches a "- Key: value" list line.
	userMDLineRe = regexp.MustCompile(`^\s*[-*]\s+([^:]+?)\s*:\s*(.*?)\s*$`)
	// percentRe matches an allocation value such as "30%" or "12.5 %".
	percentRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*%$`)
	nonKeyRe  = regexp.MustCompile(`[^a-z0-9]+`)
)

// allocationAliases maps the labels of the USER.md template (and a few obvious
// variants) onto the keys the rest of the system uses. Any other label becomes
// a snake_case key of its own.
var allocationAliases = map[string]string{
	"crypto":              "crypto",
	"cryptocurrency":      "crypto",
	"thai equities":       "th_equity",
	"thai equity":         "th_equity",
	"thai stocks":         "th_equity",
	"th equities":         "th_equity",
	"th equity":           "th_equity",
	"us equities":         "us_equity",
	"us equity":           "us_equity",
	"us stocks":           "us_equity",
	"cash":                "cash",
	"cash / money market": "cash",
	"cash/money market":   "cash",
	"money market":        "cash",
}

// templateAllocationExample is the allocation list the shipped USER.md carries
// as an illustration ("Describe your target allocation, e.g.:"). An untouched
// copy of it is the owner not having answered, not an answer.
var templateAllocationExample = map[string]float64{"crypto": 30, "th_equity": 50, "cash": 20}

// ImportFromUserMD reads the "- Key: value" lines of the workspace USER.md
// template and returns the structured profile they describe, on top of the
// defaults. found is false when nothing usable was parsed (an unfilled
// template, an empty or unrelated file).
//
// Recognised: Timezone, Language (Thai / English), Risk tolerance, Investment
// horizon, Primary investment goal, and, under the "Portfolio Allocation
// Targets" heading, lines like "- Crypto: 30%". An empty value, or a
// placeholder wrapped in parentheses such as "(casual/formal)" or
// "(e.g., Asia/Bangkok)", is ignored. The allocation example that ships in the
// template is ignored as long as it is unchanged. A value that is present but
// invalid (a time zone that does not exist, a risk level that is not one of the
// three, targets that overshoot 100%) is dropped with a WARN, never an error.
//
// The error is for I/O problems only; a missing file wraps os.ErrNotExist.
func ImportFromUserMD(path string) (profile Profile, found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, false, fmt.Errorf("intent: read USER.md: %w", err)
	}
	profile, found = parseUserMD(string(data), path)
	return profile, found, nil
}

func parseUserMD(text, path string) (Profile, bool) {
	cur := DefaultProfile()
	found := false
	inAllocation := false
	targets := map[string]float64{}
	var targetOrder []string

	// try applies set to a copy of the profile and keeps it only when the
	// result is valid, so one bad line cannot spoil the others.
	try := func(label string, set func(*Profile)) {
		next := cur
		set(&next)
		norm, err := next.Normalize()
		if err != nil {
			logger.WarnCF("intent", "Ignoring unusable USER.md value",
				map[string]any{"path": path, "field": label, "error": err.Error()})
			return
		}
		cur, found = norm, true
	}

	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			inAllocation = strings.Contains(strings.ToLower(trimmed), "allocation")
			continue
		}
		m := userMDLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		label := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
		value := strings.TrimSpace(m[2])
		if isPlaceholder(value) {
			continue
		}

		if inAllocation {
			pm := percentRe.FindStringSubmatch(value)
			if pm == nil {
				continue
			}
			v, err := strconv.ParseFloat(pm[1], 64)
			if err != nil {
				continue
			}
			key, ok := allocationAliases[label]
			if !ok {
				key = strings.Trim(nonKeyRe.ReplaceAllString(label, "_"), "_")
			}
			if key == "" {
				continue
			}
			if _, dup := targets[key]; !dup {
				targetOrder = append(targetOrder, key)
			}
			targets[key] = v
			continue
		}

		switch label {
		case "timezone", "time zone":
			try("timezone", func(p *Profile) { p.Timezone = value })
		case "language":
			if lang, ok := parseLanguage(value); ok {
				try("language", func(p *Profile) { p.Language = lang })
			}
		case "risk tolerance":
			try("risk_tolerance", func(p *Profile) { p.RiskTolerance = strings.TrimRight(value, ". ") })
		case "investment horizon":
			try("horizon", func(p *Profile) { p.Horizon = value })
		case "primary investment goal":
			try("primary_goal", func(p *Profile) { p.PrimaryGoal = value })
		}
	}

	if len(targets) > 0 && !sameTargets(targets, templateAllocationExample) {
		// Add one target at a time so that a line that pushes the total past
		// 100% is dropped alone, not the whole list.
		for _, key := range targetOrder {
			v := targets[key]
			try("allocation_targets."+key, func(p *Profile) {
				next := make(map[string]float64, len(p.AllocationTargets)+1)
				for k, tv := range p.AllocationTargets {
					next[k] = tv
				}
				next[key] = v
				p.AllocationTargets = next
			})
		}
	}
	return cur, found
}

// isPlaceholder reports whether a USER.md value is template filler: empty, or
// text wrapped in parentheses.
func isPlaceholder(v string) bool {
	return v == "" || (strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")"))
}

// parseLanguage maps USER.md's wording onto "th"/"en". "both" says nothing
// usable, so it is not a language.
func parseLanguage(v string) (string, bool) {
	switch strings.ToLower(strings.TrimRight(strings.TrimSpace(v), ". ")) {
	case "thai", "th", "ภาษาไทย", "ไทย":
		return LanguageThai, true
	case "english", "en":
		return LanguageEnglish, true
	}
	return "", false
}

func sameTargets(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
