package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
)

// The live budget is money spent at a real provider, so it outlives the process: a kill, a
// graceful restart or a crash of a record or live instance must not give the model its
// budget back. With a BudgetStore the engine saves the budget (spend, calls, the sticky
// exhausted flag and the limits as they were lowered) before it makes a call that could
// spend, and again once the call is accounted. A store that cannot be read or written
// refuses every live call: the cost of being wrong is real money, so it fails closed.

// ErrBudgetStore is what a live call gets when the budget cannot be read back or saved.
var ErrBudgetStore = fmt.Errorf("%w: the budget cannot be read or saved", ErrBudget)

type budgetFile struct {
	LimitUSD  float64 `json:"limit_usd"`
	LimitTok  int     `json:"limit_tok"`
	SpentUSD  float64 `json:"spent_usd"`
	SpentTok  int     `json:"spent_tok"`
	Calls     int     `json:"calls"`
	Exhausted bool    `json:"exhausted"`
	MaxTok    int     `json:"max_tok"`
	MaxUSD    float64 `json:"max_usd"`
}

func fileOf(b BudgetState) budgetFile {
	return budgetFile{b.LimitUSD, b.LimitTok, b.SpentUSD, b.SpentTok, b.Calls, b.Exhausted, b.maxTok, b.maxUSD}
}

// lowest is the smaller of two limits where 0 means no limit.
func lowest[T int | float64](a, b T) T {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// loadBudget reads the saved budget into e.bud at start. A missing file is a fresh
// budget; a file that cannot be read leaves e.budErr set, which refuses live calls.
// A limit already lowered stays lowered: the smaller of the saved and the configured one wins.
func (e *Engine) loadBudget() {
	b, err := os.ReadFile(e.o.BudgetStore)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var f budgetFile
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		e.budErr = fmt.Errorf("%w: %s: %v", ErrBudgetStore, e.o.BudgetStore, err)
		return
	}
	e.bud = BudgetState{
		LimitUSD: lowest(e.bud.LimitUSD, f.LimitUSD), LimitTok: lowest(e.bud.LimitTok, f.LimitTok),
		SpentUSD: f.SpentUSD, SpentTok: f.SpentTok, Calls: f.Calls, Exhausted: f.Exhausted, maxTok: f.MaxTok, maxUSD: f.MaxUSD,
	}
}

// saveBudgetLocked writes b durably (temp file, fsync, rename). The caller holds liveMu.
// Without a store it does nothing.
func (e *Engine) saveBudgetLocked(b BudgetState) error {
	if e.o.BudgetStore == "" {
		return nil
	}
	data, err := json.Marshal(fileOf(b))
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(e.o.BudgetStore), 0o700); err == nil {
			err = fileutil.WriteFileAtomic(e.o.BudgetStore, data, 0o600)
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetStore, err)
	}
	return nil
}
