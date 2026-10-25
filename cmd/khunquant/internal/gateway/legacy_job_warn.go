package gateway

import (
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// maxLegacyWarned bounds the jobs remembered by the legacy-job warning.
const maxLegacyWarned = 512

// legacyJobFamilies are the cron job families the triggers replace: while triggers
// are live, a job of one of them is switched off by the migration, so one that runs
// is either a rollback that has not finished, a job made by hand, or a bug. The
// handlers still run (a rollback must find them working); this only says so.
// dca: is not here: it stays on cron until its own migration.
var legacyJobFamilies = []string{
	tools.JobPrefixPriceAlert, tools.JobPrefixIndicatorAlert, tools.JobPrefixDeltaNeutral,
}

// legacyFamily returns the family prefix (without the colon) of a legacy job name,
// or "".
func legacyFamily(name string) string {
	for _, p := range legacyJobFamilies {
		if strings.HasPrefix(name, p) {
			return strings.TrimSuffix(p, ":")
		}
	}
	return ""
}

// legacyJobWarner warns that a legacy job ran while triggers are live, at most once
// per job per day (the owner's local day is not needed: the process's).
type legacyJobWarner struct {
	mu     sync.Mutex
	warned map[string]string // job ID -> the day it was last warned about
	now    func() time.Time
}

// legacyJobs is the process's warner: a config reload rebuilds the cron service but
// the day's warnings stand.
var legacyJobs = &legacyJobWarner{now: time.Now}

// check warns when job is of a legacy family and live reports that triggers are
// live right now. It reports whether it logged. A nil live means "not live".
func (w *legacyJobWarner) check(job *cron.CronJob, live func() bool) bool {
	if job == nil || live == nil {
		return false
	}
	family := legacyFamily(job.Name)
	if family == "" || !live() {
		return false
	}
	day := w.now().Format(time.DateOnly)
	w.mu.Lock()
	if w.warned == nil {
		w.warned = map[string]string{}
	}
	if w.warned[job.ID] == day {
		w.mu.Unlock()
		return false
	}
	if len(w.warned) >= maxLegacyWarned {
		for id, d := range w.warned {
			if d != day {
				delete(w.warned, id)
			}
		}
		if len(w.warned) >= maxLegacyWarned {
			w.warned = map[string]string{} // a day with more jobs than that: start over
		}
	}
	w.warned[job.ID] = day
	w.mu.Unlock()

	logger.WarnCF("cron", "legacy job ran while triggers live", map[string]any{
		"job_id": job.ID, "family": family,
		"hint": "the trigger registry owns this alert now; this job still ran so that a rollback finds it working",
	})
	return true
}
