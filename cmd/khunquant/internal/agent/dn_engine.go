package agent

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// dnEngineStopTimeout bounds how long the CLI waits for the workflow engine at exit.
// The engine's own shutdown wait (10 s) is shorter; this is the outer bound.
const dnEngineStopTimeout = 15 * time.Second

// startDNEngine runs a journal engine for dn.open, dn.unwind and dn.resize for as long
// as this `khunquant agent` process lives, and returns the function that stops it.
//
// Why: the workflows are durable and leased, so any process that knows their kinds
// may carry one on. A gateway that died between the two legs of an open leaves a
// workflow behind; whichever process runs the engine next (this one, or the gateway
// when it restarts) takes it over once its lease has run out and finishes it, with
// the same idempotency key, so the second leg is placed once. The lease keeps two
// processes from running one workflow at the same time.
//
// Which kinds it runs: dn.open and dn.resize (they place orders and read the kill flag
// below, which fails closed) and dn.unwind (an exit, which reads no flag, and which
// is better finished by whoever is alive than left half-closed until the gateway
// returns). What it does not do: the CLI has no owner channel and no approval service,
// so it never RUNS a dn.recover (it could find the orphan but not ask the owner, and
// would only use up a round); an operation that fails here starts the recovery and the
// gateway, when it runs, carries it out. It has no way to message the owner either, so
// an operation it finishes after the tool's wait gives no "it finished" message. The CLI's own tool registry does not carry
// the delta-neutral tools, so what runs here is a workflow some other process
// started and did not finish.
//
// When the CLI exits mid-workflow the engine stops at the step boundary, releases the
// workflow (it becomes pending), and the next engine continues it; a step that does
// not return within the shutdown wait leaves its workflow claimed until the lease
// runs out, and it resumes after that. Nothing is lost.
func startDNEngine(cfg *config.Config) (stop func()) {
	nop := func() {}
	if !cfg.Tools.IsToolEnabled(tools.NameOpenDeltaNeutralPosition) && !cfg.Tools.IsToolEnabled(tools.NameUnwindDeltaNeutralPosition) &&
		!cfg.Tools.IsToolEnabled(tools.NameResizeDeltaNeutralPosition) {
		return nop
	}
	// Only where delta-neutral plans have been used: a CLI session that never touched
	// them gains no database, no table and no goroutine.
	if _, err := os.Stat(filepath.Join(cfg.WorkspacePath(), "memory", "delta_neutral", "delta_neutral.db")); err != nil {
		return nop
	}
	store, err := deltaneutral.NewStore(cfg.WorkspacePath())
	if err != nil {
		logger.WarnCF("agent", "No workflow engine for delta-neutral opens: the plan store is unavailable", map[string]any{"error": err.Error()})
		return nop
	}
	db, err := sqlitex.Shared(agent.DefaultAgentWorkspace(cfg))
	if err != nil {
		_ = store.Close()
		logger.WarnCF("agent", "No workflow engine for delta-neutral opens: the shared database is unavailable", map[string]any{"error": err.Error()})
		return nop
	}
	eng, stopEngine := runDNEngine(db, store, cfg, openKillFlag(db))
	if eng == nil {
		_ = store.Close()
		return nop
	}
	return func() {
		stopEngine()
		_ = store.Close()
	}
}

// openKillFlag opens the kill flag on the shared database, the same table the gateway
// reads, and returns its reader. When it cannot be opened the flag is treated as
// ENGAGED: the engine refuses to place anything (fail closed), and says so once.
func openKillFlag(db *sql.DB) func() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := control.NewState(ctx, db)
	if err != nil {
		logger.ErrorCF("agent", "The kill switch cannot be read: delta-neutral opens resumed by this process will refuse to place orders",
			map[string]any{"error": err.Error()})
		return func() bool { return true }
	}
	return st.Killed
}

// runDNEngine builds the engines and runs the one that claims. It returns a nil
// engine when it cannot be built.
//
// Two engines on one database, because the process claims the operations but must not
// claim dn.recover: the runner knows only dn.open, dn.unwind and dn.resize (an engine
// claims only the kinds it defines), and the starter, which never runs, knows all four
// so that an operation that fails in this process can still START its recovery for the
// gateway to carry out.
func runDNEngine(db *sql.DB, store *deltaneutral.Store, cfg *config.Config, killed func() bool) (*journal.Engine, func()) {
	deps := &deltaneutral.Deps{
		Store:    store,
		Exchange: tools.NewDNExchange(cfg, nil),
		Killed:   killed,
		Tag: func(ctx context.Context, origin, sender string) context.Context {
			return tools.WithToolSender(tools.WithToolOrigin(ctx, origin), sender)
		},
	}
	all := deps.Definitions()
	var operations []journal.Definition
	for _, def := range all {
		if def.Kind != deltaneutral.KindRecover {
			operations = append(operations, def)
		}
	}
	starter, err := journal.New(db, all, nil)
	if err != nil {
		logger.WarnCF("agent", "No workflow engine for delta-neutral opens", map[string]any{"error": err.Error()})
		return nil, nil
	}
	runner, err := journal.New(db, operations, nil)
	if err != nil {
		logger.WarnCF("agent", "No workflow engine for delta-neutral opens", map[string]any{"error": err.Error()})
		return nil, nil
	}
	deps.SetStarter(starter)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runner.Run(ctx); err != nil {
			logger.WarnCF("agent", "Workflow engine stopped", map[string]any{"error": err.Error()})
		}
	}()
	return runner, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(dnEngineStopTimeout):
			logger.WarnCF("agent", "The workflow engine did not stop in time; its workflows resume elsewhere after their lease", nil)
		}
	}
}
