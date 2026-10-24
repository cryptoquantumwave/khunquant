package feedback

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Sources of the responses this file records.
const (
	SourceOrderFill = "order_fill"
	SourceApproval  = "approval"
	SourceHousekeep = "housekeeper"
	// ChangePlan and ChangeIntent name the source of an ObserveChange.
	ChangePlan   = "plan_change"
	ChangeIntent = "intent_change"
)

// approvalWorkflow is the journal workflow of an approved order (approval.WorkflowKind).
const approvalWorkflow = "approval.order"

// Start subscribes to the event bus (fills, approval decisions) and runs the housekeeper
// until Stop. A nil bus subscribes to nothing. Start is called once.
func (s *Service) Start(ctx context.Context, bus *events.EventBus) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bus != nil {
		sub, err := bus.Channel().
			OfKind(events.KindPerceptionOrderFilled, events.KindApprovalDecided, events.KindApprovalExpired).
			Subscribe(ctx, events.SubscribeOptions{Name: "feedback", Buffer: 64, Timeout: 2 * dbTimeout}, s.onEvent)
		if err != nil {
			return fmt.Errorf("feedback: subscribe: %w", err)
		}
		s.sub = sub
	}
	hctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.housekeepLoop(hctx, s.done)
	return nil
}

// Stop ends the subscription and the housekeeper and waits for recordings in flight.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	sub, cancel, done := s.sub, s.cancel, s.done
	hcancel, hdone := s.hcancel, s.hdone
	s.sub, s.cancel, s.done, s.hind, s.hcancel, s.hdone = nil, nil, nil, nil, nil, nil
	s.mu.Unlock()
	if hcancel != nil {
		hcancel()
		<-hdone
	}
	if sub != nil {
		_ = sub.Close()
	}
	if cancel != nil {
		cancel()
		<-done
	}
	s.Wait()
}

func (s *Service) onEvent(ctx context.Context, ev events.Event) error {
	switch ev.Kind {
	case events.KindPerceptionOrderFilled:
		s.onFill(ctx, ev)
	case events.KindApprovalDecided, events.KindApprovalExpired:
		s.onApproval(ctx, ev)
	}
	return nil
}

// onApproval links an approval decision to the intervention whose message carried the
// card, when it had one. Only the owner can decide a card (approval.Service.Decide).
func (s *Service) onApproval(ctx context.Context, ev events.Event) {
	p, _ := ev.Payload.(map[string]any)
	iid, _ := p["intervention_id"].(string)
	id, _ := p["id"].(string)
	status, _ := p["status"].(string)
	if iid == "" {
		return
	}
	kind := Kind(status)
	if ev.Kind == events.KindApprovalExpired {
		kind = KindExpired
	}
	if kind != KindApproved && kind != KindRejected && kind != KindExpired {
		return
	}
	s.recordLogged(ctx, Response{InterventionID: iid, Kind: kind, Source: SourceApproval, Event: id, At: ev.Time})
}

// onFill records acted_on for the interventions about the asset of a real fill that
// happened within ActedOnWindow of their delivery. A paper order is not a fill, and
// neither is one placed by a schedule rather than by the owner's own turn or an
// approved proposal (a routine DCA buy near an alert on the same asset is not a reaction
// to it).
func (s *Service) onFill(ctx context.Context, ev events.Event) {
	var o orderfeed.OrderEvent
	switch p := ev.Payload.(type) {
	case orderfeed.OrderEvent:
		o = p
	case *orderfeed.OrderEvent:
		if p == nil {
			return
		}
		o = *p
	default:
		return
	}
	if o.Paper || o.Symbol == "" || scheduledOrder(o) {
		return
	}
	at := o.At
	if at.IsZero() {
		at = ev.Time
	}
	key := o.OrderID
	if key == "" {
		key = ev.ID
	}
	s.logFill(ctx, key, o.Symbol, at)
	s.ObserveChange(ctx, SourceOrderFill, o.Symbol, at, key)
}

// logFill keeps the fill's time and symbol, and nothing else, in feedback_fills for the
// hindsight job and the harm metric. A repeat of the same order is one row.
func (s *Service) logFill(ctx context.Context, key, symbol string, at time.Time) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id := responseID(Response{InterventionID: key, Kind: "fill", Source: SourceOrderFill})
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO feedback_fills (id, at_ms, symbol) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		id, at.UnixMilli(), clip(symbol, maxSourceRunes)); err != nil {
		logger.DebugCF("feedback", "Could not log a fill", map[string]any{"error": err.Error()})
	}
}

func scheduledOrder(o orderfeed.OrderEvent) bool {
	switch o.TurnKind {
	case proactive.TurnCronLegacy, proactive.TurnHeartbeatLegacy, proactive.TurnReflection:
		return true
	}
	return strings.HasPrefix(o.Origin, "journal:") && o.Origin != "journal:"+approvalWorkflow
}

// ObserveChange records acted_on, with source as its source, for every intervention
// delivered in the ActedOnWindow before at whose stored subject is subject: the same
// asset (an order, a position, a plan on BTC/USDT matches an alert about BTC) or the very
// same subject string ("plan:12"). The match is by code on what attention stored; no
// model reads anything. event keys the row, so a replayed change records once.
func (s *Service) ObserveChange(ctx context.Context, source, subject string, at time.Time, event string) {
	if s == nil {
		return
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return
	}
	if at.IsZero() {
		at = s.clock()
	}
	ids, err := s.deliveredAbout(ctx, subject, at)
	if err != nil {
		logger.DebugCF("feedback", "Could not look up the interventions a change is about", map[string]any{"error": err.Error()})
		return
	}
	for _, id := range ids {
		s.recordLogged(ctx, Response{InterventionID: id, Kind: KindActedOn, Value: source, Source: source, Event: event, At: at})
	}
}

// deliveredAbout returns the interventions delivered in [at-ActedOnWindow, at] about
// subject. attention_decisions.base_asset is the upper-case base asset code computed
// when the decision was made.
func (s *Service) deliveredAbout(ctx context.Context, subject string, at time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT COALESCE(intervention_id, id) FROM attention_decisions
		  WHERE delivered = 1 AND delivered_at_ms BETWEEN ? AND ?
		    AND ((base_asset IS NOT NULL AND base_asset = ?) OR json_extract(candidate_json, '$.Subject') = ?)`,
		at.Add(-ActedOnWindow).UnixMilli(), at.UnixMilli(), baseOf(subject), subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// baseOf is the upper-case base asset of a market symbol: "BTC/USDT", "BTC/USDT:USDT",
// "BTC-USDT-SWAP" and "BTC" all give "BTC". A subject that is not one ("plan:12",
// "portfolio") gives a string no base_asset can equal, since base_asset is letters and
// digits only.
func baseOf(subject string) string {
	s := strings.ToUpper(strings.TrimSpace(subject))
	if i := strings.IndexAny(s, "/-:_ "); i >= 0 {
		if s[:i] == "PLAN" || s[:i] == "SOURCE" {
			return ""
		}
		s = s[:i]
	}
	return s
}

func (s *Service) recordLogged(ctx context.Context, r Response) {
	if _, err := s.Record(ctx, r); err != nil {
		logger.WarnCF("feedback", "Could not record a response", map[string]any{"kind": string(r.Kind), "error": err.Error()})
	}
}

// pollChanges asks the change source for what changed since the last look and records
// each change with ObserveChange.
func (s *Service) pollChanges(ctx context.Context) {
	if s.changes == nil {
		return
	}
	s.mu.Lock()
	from := s.polled
	now := s.clock()
	s.polled = now
	s.mu.Unlock()
	cs, err := s.changes.ChangesSince(ctx, from)
	if err != nil {
		logger.DebugCF("feedback", "Could not read the changes", map[string]any{"error": err.Error()})
		return
	}
	for _, c := range cs {
		s.ObserveChange(ctx, c.Source, c.Subject, c.At, c.Event)
	}
}

// Housekeep deletes the responses older than Retention and marks none, with the value
// "unknown", the interventions delivered more than UnansweredAfter ago that have no
// other row. It looks at interventions delivered after the table was created and in the
// last scanWindow only. It returns how many none rows it wrote.
func (s *Service) Housekeep(ctx context.Context) (int, error) {
	if s == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 4*dbTimeout)
	defer cancel()
	s.pollChanges(ctx)
	now := s.clock()
	if n, err := prune(ctx, s.db, now.Add(-Retention)); err != nil {
		return 0, fmt.Errorf("feedback: prune: %w", err)
	} else if n > 0 {
		logger.InfoCF("feedback", "Pruned old responses", map[string]any{"rows": n})
		_ = dropOrphanMarkers(ctx, s.db)
	}
	from := now.Add(-scanWindow)
	if s.since.After(from) {
		from = s.since
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(d.intervention_id, d.id), MIN(d.delivered_at_ms) FROM attention_decisions d
		  WHERE d.delivered = 1 AND d.delivered_at_ms >= ? AND d.delivered_at_ms <= ?
		    AND EXISTS (SELECT 1 FROM notify_sent n WHERE n.intervention_id = COALESCE(d.intervention_id, d.id))
		    AND NOT EXISTS (SELECT 1 FROM feedback_responses f WHERE f.intervention_id = COALESCE(d.intervention_id, d.id))
		  GROUP BY 1 LIMIT 500`,
		from.UnixMilli(), now.Add(-UnansweredAfter).UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("feedback: find unanswered: %w", err)
	}
	type unanswered struct {
		id string
		at int64
	}
	var todo []unanswered
	for rows.Next() {
		var u unanswered
		if err := rows.Scan(&u.id, &u.at); err != nil {
			_ = rows.Close()
			return 0, err
		}
		todo = append(todo, u)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	n := 0
	for _, u := range todo {
		added, err := s.Record(ctx, Response{
			InterventionID: u.id, Kind: KindNone, Value: "unknown", Source: SourceHousekeep,
			At: time.UnixMilli(u.at).Add(UnansweredAfter),
		})
		if err != nil {
			return n, err
		}
		if added {
			n++
		}
	}
	return n, nil
}

func (s *Service) housekeepLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(housekeepEvery)
	defer t.Stop()
	for {
		if _, err := s.Housekeep(ctx); err != nil && ctx.Err() == nil {
			logger.DebugCF("feedback", "Housekeeping failed", map[string]any{"error": err.Error()})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
