package journal

import (
	"context"
	"fmt"
	"time"
)

// DefaultListLimit and MaxListLimit bound List.
const (
	DefaultListLimit = 200
	MaxListLimit     = 1000
)

// ListFilter selects workflows for List.
type ListFilter struct {
	// Statuses limits the result to these statuses; empty means every status.
	Statuses []Status
	// Since limits the result to workflows created at or after it; zero means any age.
	Since time.Time
	// Limit bounds the number of rows (DefaultListLimit when zero, never more than
	// MaxListLimit). The newest come first.
	Limit int
}

// ActiveStatuses are the statuses of a workflow that has not ended.
var ActiveStatuses = []Status{StatusPending, StatusRunning, StatusWaiting}

// StepKey is the idempotency key of step name at sequence number seq of workflow
// id: what Ctx.IdempotencyKey returns for it. It lets code that only has the journal
// (the kill switch looking for an order a cut step may have placed) rebuild the key
// a step sent with its order.
func StepKey(workflowID string, seq int, name string) string {
	return fmt.Sprintf("%s:%d:%s", workflowID, seq, name)
}

// List returns workflows matching f, newest first. It reads rows; it changes none.
func (e *Engine) List(ctx context.Context, f ListFilter) ([]Workflow, error) {
	return e.st.listWorkflows(ctx, f)
}

func (s *store) listWorkflows(ctx context.Context, f ListFilter) ([]Workflow, error) {
	if s.fault != nil {
		if err := s.fault("list_workflows"); err != nil {
			return nil, err
		}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	since := int64(0)
	if !f.Since.IsZero() {
		since = ms(f.Since)
	}
	q := `SELECT ` + workflowCols + ` FROM workflows WHERE created_at_ms >= ?`
	args := []any{since}
	if len(f.Statuses) > 0 {
		q += ` AND status IN ` + inClause(len(f.Statuses))
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY created_at_ms DESC, id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("journal: list: %w", err)
	}
	defer rows.Close()
	var out []Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, fmt.Errorf("journal: list: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
