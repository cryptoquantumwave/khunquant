package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// AlertTriggers is what the alert tools need from the trigger registry. The gateway
// implements it over the registry of the running generation; the tools hold only a
// function that returns it (nil, or a nil result, when there is none), because they
// are registered before the registry exists.
type AlertTriggers interface {
	// Live reports whether a live registry is running right now: triggers are
	// effectively live (which needs the owner) and the registry was built. It is
	// asked on every call; the tools never remember the answer.
	Live() bool
	// Add stores a new spec.
	Add(ctx context.Context, s triggers.Spec) (triggers.Spec, error)
	// List returns the stored specs that match f, in any mode.
	List(ctx context.Context, f triggers.ListFilter) ([]triggers.Spec, error)
	// Remove deletes a spec.
	Remove(ctx context.Context, id string) error
}

// AlertTriggersFunc resolves the AlertTriggers of the running generation, late.
type AlertTriggersFunc func() AlertTriggers

// Origins of the specs the alert tools write.
const (
	alertOriginPrefix = "tool:"
	// migratedOriginPrefix starts the Origin of a spec made from a cron job
	// (migrate.OriginPrefix; this package cannot import it).
	migratedOriginPrefix = "migration:cron:"
)

// liveTriggers returns the registry when alerts go to it: a live registry is
// running. Anything else is nil, and the caller takes the cron path, unchanged.
func liveTriggers(f AlertTriggersFunc) AlertTriggers {
	if f == nil {
		return nil
	}
	if at := f(); at != nil && at.Live() {
		return at
	}
	return nil
}

// anyTriggers returns the registry whatever its mode, for listing and cancelling.
func anyTriggers(f AlertTriggersFunc) AlertTriggers {
	if f == nil {
		return nil
	}
	return f()
}

// liveDeliveryNote is said in the result of a create that went to the registry.
// It is a change in behaviour and is not to be missed.
const liveDeliveryNote = "  Delivery: to you, the owner, privately. It will NOT be sent to the chat where it was created " +
	"(the channel and chat_id arguments are ignored).\n" +
	"  Quiet hours: an alert you set is sent at once, even in your quiet hours."

func fmtAlertNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func isAlertKind(kind string) bool {
	return kind == triggers.KindPriceCross || kind == triggers.KindIndicatorCross
}

// registryAlerts returns the alert specs a user should see: those the tools made, in
// any mode, and those migrated from a cron job once they are live (before that the
// cron job is the alert and is listed as itself).
func registryAlerts(ctx context.Context, at AlertTriggers, standIns map[string]bool) ([]triggers.Spec, error) {
	all, err := at.List(ctx, triggers.ListFilter{})
	if err != nil {
		return nil, err
	}
	var out []triggers.Spec
	for _, s := range all {
		if !isAlertKind(s.Kind) || standIns[s.ID] {
			continue // a spec with a stand-in cron job is listed as that job
		}
		switch {
		case strings.HasPrefix(s.Origin, alertOriginPrefix):
		case strings.HasPrefix(s.Origin, migratedOriginPrefix) && s.Mode == triggers.ModeLive:
		default:
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// specStatus is what the Mode column says: the spec's mode while a live registry
// checks it, "inactive" when it is on but nothing is checking it, "done" for one that
// has fired.
func specStatus(s triggers.Spec, live bool) string {
	switch {
	case s.Mode == triggers.ModeOff:
		return "done"
	case !live && strings.HasPrefix(s.Origin, alertOriginPrefix):
		return "inactive"
	}
	return s.Mode
}

// describeSpec is one line saying what a spec watches.
func describeSpec(s triggers.Spec) string {
	switch s.Kind {
	case triggers.KindPriceCross:
		var p triggers.PriceCrossParams
		if json.Unmarshal(s.Params, &p) == nil {
			return fmt.Sprintf("%s %s price %s on %s", p.Symbol, p.Direction, fmtAlertNumber(p.Threshold), p.Provider)
		}
	case triggers.KindIndicatorCross:
		var p triggers.IndicatorCrossParams
		if json.Unmarshal(s.Params, &p) == nil {
			ind := strings.ToUpper(p.Indicator)
			if p.Period > 0 {
				ind = fmt.Sprintf("%s(%d)", ind, p.Period)
			}
			return fmt.Sprintf("%s %s(%s) %s %s on %s", p.Symbol, ind, p.Timeframe, p.Direction, fmtAlertNumber(p.Threshold), p.Provider)
		}
	}
	return s.Kind
}

// listAlertsAll renders the cron alerts exactly as before, and, when the registry holds
// alerts that belong in the list, a second, labelled section for them. With none it
// is the old text, byte for byte.
func listAlertsAll(ctx context.Context, cs *cron.CronService, f AlertTriggersFunc) *ToolResult {
	var cronAlerts []cron.CronJob
	for _, j := range cs.ListJobs(false) {
		if strings.HasPrefix(j.Name, "price_alert:") || strings.HasPrefix(j.Name, "indicator_alert:") {
			cronAlerts = append(cronAlerts, j)
		}
	}
	var specs []triggers.Spec
	live := false
	if at := anyTriggers(f); at != nil {
		standIns := map[string]bool{}
		for _, j := range cs.ListJobs(true) {
			if j.State.StandInFor != "" {
				standIns[j.State.StandInFor] = true
			}
		}
		var err error
		if specs, err = registryAlerts(ctx, at, standIns); err != nil {
			return ErrorResult(fmt.Sprintf("listing trigger alerts: %v", err))
		}
		live = at.Live()
	}

	if len(specs) == 0 {
		if len(cronAlerts) == 0 {
			return UserResult("No active price or indicator alerts.")
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Active alerts (%d):\n\n", len(cronAlerts)))
		sb.WriteString(fmt.Sprintf("%-24s  %-s\n", "ID", "Name"))
		sb.WriteString(fmt.Sprintf("%-24s  %-s\n", strings.Repeat("-", 24), strings.Repeat("-", 40)))
		for _, a := range cronAlerts {
			sb.WriteString(fmt.Sprintf("%-24s  %s\n", a.ID, a.Name))
		}
		return UserResult(sb.String())
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Active alerts (%d):\n", len(cronAlerts)+len(specs)))
	if len(cronAlerts) > 0 {
		sb.WriteString("\nCron alerts (sent to the chat they were created in):\n")
		sb.WriteString(fmt.Sprintf("%-24s  %-s\n", "ID", "Name"))
		sb.WriteString(fmt.Sprintf("%-24s  %-s\n", strings.Repeat("-", 24), strings.Repeat("-", 40)))
		for _, a := range cronAlerts {
			sb.WriteString(fmt.Sprintf("%-24s  %s\n", a.ID, a.Name))
		}
	}
	sb.WriteString("\nTrigger alerts (sent to the owner privately):\n")
	sb.WriteString(fmt.Sprintf("%-26s  %-8s  %-s\n", "ID", "Mode", "Alert"))
	sb.WriteString(fmt.Sprintf("%-26s  %-8s  %-s\n", strings.Repeat("-", 26), strings.Repeat("-", 8), strings.Repeat("-", 40)))
	for _, s := range specs {
		sb.WriteString(fmt.Sprintf("%-26s  %-8s  %s\n", s.ID, specStatus(s, live), describeSpec(s)))
	}
	if !live {
		for _, s := range specs {
			if specStatus(s, live) == "inactive" {
				sb.WriteString("\nInactive alerts are not being checked: triggers are not live and no cron stand-in could be made " +
					"(set proactive.owner so they can run on the cron path, or turn triggers live).\n")
				break
			}
		}
	}
	return UserResult(sb.String())
}

// cancelAlertAny cancels a cron alert exactly as before, or, when no cron job has that
// ID, a trigger alert. Cancelling one that was migrated from a cron job removes the
// switched-off job too, so a later rollback cannot bring it back.
func cancelAlertAny(ctx context.Context, cs *cron.CronService, f AlertTriggersFunc, id string) *ToolResult {
	at := anyTriggers(f)
	standInFor := ""
	if j := cs.GetJob(id); j != nil {
		standInFor = j.State.StandInFor
	}
	if cs.RemoveJob(id) {
		if standInFor != "" && at != nil {
			// The job was standing in for a spec: cancelling the alert cancels both.
			_ = at.Remove(ctx, standInFor)
		}
		return UserResult(fmt.Sprintf("Alert %s cancelled.", id))
	}
	if at == nil {
		return ErrorResult(fmt.Sprintf("Alert %s not found.", id))
	}
	specs, err := at.List(ctx, triggers.ListFilter{})
	if err != nil {
		return ErrorResult(fmt.Sprintf("cancelling alert: %v", err))
	}
	for _, s := range specs {
		if s.ID != id || !isAlertKind(s.Kind) {
			continue
		}
		if err := at.Remove(ctx, id); err != nil {
			return ErrorResult(fmt.Sprintf("cancelling alert: %v", err))
		}
		if jobID, ok := strings.CutPrefix(s.Origin, migratedOriginPrefix); ok {
			cs.RemoveJob(jobID)
		}
		return UserResult(fmt.Sprintf("Alert %s cancelled.", id))
	}
	return ErrorResult(fmt.Sprintf("Alert %s not found.", id))
}
