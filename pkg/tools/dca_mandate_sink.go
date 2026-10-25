package tools

import (
	"context"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// ---- the executor of an approved mandate proposal ----

type mandateSink struct {
	cfg   *config.Config
	store *dca.Store
	now   func() time.Time
}

// NewMandateSink returns the approval executor for Kind mandate: it writes the
// mandate row and places nothing.
func NewMandateSink(cfg *config.Config, store *dca.Store, now func() time.Time) approval.MandateSink {
	if now == nil {
		now = time.Now
	}
	return &mandateSink{cfg: cfg, store: store, now: now}
}

func (s *mandateSink) CreateMandate(ctx context.Context, p approval.Proposal) error {
	ownerID, ok := s.cfg.OwnerDecider(p.DecidedBy)
	if !ok {
		return errors.New("no owner is configured")
	}
	_, err := s.store.CreateMandateFromProposal(ctx, p, ownerID, s.now())
	return err
}
