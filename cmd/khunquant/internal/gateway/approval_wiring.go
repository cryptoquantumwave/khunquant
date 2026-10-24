package gateway

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func init() {
	// The approval.order workflow joins the journal registry. The source yields
	// nothing unless approvals were built for this generation, so a gateway with
	// approvals off gains no engine, no table and no goroutine.
	journalSources = append(journalSources, func(d assemblyDeps) []journal.Definition {
		if d.Approval == nil {
			return nil
		}
		return d.Approval.svc.Definitions()
	})
}

// approvalRuntime is the running order-approval side of one generation of
// services: the service and its sweeper.
//
// The goroutines that exist while it runs (and nothing else is started):
//
//	Service.Run   the sweeper (expiry, healing, pruning); owner: this runtime;
//	              stops when cancel fires.
//
// Executing an approved order is the journal engine's job (approval.order), not
// a goroutine of this runtime.
type approvalRuntime struct {
	svc *approval.Service
	// mandateStore is the DCA plan store handle mandates live in (nil while mandates
	// are off). It is closed by closeStores, after the workflow engine has stopped.
	mandateStore *dca.Store
	// memory is whether memory suggestions can reach the owner (a live digest exists).
	memory bool
	// reflect is whether the nightly reflection's changes can be applied once approved.
	reflect bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// closeStores releases the plan store handle. Safe on nil.
func (rt *approvalRuntime) closeStores() {
	if rt == nil || rt.mandateStore == nil {
		return
	}
	if err := rt.mandateStore.Close(); err != nil {
		logger.WarnCF("gateway", "Closing the mandate plan store failed", map[string]any{"error": err.Error()})
	}
	rt.mandateStore = nil
}

// buildApproval constructs the approval service, or nil when approvals are not
// effectively on (proactive.enabled, a valid owner and proactive.approvals.enabled),
// when there is nobody to deliver a card to, or when the shared database cannot be
// opened. It touches no table and starts nothing; attach does that once the
// workflow engine exists. A failure is logged and never stops the gateway.
func buildApproval(d assemblyDeps, ns *notify.Service, mgr *channels.Manager, ob *attention.Outbox, reg *triggers.Registry) *approvalRuntime {
	if !d.Cfg.Proactive.Effective().Approvals {
		return nil
	}
	if ns == nil {
		logger.WarnCF("gateway", "Order approvals disabled: owner delivery is not available", nil)
		return nil
	}
	// No approvals without the kill flag: an order that could be approved while a
	// /stop cannot be honoured is the failure the flag exists to prevent.
	if d.Control.killer() == nil {
		logger.ErrorCF("gateway", "Order approvals disabled: the kill switch is unavailable", nil)
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.ErrorCF("gateway", "Order approvals disabled: the shared proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return nil
	}
	opts := approval.Options{
		DB: db, Cfg: d.Cfg, Backend: tools.NewApprovalOrderBackend(d.Cfg), Notifier: ns,
		Events: d.Shared.eventBus(),
		Locale: profileLocale(db),
	}
	// Memory suggestions: an approved one is appended to MEMORY.md, and the suggestion
	// reaches the owner in the daily digest, so they need the live digest.
	opts.Memories = tools.NewMemorySink(d.Cfg.WorkspacePath())
	opts.Digest = memoryDigestFor(d.Cfg, ob)
	opts.Reflections = reflectionSinkFor(d, db, ob, reg, opts.Digest != nil)
	if mgr != nil {
		opts.Editor = channelCardEditor{mgr: mgr}
	}
	// The kill flag: set only when there is one (a nil *State in the interface
	// would not read as "never killed").
	if k := d.Control.killer(); k != nil {
		opts.Killer = k
	}
	// Mandates: an approved mandate card writes its row in the DCA store, so the
	// approval service gets the executor only while mandates are on.
	var mandateStore *dca.Store
	if d.Cfg.Proactive.Effective().Mandates {
		if st, err := dca.NewStore(d.Cfg.WorkspacePath()); err != nil {
			logger.ErrorCF("gateway", "Mandates disabled: the DCA plan store could not be opened", map[string]any{"error": err.Error()})
		} else {
			mandateStore = st
			opts.Mandates = tools.NewMandateSink(d.Cfg, st, nil)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc, err := approval.New(ctx, opts)
	if err != nil {
		logger.ErrorCF("gateway", "Order approvals disabled", map[string]any{"error": err.Error()})
		if mandateStore != nil {
			_ = mandateStore.Close()
		}
		return nil
	}
	return &approvalRuntime{svc: svc, mandateStore: mandateStore, memory: opts.Digest != nil, reflect: opts.Reflections != nil}
}

// attach connects the service to the workflow engine, starts the sweeper (whose
// first pass heals an approval whose workflow start a crash lost) and opens
// propose_order. Without an engine nothing executes, so propose_order stays shut.
func (rt *approvalRuntime) attach(j *journalRuntime) {
	if rt == nil {
		return
	}
	if j == nil {
		logger.ErrorCF("gateway", "Order approvals disabled: the workflow engine is not running", nil)
		return
	}
	rt.svc.SetWorkflows(j.engine)
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel, rt.done = cancel, make(chan struct{})
	go func() {
		defer close(rt.done)
		rt.svc.Run(ctx)
	}()
	tools.SetOrderProposer(rt.svc)
	setDigestButtons(rt.svc)
	if rt.memory { // without a live digest a suggestion could never reach the owner: the tool stays closed
		tools.SetMemoryProposer(rt.svc)
	}
	if rt.mandateStore != nil {
		tools.SetMandateProposer(rt.svc)
	}
}

// stop closes propose_order and ends the sweeper. Safe on nil.
func (rt *approvalRuntime) stop() {
	if rt == nil {
		return
	}
	tools.SetOrderProposer(nil)
	tools.SetMemoryProposer(nil)
	setDigestButtons(nil)
	tools.SetMandateProposer(nil)
	if rt.cancel == nil {
		return
	}
	rt.cancel()
	select {
	case <-rt.done:
	case <-time.After(10 * time.Second):
		logger.WarnCF("gateway", "The approval sweeper did not stop in time", nil)
	}
}

// channelCardEditor adapts the channel manager to approval.CardEditor.
type channelCardEditor struct{ mgr *channels.Manager }

func (e channelCardEditor) EditCard(ctx context.Context, channel, chatID, messageID, text string) error {
	ch, ok := e.mgr.GetChannel(channel)
	if !ok {
		return errors.New("channel not found")
	}
	ed, ok := ch.(channels.MessageEditor)
	if !ok {
		return errors.New("channel cannot edit messages")
	}
	return ed.EditMessage(ctx, chatID, messageID, text)
}

// profileLocale words and times cards in the owner's language and time zone, from
// the profile; English in Asia/Bangkok when none is set.
func profileLocale(db *sql.DB) func(ctx context.Context) approval.Locale {
	var (
		mu    sync.Mutex
		store *intent.Store
	)
	return func(ctx context.Context) approval.Locale {
		loc := approval.Locale{Lang: "en"}
		tz := intent.DefaultTimezone
		mu.Lock()
		if store == nil {
			store, _ = intent.NewStore(ctx, db)
		}
		st := store
		mu.Unlock()
		if st != nil {
			if p, err := st.Profile(ctx); err == nil {
				if p.Language == intent.LanguageThai {
					loc.Lang = "th"
				}
				if p.Timezone != "" {
					tz = p.Timezone
				}
			}
		}
		if l, err := time.LoadLocation(tz); err == nil {
			loc.Loc = l
		}
		return loc
	}
}
