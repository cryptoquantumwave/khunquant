package triggers

import (
	"encoding/hex"
	"fmt"
	"strconv"
)

// The dedup key of every trigger kind is built here, once, so that whoever reads a
// key back (the heartbeat sweep names the asset a key is about) tests against the
// same functions the kinds use. Every key is "<kind>:<subject…>", as the registry
// requires.

// MoveKey is move_vs_vol:<asset>:<window>:tier<n>. The asset is a holding's name as
// the portfolio snapshot has it.
func MoveKey(asset, window string, tier int) string {
	return fmt.Sprintf("%s:%s:%s:tier%d", KindMoveVsVol, asset, window, tier)
}

// DrawdownKey is drawdown:portfolio:<reference>:tier<n>.
func DrawdownKey(ref string, tier int) string {
	return fmt.Sprintf("%s:portfolio:%s:tier%d", KindDrawdown, ref, tier)
}

// DrawdownRebaseKey is drawdown:rebase:<day>.
func DrawdownRebaseKey(day string) string {
	return fmt.Sprintf("%s:rebase:%s", KindDrawdown, cleanKeyToken(day))
}

// DriftKey is allocation_drift:<class label>:<direction>.
func DriftKey(label, dir string) string {
	return fmt.Sprintf("%s:%s:%s", KindAllocationDrift, label, dir)
}

// StaleKey is data_stale:<source>:stale<n>, and StaleClearKey the all-clear
// data_stale:<source>:ok<n>.
func StaleKey(source string, n int) string {
	return fmt.Sprintf("%s:%s:stale%d", KindDataStale, cleanKeyToken(source), n)
}

// StaleClearKey: see StaleKey.
func StaleClearKey(source string, n int) string {
	return fmt.Sprintf("%s:%s:ok%d", KindDataStale, cleanKeyToken(source), n)
}

// LiquidationKey is liquidation_distance:<account>:<symbol>:<side>:<level>, with the
// account as "provider/account" and the symbol a contract symbol.
func LiquidationKey(account, symbol, side, level string) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s", KindLiquidationDistance, cleanKeyToken(account), cleanKeyToken(symbol), side, level)
}

// DNHealthKey is dn_health:<plan number>:<breach code>.
func DNHealthKey(plan, code string) string { return KindDNHealth + ":" + plan + ":" + code }

// DCADueKey is dca_due:<plan number>:<due time in unix milliseconds>.
func DCADueKey(planID, dueUnixMilli int64) string {
	return KindDCADue + ":" + strconv.FormatInt(planID, 10) + ":" + strconv.FormatInt(dueUnixMilli, 10)
}

// PriceCrossKey is price_cross:<provider>:<symbol>:<direction>:<threshold>.
func PriceCrossKey(provider, symbol, direction, threshold string) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s", KindPriceCross, provider, symbol, direction, threshold)
}

// IndicatorCrossKey is
// indicator_cross:<provider>:<symbol>:<timeframe>:<indicator><period>:<direction>:<threshold>.
func IndicatorCrossKey(provider, symbol, timeframe, indicator string, period int, direction, threshold string) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s%d:%s:%s", KindIndicatorCross, provider, symbol, timeframe, indicator, period, direction, threshold)
}

// ExternalKey is external:<name>:<8 hex characters of the message's hash>.
func ExternalKey(name string, hash []byte) string {
	return "external:" + name + ":" + hex.EncodeToString(hash)
}
