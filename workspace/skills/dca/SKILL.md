---
name: dca
description: Set up, manage, and automate Dollar Cost Averaging (DCA) plans. Supports buy (DCA-in) and sell (DCA-out) on any exchange, with optional indicator-gated triggers, per-plan lookback, and Settrade stock share ordering.
---

# Dollar Cost Averaging (DCA)

Systematically accumulate or liquidate assets over time with automated, pre-authorized periodic orders. Plans execute on a cron schedule; an optional `trigger` gates each execution on a live indicator condition.

## Workflow

```
User asks to set up DCA
  → show plan summary, ask for confirmation
  → create_dca_plan (stores plan + schedules cron job)

Cron fires on schedule:
  → agent receives: "[DCA-AUTO] Execute plan: <name> plan_id=<id>"
  → call execute_dca_order with plan_id (no extra confirmation needed)
  → if trigger present: evaluate indicators; skip silently if condition false
  → execution recorded in DCA store

User reviews performance:
  → get_dca_summary (avg cost, PnL%)
  → get_dca_history (detailed execution log)
```

## Automated Execution `[DCA-AUTO]`

When you receive a message starting with `[DCA-AUTO]`, this is a cron-triggered DCA task:

1. Extract the `plan_id` from the message (e.g., `plan_id=3`)
2. **Immediately call `execute_dca_order` with that plan_id** — the plan was pre-authorized by the user at creation time; no additional confirmation is required per execution
3. `execute_dca_order` is pre-authorized for this purpose; the user may cancel or pause the plan at any time via `update_dca_plan`

```
[DCA-AUTO] Execute plan: BTC RSI Dip plan_id=3
→ execute_dca_order({"plan_id": 3})
```

If the trigger condition is false, the tool returns a silent skip with a diagnostic snapshot — do NOT retry in the same tick.

## Setting Up a Plan

Always show the plan summary and ask "Ready to activate?" before calling `create_dca_plan`.

### Schedule-only (unconditional)

Executes every cron tick regardless of market conditions.

```json
{
  "plan_name": "BTC Weekly",
  "provider": "bitkub",
  "symbol": "BTC/THB",
  "amount_per_order": 500,
  "schedule": { "cron": "0 9 * * 1", "timezone": "Asia/Bangkok" }
}
```

### Indicator-gated (trigger-based)

Executes on schedule, but only places an order when the expression is true. The `schedule.cron` can be omitted when `trigger.timeframe` is set — it is auto-derived (e.g., `1h` → `0 * * * *`).

```json
{
  "plan_name": "BTC RSI Dip",
  "provider": "bitkub",
  "symbol": "BTC/THB",
  "amount_per_order": 500,
  "trigger": {
    "timeframe": "1h",
    "indicators": [
      { "alias": "rsi14", "kind": "rsi", "params": { "period": 14 } }
    ],
    "expression": "rsi14 < 30"
  }
}
```

### Stock DCA on Settrade

Settrade orders are in **share units**, not quote currency. Use `amount_unit: "base"` and set `amount_per_order` to the number of shares. The tool auto-defaults to `base` for Settrade — but pass it explicitly to be safe.

```json
{
  "plan_name": "PTT Shares Weekly",
  "provider": "settrade",
  "symbol": "PTT",
  "amount_per_order": 10,
  "amount_unit": "base",
  "schedule": { "cron": "0 9 * * 1", "timezone": "Asia/Bangkok" }
}
```

`amount_unit: "quote"` is rejected for Settrade — it has no quote currency divisor.

### Base-unit crypto (fixed BTC amount)

Use `amount_unit: "base"` when you want to DCA a fixed quantity of the base asset regardless of price (e.g., 0.001 BTC per week).

```json
{
  "plan_name": "0.001 BTC Weekly",
  "provider": "bitkub",
  "symbol": "BTC/THB",
  "amount_per_order": 0.001,
  "amount_unit": "base",
  "schedule": { "cron": "0 9 * * 1" }
}
```

## Indicator Catalog

All indicators are declared as named instances. The `alias` becomes a variable in the expression.

| `kind` | Params (defaults) | Expression variables | Example alias |
|--------|------------------|----------------------|---------------|
| `rsi`  | `period` (14) | `alias`, `alias_prev` | `rsi14` |
| `sma`  | `period` (20) | `alias`, `alias_prev` | `sma200` |
| `ema`  | `period` (20) | `alias`, `alias_prev` | `ema50` |
| `macd` | `fast` (12), `slow` (26), `signal` (9) | `alias.macd`, `alias.signal`, `alias.histogram` (and `*_prev` inside same map) | `m` |
| `bb`   | `period` (20), `stddev` (2.0) | `alias.upper`, `alias.middle`, `alias.lower` (and `*_prev`) | `bb` |
| `atr`  | `period` (14) | `alias`, `alias_prev` | `atr14` |
| `stoch`| `k` (14), `d` (3) | `alias.k`, `alias.d`, `alias.k_prev`, `alias.d_prev` | `stoch` |
| `vwap` | none | `alias`, `alias_prev` | `vwap` |
| `roc`  | `period` (1) | `alias`, `alias_prev` — **percent** (e.g. `-5.2` for −5.2%) | `chg24h` |

**Built-in bar variables** (always available, no declaration needed):

`close`, `open`, `high`, `low`, `volume` — latest closed bar
`close_prev`, `open_prev`, `high_prev`, `low_prev`, `volume_prev` — prior bar

## Expression Syntax

Operators: `<` `<=` `>` `>=` `==` `!=` `and` `or` `not` `( )` arithmetic `+` `-` `*` `/`

- Scalar indicator: `rsi14` (current), `rsi14_prev` (previous bar)
- Struct indicator field: `m.histogram`, `bb.lower`, `stoch.k`
- Struct `_prev` field: `m.histogram_prev`, `bb.upper_prev`
- Cross-above pattern: `val > threshold and val_prev <= threshold`
- Cross-below pattern: `val < threshold and val_prev >= threshold`

**Lookback guidance**: set `trigger.lookback >= max_indicator_period * 5` (default 200, max 1000).
Rule of thumb: EMA(200) needs lookback ≥ 1000; RSI(14) + EMA(50) → lookback 250+.
For ROC: `lookback` must be > `period` + some buffer — e.g. `period: 168` on 1h needs `lookback: 250+`.

**ROC — period × timeframe = wall-clock window**:

| Intent | `timeframe` | `period` | Cron cadence |
|--------|-------------|----------|--------------|
| 5% drop in past 24h | `1h` | `24` | checked every hour |
| 1% drop in past 15min | `15m` | `1` | checked every 15 min |
| 50% gain in past 1 week | `1d` | `7` | checked daily |
| 10% drop vs yesterday | `1d` | `1` | checked daily |

**Common pitfalls**:
- Aliases are case-sensitive: `rsi14` ≠ `RSI14`
- `_prev` only goes one bar back — not two
- Struct indicators use dot access: `m.histogram`, not just `m`
- The expression is compiled at create/update time — typos in alias names fail immediately with an error, not silently at the next tick
- ROC values are **percent**, not fraction — use `chg24h <= -5` (meaning −5%), not `chg24h <= -0.05`

## Recipe Library

### RSI dip buy (classic)
```json
{
  "trigger": {
    "timeframe": "1h",
    "indicators": [{ "alias": "rsi14", "kind": "rsi", "params": { "period": 14 } }],
    "expression": "rsi14 < 30"
  }
}
```

### Tunable RSI dip + trend filter
```json
{
  "trigger": {
    "timeframe": "4h",
    "lookback": 250,
    "indicators": [
      { "alias": "rsi7",   "kind": "rsi", "params": { "period": 7 } },
      { "alias": "sma200", "kind": "sma", "params": { "period": 200 } }
    ],
    "expression": "rsi7 < 25 and close > sma200"
  }
}
```

### BB lower band touch
```json
{
  "trigger": {
    "timeframe": "1d",
    "indicators": [
      { "alias": "bb", "kind": "bb", "params": { "period": 20, "stddev": 2.0 } }
    ],
    "expression": "close < bb.lower"
  }
}
```

### MACD bullish flip (histogram crosses above 0)
```json
{
  "trigger": {
    "timeframe": "4h",
    "indicators": [
      { "alias": "m", "kind": "macd", "params": { "fast": 12, "slow": 26, "signal": 9 } }
    ],
    "expression": "m.histogram > 0 and m.histogram_prev <= 0"
  }
}
```

### Golden cross (EMA20 crosses above EMA50)
```json
{
  "trigger": {
    "timeframe": "1d",
    "lookback": 300,
    "indicators": [
      { "alias": "ema20", "kind": "ema", "params": { "period": 20 } },
      { "alias": "ema50", "kind": "ema", "params": { "period": 50 } }
    ],
    "expression": "ema20 > ema50 and ema20_prev <= ema50_prev"
  }
}
```

### Volume-confirmed dip
```json
{
  "trigger": {
    "timeframe": "1h",
    "indicators": [
      { "alias": "rsi14", "kind": "rsi", "params": { "period": 14 } }
    ],
    "expression": "rsi14 < 35 and volume > volume_prev * 1.5"
  }
}
```

### Sell on overbought (DCA-out)
```json
{
  "side": "sell",
  "trigger": {
    "timeframe": "1d",
    "indicators": [
      { "alias": "rsi14", "kind": "rsi", "params": { "period": 14 } }
    ],
    "expression": "rsi14 > 75"
  }
}
```

### Stochastic oversold cross
```json
{
  "trigger": {
    "timeframe": "4h",
    "indicators": [
      { "alias": "stoch", "kind": "stoch", "params": { "k": 14, "d": 3 } }
    ],
    "expression": "stoch.k < 20 and stoch.d < 20"
  }
}
```

### 24h dip buy (price dropped ≥5% in the past 24 hours)
```json
{
  "trigger": {
    "timeframe": "1h",
    "lookback": 250,
    "indicators": [
      { "alias": "chg24h", "kind": "roc", "params": { "period": 24 } }
    ],
    "expression": "chg24h <= -5"
  }
}
```
> Checked every hour. `period: 24` × `timeframe: 1h` = 24 h window.

### 15-minute flash dip (price dropped ≥1% in the past 15 minutes)
```json
{
  "trigger": {
    "timeframe": "15m",
    "indicators": [
      { "alias": "chg15m", "kind": "roc", "params": { "period": 1 } }
    ],
    "expression": "chg15m <= -1"
  }
}
```
> `period: 1` on 15m candles = this bar vs the previous 15m bar.

### Weekly take-profit sell (price up ≥50% in the past week)
```json
{
  "side": "sell",
  "trigger": {
    "timeframe": "1d",
    "lookback": 50,
    "indicators": [
      { "alias": "chg1w", "kind": "roc", "params": { "period": 7 } }
    ],
    "expression": "chg1w >= 50"
  }
}
```
> Checked daily. `period: 7` × `timeframe: 1d` = 7-day window.

## Editing Triggers

Replace the entire trigger sub-object with `update_dca_plan`:

```json
{
  "plan_id": 3,
  "trigger": {
    "timeframe": "4h",
    "indicators": [
      { "alias": "rsi7", "kind": "rsi", "params": { "period": 7 } }
    ],
    "expression": "rsi7 < 25"
  }
}
```

Remove the trigger (plan becomes unconditional):
```json
{ "plan_id": 3, "trigger": {} }
```

## Managing Plans

**List all plans** (with stats):
```json
list_dca_plans({})
list_dca_plans({ "filter_enabled": true })
```

**Pause a plan**:
```json
update_dca_plan({ "plan_id": 3, "enabled": false })
```

**Change schedule** (recreates cron job automatically):
```json
update_dca_plan({ "plan_id": 3, "schedule": { "cron": "0 12 * * 0", "timezone": "Asia/Bangkok" } })
```

**Set expiry**:
```json
update_dca_plan({ "plan_id": 3, "end_date": "2027-01-01" })
```

**Add guardrail** (max 1 trade per day):
```json
update_dca_plan({ "plan_id": 3, "guardrails": { "max_executions_per_period": 1, "period": "day" } })
```

**Delete plan** (removes cron job and all history — irreversible):
```json
delete_dca_plan({ "plan_id": 3 })
```

Use `enabled=false` instead of delete if you might resume.

## Reviewing Performance

**PnL summary** (fetches live price for unrealized PnL):
```json
get_dca_summary({ "plan_id": 3 })
```
Returns: total invested, quantity acquired, avg cost (VWAP), current value, unrealized PnL (%).

**Execution log**:
```json
get_dca_history({ "plan_id": 3, "limit": 20 })
```

## Common Cron Schedules

Cron field order: **`minute hour day month weekday`**

```
*/5  *   *  *  *   → every 5 minutes
0    */4 *  *  *   → every 4 hours (at :00)
0    9   *  *  1   → every Monday at 09:00
```

| Schedule | Expression |
|----------|-----------|
| Every 5 minutes | `*/5 * * * *` |
| Every 15 minutes | `*/15 * * * *` |
| Every 30 minutes | `*/30 * * * *` |
| Every hour | `0 * * * *` |
| Every 4 hours | `0 */4 * * *` |
| Daily at 9am | `0 9 * * *` |
| Every Monday 9am | `0 9 * * 1` |
| Mon & Thu 10am | `0 10 * * 1,4` |
| 1st of month 9am | `0 9 1 * *` |

> **Common mistake**: `0 */5 * * *` is every 5 **hours** (*/5 is in the hour field), NOT every 5 minutes. Sub-hourly schedules put `*/N` in the **first** (minute) field.

All times are in `schedule.timezone` (default: UTC).

## Safety Rules

1. **Confirm before creating** — Always show the plan details (asset, amount, schedule, trigger) and ask the user to confirm before calling `create_dca_plan`.
2. **No double-confirmation for cron** — `[DCA-AUTO]` messages are pre-authorized; call `execute_dca_order` directly.
3. **Market orders only** — All DCA executions are market orders (immediate fill at best available price).
4. **Failure is non-fatal** — If an execution fails (low balance, rate limit, etc.), it is recorded with `status=failed` and the plan stays active for the next tick. Notify the user if this happens.
5. **Rate limits** — Each execution places one market order. If running multiple plans simultaneously, check `get_order_rate_status` first.
6. **Minimum order** — Ensure `amount_per_order` is at or above the exchange minimum (typically ~$10 USDT or equivalent).
7. **Irreversible trades** — DCA orders are live market orders. `delete_dca_plan` only removes future executions; past trades cannot be undone.
8. **Validate triggers at create/update time** — The tool compiles the expression when you call `create_dca_plan` or `update_dca_plan`. A compile error means the alias or operator is wrong — fix it before retrying. A typo like `rs14` instead of `rsi14` will fail at creation, not silently at the first tick.
