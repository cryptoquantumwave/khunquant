package approval

import (
	"context"
	"time"
)

// maxListedPending bounds one listing. The pending bounds (see store.go) keep the
// real number far below it.
const maxListedPending = 100

// QuoteFact and QuoteRationale name what a PendingView's Quote is.
const (
	QuoteFact      = "fact"      // a memory suggestion's fact
	QuoteRationale = "rationale" // the model's "why" under an order, mandate or reflection change
)

// PendingView is what the web launcher may show of a pending proposal. It is a
// projection, not the row: no id, nonce, hash, params, workflow or card ids leave
// the service. Summary and Code are written by code; Quote is the one model-written
// field, and a reader must show it as quoted text, never as markup.
type PendingView struct {
	Code    string
	Kind    Kind
	Status  Status
	Summary string
	Origin  Origin
	// Reducing: the order could only shrink exposure.
	Reducing  bool
	CreatedAt time.Time
	ExpiresAt time.Time
	// Quote and QuoteKind carry the model's prose, or are empty.
	Quote, QuoteKind string
}

// ListPending returns the proposals that are pending and not yet expired, soonest
// expiry first, with the service's own clock reading (the browser never decides
// expiry: Decide re-checks it, and that is the only check that counts). It reads;
// it changes nothing.
func (s *Service) ListPending(ctx context.Context) ([]PendingView, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	now := s.now()
	ps, err := s.st.list(ctx, `status = 'pending' AND expires_at_ms > ? ORDER BY expires_at_ms ASC LIMIT ?`, ms(now), maxListedPending)
	if err != nil {
		return nil, now, err
	}
	out := make([]PendingView, 0, len(ps))
	for _, p := range ps {
		v := PendingView{
			Code: p.Code, Kind: p.Kind, Status: p.Status, Summary: p.Summary, Origin: p.Origin,
			Reducing: p.Reducing, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
		}
		if p.Kind == KindMemory {
			// The fact comes from the verified params, so a row that no longer matches its
			// hash shows no model text at all.
			if m, err := VerifiedMemory(p); err == nil {
				v.Quote, v.QuoteKind = m.Fact, QuoteFact
			}
		} else if p.Rationale != "" {
			v.Quote, v.QuoteKind = p.Rationale, QuoteRationale
		}
		out = append(out, v)
	}
	return out, now, nil
}
