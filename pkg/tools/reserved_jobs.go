package tools

import (
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

// Scheduled-job name prefixes that select a code-driven handler. The gateway's
// dispatcher routes a job by its name, and the handler trusts what the name says
// (handleDCAAutoJob parses a plan ID from "dca:<id>:..." and hands the turn that
// plan's scope). The name is therefore a capability, and only the tools that own
// these jobs - the alert, DCA and delta-neutral tools, which call
// cronService.AddJob directly - may mint it. The cron tool, which lets a model
// schedule an arbitrary prompt, refuses to create or rename a job to one of
// them.
const (
	JobPrefixPriceAlert     = "price_alert:"
	JobPrefixIndicatorAlert = "indicator_alert:"
	JobPrefixDCA            = "dca:"
	JobPrefixDeltaNeutral   = "dn:"
)

// ReservedJobNamePrefixes is every prefix above. The dispatcher in
// cmd/khunquant/internal/gateway reads the same constants.
var ReservedJobNamePrefixes = []string{
	JobPrefixPriceAlert, JobPrefixIndicatorAlert, JobPrefixDCA, JobPrefixDeltaNeutral,
}

// IsReservedJobName reports whether a job name starts with a reserved prefix.
// It ignores case and leading space: refusing a near miss costs nothing, and
// the dispatcher's own match is exact, so anything it would route is covered.
func IsReservedJobName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, p := range ReservedJobNamePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// removeDCAPlanJobs removes every scheduled job named for the plan - "dca:<id>:"
// with the trailing colon, so plan 1 never matches plan 12 - except the one
// whose ID is keepJobID (empty keeps none). Only the DCA tools mint such names.
func removeDCAPlanJobs(svc *cron.CronService, planID int64, keepJobID string) {
	prefix := fmt.Sprintf("%s%d:", JobPrefixDCA, planID)
	for _, j := range svc.ListJobs(true) {
		if strings.HasPrefix(j.Name, prefix) && j.ID != keepJobID {
			svc.RemoveJob(j.ID)
		}
	}
}
