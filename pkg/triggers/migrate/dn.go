package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// jobPrefixDeltaNeutral is the name prefix of the delta-neutral monitor jobs
// ("dn:<plan id>:<plan name>"), copied from pkg/tools (which this package must not
// import); a test checks the copy against the original.
const jobPrefixDeltaNeutral = "dn:"

// PlanLookup says what is known of a delta-neutral plan: its futures symbol, which
// names it in a message. ok false means the plan does not exist (the job watches
// nothing and is left to run as it is). It is called from a Sync, never from the
// pipeline, and may read the plan store.
type PlanLookup func(planID int64) (futuresSymbol string, ok bool)

// DNFamily is the delta-neutral monitor jobs: one dn_health spec per "dn:" job.
//
// A promoted job whose spec is found switched off (the registry does that after ten
// errors in a row) is switched back on, not removed: a monitor is never "finished".
//
// The family has no tool-made specs (the plan tools write cron jobs, and the spec
// follows them), so there is no stand-in, no hand-over and no one-shot: the rules of
// the package for promotion and rollback are all that apply. A job's schedule is NOT
// part of its fingerprint: the plan tools and the cron screen change a plan's monitor
// interval by editing or replacing the job, and that is the plan's own business, not
// a user taking the job over; if it counted, a rollback after such an edit would
// leave the monitor switched off in both paths.
func DNFamily(lookup PlanLookup) Family {
	return Family{
		Name:     "delta-neutral",
		Prefixes: []string{jobPrefixDeltaNeutral},
		Kinds:    []string{triggers.KindDNHealth},
		Import: func(job cron.CronJob) (string, json.RawMessage, error) {
			p, err := dnParams(job, lookup)
			if err != nil {
				return "", nil, err
			}
			return triggers.KindDNHealth, p.JSON(), nil
		},
		Same:        sameDN,
		Fingerprint: fingerprintDN,
		OneShot:     func(cron.CronJob) bool { return false },
		OffRestores: true,
	}
}

// MigrateDNJobs is Sync for the delta-neutral family: it imports every "dn:" job as a
// dn_health spec (idempotently, in shadow) and, in Live mode, promotes them; in Shadow
// mode it hands back anything promoted. Call it at startup, on a reload and now and
// then, with Live only when the registry is live and running.
func MigrateDNJobs(ctx context.Context, jobs Jobs, specs Specs, mode Mode, lookup PlanLookup, opts ...Option) (Report, error) {
	return New(DNFamily(lookup), jobs, specs, opts...).Sync(ctx, mode)
}

// planOfJob reads the plan ID and the plan's label out of a job's name.
func planOfJob(job cron.CronJob) (id int64, label string, err error) {
	rest, ok := strings.CutPrefix(job.Name, jobPrefixDeltaNeutral)
	if !ok {
		return 0, "", fmt.Errorf("not a delta-neutral job")
	}
	idText, label, _ := strings.Cut(rest, ":")
	id, err = strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return 0, "", fmt.Errorf("no plan id in the job name")
	}
	return id, label, nil
}

func dnParams(job cron.CronJob, lookup PlanLookup) (triggers.DNHealthParams, error) {
	id, label, err := planOfJob(job)
	if err != nil {
		return triggers.DNHealthParams{}, err
	}
	p := triggers.DNHealthParams{PlanID: id, Name: triggers.CleanAlertMessage(label)}
	if lookup != nil {
		symbol, found := lookup(id)
		if !found {
			return p, fmt.Errorf("plan %d does not exist", id)
		}
		p.Symbol = symbol
	}
	if err := p.Validate(); err != nil {
		// A symbol or label that does not pass: the plan is still watched, by number.
		p.Symbol, p.Name = "", ""
		if err := p.Validate(); err != nil {
			return p, err
		}
	}
	return p, nil
}

// sameDN: the job still describes the spec when it names the same plan. The label and
// the symbol are only there to name the plan in a message.
func sameDN(job cron.CronJob, spec triggers.Spec) bool {
	id, _, err := planOfJob(job)
	if err != nil {
		return false
	}
	var p triggers.DNHealthParams
	return json.Unmarshal(spec.Params, &p) == nil && p.PlanID == id
}

// fingerprintDN digests what a user could have changed to take the job over: its name
// (which holds the plan), where it delivers and what it says. Not its schedule (see
// DNFamily) and not what the cron service rewrites as it runs.
func fingerprintDN(job cron.CronJob) string {
	b, _ := json.Marshal(struct{ Name, Channel, To, Message string }{
		Name: job.Name, Channel: job.Payload.Channel, To: job.Payload.To, Message: job.Payload.Message,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
