package tools

// DNSyncFunc asks the gateway to bring the delta-neutral monitor's trigger specs in line
// with the plans' cron jobs, soon. The plan tools call it after they create, enable,
// re-schedule or delete a plan's "dn:" job: the migration switches that job off once a
// trigger watches the plan, and an edit that makes a new or re-enabled job would
// otherwise leave the legacy path delivering until the next hourly pass.
//
// It is late-bound and may be nil (no gateway, no registry, a test): then a tool behaves
// exactly as it always did. The function must return at once and never block: the
// gateway only signals its maintenance goroutine, which debounces and does the work.
type DNSyncFunc func()

// request calls the hook if there is one.
func (f DNSyncFunc) request() {
	if f != nil {
		f()
	}
}
