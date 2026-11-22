package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// PaperBookRelPath is where a workspace keeps its paper order book, slash-separated.
// The directory is on autonomousProtectedPaths (pkg/tools/autonomous_paths.go): a
// background turn must not be able to plant or edit a simulated order.
const PaperBookRelPath = "memory/paper/paper_orders.db"

// ErrNoPaperOrder is returned for a paper- order ID the book does not hold. A
// simulated order that was never placed (or was placed in another workspace) is an
// error, never a made-up "open" order.
var ErrNoPaperOrder = errors.New("no such paper order")

// ErrNotPaperOrderID is returned when the paper book is asked about an ID that is
// not one of its own. A real exchange order ID is never answered from the book.
var ErrNotPaperOrderID = errors.New("not a paper order ID (the paper order book never answers for a real exchange order)")

// ErrPaperIDWhileLive is returned for a paper- order ID while paper_trading_mode is
// off: such an ID never reaches a real exchange.
var ErrPaperIDWhileLive = errors.New("this is a simulated (paper) order ID and paper_trading_mode is off, so it cannot be looked up or cancelled on the real exchange; turn paper mode back on to manage it")

// IsPaperOrderID reports whether id is one the simulation issued.
func IsPaperOrderID(id string) bool { return strings.HasPrefix(id, PaperOrderIDPrefix) }

// RefusePaperIDWhileLive returns ErrPaperIDWhileLive when id is a paper order ID and
// paper mode is off. Every tool that sends an order ID to a real exchange (fetch,
// cancel) calls it first, so a simulated ID never goes out on the wire.
func RefusePaperIDWhileLive(cfg *config.Config, id string) error {
	if IsPaperOrderID(id) && !IsPaperMode(cfg) {
		return ErrPaperIDWhileLive
	}
	return nil
}

const (
	paperStatusOpen     = "open"
	paperStatusClosed   = "closed"
	paperStatusCanceled = "canceled"
)

// paperRow is one simulated order as the book stores it.
type paperRow struct {
	ID        string
	Symbol    string
	Type      string
	Side      string
	Amount    float64
	Price     float64 // the limit price (or the fill price of a market order)
	Status    string
	Filled    float64
	Average   float64
	Cost      float64
	CreatedMs int64
	ClosedMs  int64 // when it filled or was cancelled; 0 while open
}

// PaperBook is the simulated order book: every order SimulationProvider places,
// per account (AccountKey), so a paper order can be listed, fetched, cancelled and
// found in history like a real one, and survives a restart. It is a plain store: it
// never prices anything; SimulationProvider decides when an order fills.
type PaperBook struct {
	db *sql.DB
	mu sync.Mutex // one writer at a time; the in-memory form has a single connection
}

var paperBookMigrations = []sqlitex.Migration{{
	Component: "paperbook",
	Version:   1,
	SQL: `CREATE TABLE paper_orders (
	account_key TEXT    NOT NULL,
	id          TEXT    NOT NULL,
	symbol      TEXT    NOT NULL,
	type        TEXT    NOT NULL,
	side        TEXT    NOT NULL,
	amount      REAL    NOT NULL,
	price       REAL    NOT NULL,
	status      TEXT    NOT NULL,
	filled      REAL    NOT NULL,
	average     REAL    NOT NULL,
	cost        REAL    NOT NULL,
	created_ms  INTEGER NOT NULL,
	closed_ms   INTEGER NOT NULL,
	PRIMARY KEY (account_key, id)
);
CREATE INDEX idx_paper_orders_status ON paper_orders(account_key, status, created_ms);`,
}}

// NewMemoryPaperBook returns a book that lives only as long as the process. It is
// what NewSimulationProvider uses, so a caller that does not give a workspace still
// gets a coherent (just not persistent) order book.
func NewMemoryPaperBook() (*PaperBook, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("paper book: %w", err)
	}
	// Every connection of ":memory:" is its own database: pin the pool to one.
	db.SetMaxOpenConns(1)
	return finishPaperBook(db)
}

// OpenPaperBook opens (creating) the book file at path.
func OpenPaperBook(path string) (*PaperBook, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("paper book: create dir: %w", err)
	}
	// Create the file 0600 ourselves, as sqlitex.Shared does.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("paper book: create %s: %w", path, err)
	}
	_ = f.Close()
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	db, err := sqlitex.Open(path)
	if err != nil {
		return nil, fmt.Errorf("paper book: %w", err)
	}
	return finishPaperBook(db)
}

func finishPaperBook(db *sql.DB) (*PaperBook, error) {
	if err := sqlitex.Migrate(context.Background(), db, paperBookMigrations...); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("paper book: migrate: %w", err)
	}
	return &PaperBook{db: db}, nil
}

// Close releases the database.
func (b *PaperBook) Close() error { return b.db.Close() }

var (
	paperBooksMu sync.Mutex
	paperBooks   = map[string]*PaperBook{}
)

// PaperBookFor returns the process-wide book of cfg's workspace, opened on first
// use, so every SimulationProvider of one workspace shares one handle. A config
// with no workspace is an error: a book that is not on disk would forget.
func PaperBookFor(cfg *config.Config) (*PaperBook, error) {
	ws := ""
	if cfg != nil {
		ws = strings.TrimSpace(cfg.WorkspacePath())
	}
	if ws == "" {
		return nil, errors.New("paper book: the config has no workspace to keep it in")
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return nil, fmt.Errorf("paper book: resolve workspace: %w", err)
	}
	path := filepath.Join(filepath.Clean(abs), filepath.FromSlash(PaperBookRelPath))

	paperBooksMu.Lock()
	defer paperBooksMu.Unlock()
	if b, ok := paperBooks[path]; ok {
		return b, nil
	}
	b, err := OpenPaperBook(path)
	if err != nil {
		return nil, err
	}
	paperBooks[path] = b
	return b, nil
}

// ClosePaperBooks closes every book PaperBookFor opened (tests, shutdown).
func ClosePaperBooks() {
	paperBooksMu.Lock()
	defer paperBooksMu.Unlock()
	for k, b := range paperBooks {
		_ = b.Close()
		delete(paperBooks, k)
	}
}

const paperCols = `id, symbol, type, side, amount, price, status, filled, average, cost, created_ms, closed_ms`

func scanPaperRow(sc interface{ Scan(dest ...any) error }) (paperRow, error) {
	var r paperRow
	err := sc.Scan(&r.ID, &r.Symbol, &r.Type, &r.Side, &r.Amount, &r.Price, &r.Status,
		&r.Filled, &r.Average, &r.Cost, &r.CreatedMs, &r.ClosedMs)
	return r, err
}

func (b *PaperBook) insert(ctx context.Context, acct string, r paperRow) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.db.ExecContext(ctx, `INSERT INTO paper_orders (account_key, `+paperCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		acct, r.ID, r.Symbol, r.Type, r.Side, r.Amount, r.Price, r.Status,
		r.Filled, r.Average, r.Cost, r.CreatedMs, r.ClosedMs)
	if err != nil {
		return fmt.Errorf("paper book: record order %s: %w", r.ID, err)
	}
	return nil
}

// get returns the order, or ErrNoPaperOrder.
func (b *PaperBook) get(ctx context.Context, acct, id string) (paperRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := scanPaperRow(b.db.QueryRowContext(ctx,
		`SELECT `+paperCols+` FROM paper_orders WHERE account_key = ? AND id = ?`, acct, id))
	if errors.Is(err, sql.ErrNoRows) {
		return paperRow{}, fmt.Errorf("%w: %s", ErrNoPaperOrder, id)
	}
	if err != nil {
		return paperRow{}, fmt.Errorf("paper book: read order %s: %w", id, err)
	}
	return r, nil
}

func (b *PaperBook) query(ctx context.Context, q string, args ...any) ([]paperRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("paper book: list orders: %w", err)
	}
	defer rows.Close()
	var out []paperRow
	for rows.Next() {
		r, err := scanPaperRow(rows)
		if err != nil {
			return nil, fmt.Errorf("paper book: list orders: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// open returns the account's open orders (one symbol, or all when symbol is empty),
// oldest first.
func (b *PaperBook) open(ctx context.Context, acct, symbol string) ([]paperRow, error) {
	q := `SELECT ` + paperCols + ` FROM paper_orders WHERE account_key = ? AND status = 'open'`
	args := []any{acct}
	if symbol != "" {
		q += ` AND symbol = ?`
		args = append(args, symbol)
	}
	return b.query(ctx, q+` ORDER BY created_ms, id`, args...)
}

// closed returns the account's filled and cancelled orders created at or after
// sinceMs, newest last, at most limit of the most recent (limit <= 0 means all).
func (b *PaperBook) closed(ctx context.Context, acct, symbol string, sinceMs int64, limit int) ([]paperRow, error) {
	q := `SELECT ` + paperCols + ` FROM paper_orders WHERE account_key = ? AND status <> 'open' AND created_ms >= ?`
	args := []any{acct, sinceMs}
	if symbol != "" {
		q += ` AND symbol = ?`
		args = append(args, symbol)
	}
	rows, err := b.query(ctx, q+` ORDER BY created_ms DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 { // oldest first, like an exchange
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// fill closes an order that is still open at price. It reports false, changing
// nothing, when the order is no longer open (a concurrent read filled or cancelled it).
func (b *PaperBook) fill(ctx context.Context, acct, id string, price float64, nowMs int64) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	res, err := b.db.ExecContext(ctx, `UPDATE paper_orders
		SET status = 'closed', filled = amount, average = ?, cost = amount * ?, closed_ms = ?
		WHERE account_key = ? AND id = ? AND status = 'open'`, price, price, nowMs, acct, id)
	if err != nil {
		return false, fmt.Errorf("paper book: fill order %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// cancel cancels an order that is still open. It reports false, changing nothing,
// when the order is not open.
func (b *PaperBook) cancel(ctx context.Context, acct, id string, nowMs int64) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	res, err := b.db.ExecContext(ctx, `UPDATE paper_orders
		SET status = 'canceled', closed_ms = ?
		WHERE account_key = ? AND id = ? AND status = 'open'`, nowMs, acct, id)
	if err != nil {
		return false, fmt.Errorf("paper book: cancel order %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
