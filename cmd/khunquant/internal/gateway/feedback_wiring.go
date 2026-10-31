package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// feedbackRuntime is the response recorder of one generation of services
// (proactive.learning). It exists only while learning is effectively on: proactive is
// enabled, a valid owner is configured, and the owner can be reached. Off, nothing is
// built, the table is not created, and no message carries a feedback button.
type feedbackRuntime struct {
	svc *feedback.Service
	ob  *attention.Outbox // whose governor reads the posteriors; cleared on stop

	snapshots *snapshot.Store // read by the hindsight job; nil when it has none
}

// feedbackButtons says whether the attention deliverer may put the feedback buttons on
// a message: true only while a recorder runs, so a button is never shown that nothing
// would record. Set by buildFeedback, cleared by stop.
var feedbackButtons atomic.Bool

// buildFeedback creates the recorder, subscribes it to the event bus (order fills,
// approval decisions) and starts its housekeeper. It returns nil when learning is off or
// the shared database is unavailable.
//
// With the recorder up it also hands the governor behind ob its learning settings
// (attentionLearning): the posteriors to read, the holdout rate and the candidate policy.
// Attention is built before this, so the governor is given them here.
func buildFeedback(d assemblyDeps, ns *notify.Service, ob *attention.Outbox, pr *perceptionRuntime) *feedbackRuntime {
	if !d.Cfg.Proactive.Effective().Learning {
		return nil
	}
	if ns == nil || !ns.CanDeliver() {
		logger.WarnCF("feedback", "proactive.learning is on but the owner cannot be reached: nothing is recorded", nil)
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.ErrorCF("feedback", "Feedback disabled: cannot open the proactive database", map[string]any{"error": err.Error()})
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := feedback.Options{Changes: intentChanges{db: db}, Clock: d.nowFn(), After: d.Shared.simAfter()}
	if profiles, err := openIntentStore(ctx, d, db); err == nil {
		opts.Zone = ownerZone(profiles)
	} else {
		logger.WarnCF("feedback", "Posteriors will use UTC hour buckets: cannot open the profile store",
			map[string]any{"error": err.Error()})
	}
	eb := d.Shared.eventBus()
	if eb != nil { // a nil *EventBus in the interface would not read as nil
		opts.Events = eb
	}
	svc, err := feedback.New(ctx, db, opts)
	if err != nil {
		logger.ErrorCF("feedback", "Feedback disabled: cannot build the recorder", map[string]any{"error": err.Error()})
		return nil
	}
	if err := svc.Start(context.Background(), eb); err != nil {
		logger.ErrorCF("feedback", "Feedback disabled: cannot start the recorder", map[string]any{"error": err.Error()})
		return nil
	}
	feedbackButtons.Store(true)
	rt := &feedbackRuntime{svc: svc, ob: ob}
	rt.startHindsight(d, pr)
	if ob != nil && !ob.SetLearning(attentionLearning(d.Cfg.Proactive, svc)) {
		logger.WarnCF("feedback", "Learning is not applied: the attention gate is not the governor", nil)
	}
	return rt
}

// ownerZone is the owner's time zone as attention reads it (the default profile's zone
// when the profile cannot be read, UTC when the zone cannot be loaded), so that the hour
// bucket a response is filed under is the bucket a later decision looks up.
func ownerZone(p *intent.Store) func(context.Context) *time.Location {
	return func(ctx context.Context) *time.Location {
		prof, err := p.Profile(ctx)
		if err != nil {
			prof = intent.DefaultProfile()
		}
		loc, _ := prof.Location()
		return loc
	}
}

// attentionLearning is the governor's learning configuration for an effectively-on
// proactive.learning: the posteriors of svc, the holdout rate, a production random
// source, and the candidate policy. Adapt is the effective switch (it needs learning).
func attentionLearning(pc config.ProactiveConfig, svc *feedback.Service) attention.Learning {
	eff := pc.Effective()
	return attention.Learning{
		Enabled:     eff.Learning,
		Adapt:       eff.LearningAdapt,
		HoldoutRate: pc.Learning.HoldoutRate,
		Candidate:   pc.Learning.CandidatePolicy,
		Source:      svc.Posteriors(),
		Rand:        attention.CryptoRand(),
	}
}

// intentChanges lists the changes made to price and indicator intents (a watch or a
// standing order is created, activated, paused, cancelled, superseded) since a time,
// from the intent ledger's event table, by the symbol the intent's trigger names. The
// ledger's own transitions (expiry, a spent budget) are not the owner acting.
type intentChanges struct{ db *sql.DB }

func (c intentChanges) ChangesSince(ctx context.Context, since time.Time) ([]feedback.Change, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT e.id, e.at_ms, i.trigger_json FROM intent_events e JOIN intents i ON i.id = e.intent_id
		  WHERE e.at_ms > ? AND i.trigger_json != '' AND e.by_sender NOT IN (?, ?) ORDER BY e.id LIMIT 200`,
		since.UnixMilli(), intent.SystemExpiry, intent.SystemBudget)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []feedback.Change
	for rows.Next() {
		var id, at int64
		var trig string
		if err := rows.Scan(&id, &at, &trig); err != nil {
			return nil, err
		}
		var spec struct {
			Params struct {
				Symbol string `json:"symbol"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(trig), &spec) != nil || spec.Params.Symbol == "" {
			continue
		}
		out = append(out, feedback.Change{
			Source: feedback.ChangeIntent, Subject: spec.Params.Symbol,
			Event: "intent-event:" + strconv.FormatInt(id, 10), At: time.UnixMilli(at),
		})
	}
	return out, rows.Err()
}

// stop takes the buttons away and stops the recorder. Safe on nil.
func (rt *feedbackRuntime) stop() {
	if rt == nil {
		return
	}
	feedbackButtons.Store(false)
	if rt.ob != nil {
		rt.ob.SetLearning(attention.Learning{}) // the posteriors go with the recorder
	}
	rt.svc.Stop() // also ends the hindsight job, which reads the snapshot store
	if rt.snapshots != nil {
		_ = rt.snapshots.Close()
	}
}

// feedbackActions returns the [Useful] [Not useful] [Snooze] [Mute] buttons for a message
// the attention outbox delivers to the owner, or none. have is how many buttons the
// message already carries.
//
// None when no recorder runs, when the message already has buttons (a digest with memory
// or reflection suggestions answers with those, which have their own: approval cards never
// pass through here) or when all four do not fit under bus.MaxActions. A message that
// has a protective candidate (margin, liquidation, stop-loss) gets only the two ratings:
// a snooze or a mute of it would silence the alert that exists to be heard, and it is not
// one tap away.
//
// The buttons carry no payload (the tap names the message it was on) and no text command,
// so a channel that cannot render buttons shows the message exactly as it was.
func feedbackActions(m attention.Message, have int) []bus.Action {
	if !feedbackButtons.Load() || m.InterventionID == "" || have > 0 {
		return nil
	}
	out := []bus.Action{
		{ID: "useful", Label: "Useful"},
		{ID: "not_useful", Label: "Not useful"},
	}
	protective := false
	for _, dec := range m.Decisions {
		protective = protective || dec.Candidate.Protective
	}
	if !protective {
		out = append(out, bus.Action{ID: "snooze_1h", Label: "Snooze 1h"}, bus.Action{ID: "mute", Label: "Mute"})
	}
	if bus.MaxActions-have < len(out) {
		return nil
	}
	return out
}
