package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// KindTimeAt is the trigger a reminder intent is compiled into: it fires when the
// clock source's timer with the spec's id comes due.
const KindTimeAt = "time_at"

const (
	// MaxTimeAtBudget bounds a time_at spec's fire budget (the ledger's bound).
	MaxTimeAtBudget = 1000
	maxCronLen      = 120
	maxZoneLen      = 64
)

// TimeAtParams are the parameters of a time_at spec. They describe WHEN, in the
// terms the intent compiler resolved them to; the timer itself lives on the clock
// source, which is told the same moment (or schedule) by the compiler.
//
// Exactly one of AtMS (a one-shot reminder: the absolute due time, unix
// milliseconds) and Cron (a recurring one: a five-field cron expression read in TZ)
// is set. Zone is the zone the due time is shown in (the owner's, or the cron
// expression's). UntilMS, when set, is the intent's expiry: a timer that comes due
// at or after it fires nothing. Budget is the intent's fire budget (0 is without
// limit); the trigger keeps its own count in its state, which is committed with the
// decision, so a reminder can never fire more often than the budget even while the
// ledger is catching up. Message is the intent's title: outside text, carried only
// in the candidate's Quote.
type TimeAtParams struct {
	AtMS    int64  `json:"at_ms,omitempty"`
	Cron    string `json:"cron,omitempty"`
	TZ      string `json:"tz,omitempty"`
	Zone    string `json:"zone,omitempty"`
	UntilMS int64  `json:"until_ms,omitempty"`
	Budget  int    `json:"budget,omitempty"`
	Message string `json:"message,omitempty"`
}

// Validate checks the parameters.
func (p TimeAtParams) Validate() error {
	switch {
	case (p.AtMS > 0) == (p.Cron != ""):
		return fmt.Errorf("exactly one of at_ms and cron must be set")
	case p.AtMS < 0 || p.UntilMS < 0:
		return fmt.Errorf("at_ms and until_ms must not be negative")
	case len(p.Cron) > maxCronLen || !plainText(p.Cron):
		return fmt.Errorf("cron must be plain text of at most %d characters", maxCronLen)
	case p.Cron == "" && p.TZ != "":
		return fmt.Errorf("tz is for a cron expression")
	case len(p.TZ) > maxZoneLen || !plainText(p.TZ) || len(p.Zone) > maxZoneLen || !plainText(p.Zone):
		return fmt.Errorf("tz and zone must be plain zone names of at most %d characters", maxZoneLen)
	case p.Budget < 0 || p.Budget > MaxTimeAtBudget:
		return fmt.Errorf("budget must be from 0 to %d", MaxTimeAtBudget)
	}
	return nil
}

// JSON encodes the parameters for a Spec.
func (p TimeAtParams) JSON() json.RawMessage {
	b, _ := json.Marshal(p) // plain fields: cannot fail
	return b
}

// ParseTimeAt decodes and validates the parameters of a time_at spec.
func ParseTimeAt(raw json.RawMessage) (TimeAtParams, error) {
	var p TimeAtParams
	if err := decodeStrict(raw, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// timeAtState is the part of a time_at trigger the registry keeps between events.
type timeAtState struct {
	Fired int `json:"fired,omitempty"`
}

// TimeAtFactory builds time_at triggers.
func TimeAtFactory(cfg CrossConfig) Factory {
	cfg = cfg.withStats()
	return func(spec Spec) (Trigger, error) {
		p, err := ParseTimeAt(spec.Params)
		if err != nil {
			return nil, err
		}
		loc := time.UTC
		if p.Zone != "" {
			if l, err := time.LoadLocation(p.Zone); err == nil {
				loc = l
			}
		}
		intentID := spec.IntentID
		if intentID == "" {
			intentID = spec.ID
		}
		return &timeAt{
			id: spec.Kind + ":" + spec.ID, specID: spec.ID, intentID: intentID, p: p, cfg: cfg, loc: loc,
			owner: OwnerAlertSpec(spec.Kind, spec.Origin),
		}, nil
	}
}

type timeAt struct {
	id, specID, intentID string
	p                    TimeAtParams
	cfg                  CrossConfig
	loc                  *time.Location
	owner                bool
}

func (t *timeAt) ID() string { return t.id }

func (t *timeAt) Kinds() []events.Kind { return []events.Kind{events.KindPerceptionClockTimer} }

// Demands: none. The timer is the clock source's.
func (t *timeAt) Demands() []perception.Demand { return nil }

// Evaluate reacts to the timer with this spec's id and to nothing else. It is a
// function of the event and its own state: a timer replayed after a restart that the
// state has already counted fires nothing a second time.
func (t *timeAt) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	pl, ok := d.Payload.(perception.TimerPayload)
	if !ok {
		return nil, nil
	}
	due := pl.TimerDue()
	if due.ID != t.specID || due.Due.IsZero() {
		return nil, nil
	}
	if t.p.UntilMS > 0 && due.Due.UnixMilli() >= t.p.UntilMS {
		return nil, nil // the intent has expired
	}
	var s timeAtState
	if len(st.Raw) > 0 {
		_ = json.Unmarshal(st.Raw, &s)
	}
	if t.p.Budget > 0 && s.Fired >= t.p.Budget {
		st.TurnOff = true
		return nil, nil
	}
	if due.Missed {
		// The moment is gone: say so, once. A missed reminder is not a fire.
		return []proactive.Candidate{t.candidate(due, v, true)}, nil
	}
	s.Fired++
	if raw, err := json.Marshal(s); err == nil {
		st.Raw, st.Dirty = raw, true
	}
	if t.p.AtMS > 0 || (t.p.Budget > 0 && s.Fired >= t.p.Budget) {
		st.TurnOff = true
	}
	return []proactive.Candidate{t.candidate(due, v, false)}, nil
}

func (t *timeAt) candidate(due perception.TimerDue, v worldmodel.View, missed bool) proactive.Candidate {
	thai := t.cfg.isThai()
	when := due.Due.In(t.loc).Format("2006-01-02 15:04 MST")
	var title, detail string
	switch {
	case missed && thai:
		title, detail = "พลาดการเตือนความจำ", "ถึงกำหนดเมื่อ "+when+" แต่ระบบไม่ได้ทำงานในเวลานั้น"
	case missed:
		title, detail = "Reminder missed", "It was due "+when+", while the assistant was not running"
	case thai:
		title, detail = "เตือนความจำ", "ถึงกำหนด "+when
	default:
		title, detail = "Reminder", "Due "+when
	}
	key := TimeAtKey(t.intentID, due.Due.UnixMilli())
	if missed {
		key = TimeAtMissedKey(t.intentID, due.Due.UnixMilli())
	}
	ev := map[string]any{"due_ms": due.Due.UnixMilli(), "late": due.Late, "missed": missed}
	return proactive.Candidate{
		DedupKey: key,
		Severity: proactive.SeverityNotice,
		Salience: 0.6,
		Title:    title,
		Detail:   detail,
		Quote:    CleanAlertMessage(t.p.Message),
		Evidence: ev,
		Trust:    proactive.TrustSystem,
		Compose:  proactive.ComposeTemplate,
		At:       v.Now(),
		// The owner asked to be reminded at this time (OwnerAlertSpec).
		OwnerRequested: t.owner,
	}
}

// TimeAtKey is time_at:<intent id>:<due time in unix milliseconds>.
func TimeAtKey(intentID string, dueUnixMilli int64) string {
	return fmt.Sprintf("%s:%s:%d", KindTimeAt, intentID, dueUnixMilli)
}

// TimeAtMissedKey is time_at:<intent id>:missed<due time in unix milliseconds>, the
// key of the one Notice that says a reminder's moment passed while nothing ran.
func TimeAtMissedKey(intentID string, dueUnixMilli int64) string {
	return fmt.Sprintf("%s:%s:missed%d", KindTimeAt, intentID, dueUnixMilli)
}

// IsTimeAtMissedKey reports whether key is a TimeAtMissedKey.
func IsTimeAtMissedKey(key string) bool {
	if !strings.HasPrefix(key, KindTimeAt+":") {
		return false
	}
	i := strings.LastIndexByte(key, ':')
	return i > 0 && strings.HasPrefix(key[i+1:], "missed")
}
