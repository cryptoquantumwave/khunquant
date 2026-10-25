package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
)

// notifyCloseTimeout bounds how long stopServices waits for queued session
// write-backs.
const notifyCloseTimeout = 5 * time.Second

// buildNotifyService constructs the owner-delivery service for this
// generation. It is nil, and delivery is therefore disabled, when proactive is
// off (nothing to open) or when the service cannot be built; an unbuildable
// service logs an ERROR and never stops the gateway from starting.
func buildNotifyService(d assemblyDeps, mgr *channels.Manager) *notify.Service {
	if !d.Cfg.Proactive.Effective().Enabled {
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.ErrorCF("notify", "Owner delivery disabled: cannot open the proactive database",
			map[string]any{"error": err.Error()})
		return nil
	}
	svc, err := notify.New(d.Cfg, mgr, d.AgentLoop, db, routing.NewRouteResolver(d.Cfg).DefaultAgentID(), nil)
	if err != nil {
		logger.ErrorCF("notify", "Owner delivery disabled: cannot build the notify service",
			map[string]any{"error": err.Error()})
		return nil
	}
	return svc
}

// stopNotifyService drains and stops svc, waiting at most notifyCloseTimeout.
func stopNotifyService(svc *notify.Service) {
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyCloseTimeout)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		logger.WarnCF("notify", "Notify service did not drain in time", map[string]any{"error": err.Error()})
	}
}

// deviceNotifier adapts *notify.Service to devices.Notifier.
type deviceNotifier struct{ svc *notify.Service }

func (n deviceNotifier) CanDeliver() bool { return n.svc.CanDeliver() }

func (n deviceNotifier) NotifyInfo(ctx context.Context, text string) error {
	res, err := n.svc.Deliver(ctx, notify.Notification{
		Severity:            proactive.SeverityInfo,
		Body:                text,
		DisableNotification: true,
	})
	if err != nil {
		return err
	}
	if !res.Delivered {
		return fmt.Errorf("not delivered: %s", res.Reason)
	}
	return nil
}
