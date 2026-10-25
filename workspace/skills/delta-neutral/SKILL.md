---
name: delta-neutral
description: Scan, plan, monitor, and execute spot + perpetual funding-carry strategies on Binance and OKX with explicit portfolio selection and approval-based execution.
---

# Delta-Neutral Funding Strategy

A delta-neutral funding strategy pairs a spot-market long position with a perpetual-futures short position to capture positive funding payments while remaining neutral to price movement. This skill guides you through scanning for opportunities, planning positions, monitoring health, and executing with explicit user approval.

## What This Is

This is **not** a guaranteed-yield product or a hands-off leverage bot. It is a local-first KhunQuant strategy module that:

- Helps you discover funding opportunities across Binance and OKX that remain profitable after fees, slippage, liquidation risk, and exchange risk.
- Requires explicit portfolio and account selection — never infers accounts from provider name alone.
- Demands approval before opening or closing positions.
- Monitors positions with deterministic code; invokes the agent only when thresholds breach or data is unavailable.
- Warns but does not block cross-exchange hedges (spot on one exchange, futures on another).

Core promise: **Find funding opportunities that still make sense after all costs and risks.**

## Workflow

```
User asks for delta-neutral analysis or scan
  → scan watchlist with funding_rate_history, futures_get_funding, market data
  → rank opportunities by net carry (after fees, slippage), funding stability, liquidity
  → explain which are attractive/watch/blocked

User selects an opportunity and asks to create a plan
  → validate futures market with futures_validate_market
  → discover portfolios with list_portfolios
  → user selects spot-leg portfolio and futures-leg portfolio
  → validate both portfolios exist and belong to supported providers
  → estimate sizing, fees (both entry and exit), slippage
  → estimate round-trip breakeven (entry + exit costs for both legs)
  → estimate expected daily funding income
  → estimate liquidation buffer and margin health
  → confirm risk policy (liquidation distance, delta drift, max slippage, etc.)
  → persist plan and register cron monitor job

Monitor fires on configured interval (default 5 minutes):
  → deterministic gate: fetch live spot value, futures position, funding, margin data
  → compute delta drift, funding state, liquidation distance, margin health
  → save monitor snapshot
  → if no threshold breached: return silently (no LLM call)
  → if threshold breached: write alert, notify user, invoke agent for explanation
  → if data unavailable: write failed snapshot, alert immediately (do not silently skip)

User reviews active plans
  → list plans with health labels, latest alerts, monitor snapshots
  → inspect detailed plan summary with portfolios, capital, expected carry, costs

User opens a plan
  → review execution plan: spot leg details, futures leg details, estimated fees, slippage, round-trip breakeven
  → show cross-exchange warning if spot and futures use different exchanges
  → require explicit confirmation before placing any order
  → place spot buy first OR futures short first (exchange-dependent optimization)
  → if first leg fails: abort second leg, no orphaned exposure
  → if second leg fails: enter recovery state, suggest reduce-only close of first leg
  → upon fill, plan status moves to active

User monitors active plan
  → health snapshots track funding received, delta drift, margin ratio, liquidation distance
  → alerts fire for funding reversal, margin degradation, liquidation risk, funding unavailability
  → user can manually rebalance delta or close position at any time

User unwinds a plan
  → require explicit confirmation
  → close futures position (reduce-only) and sell spot holdings
  → record exit costs and final PnL
  → plan status moves to closed
```

## MVP Watchlist (Hardcoded Default)

Scan covers these assets by default:

- BTC/USDT
- ETH/USDT
- SOL/USDT
- BNB/USDT
- XRP/USDT
- ADA/USDT
- DOGE/USDT
- AVAX/USDT
- LINK/USDT
- TON/USDT

This conservative list avoids excessive API calls. Later versions can make it configurable per-user.

## Opportunity Scanning (§7.1)

When you ask to scan or analyze delta-neutral opportunities, follow this workflow:

### Step 1: Validate Futures Symbols

For each asset in the watchlist, call `futures_validate_market` to confirm the asset is available as a USDT perpetual swap on both Binance and OKX:

```json
{
  "provider": "binance",
  "symbol": "BTC/USDT"
}
```

Skip assets where either exchange does not have an active linear swap.

### Step 2: Fetch Funding Data

For each validated asset, call `futures_get_funding` to get the current funding rate:

```json
{
  "provider": "binance",
  "symbol": "BTC/USDT:USDT"
}
```

Then call `funding_rate_history` to fetch 14 days of funding history (typically 56 intervals at 8h each) and compute statistics:

```json
{
  "provider": "binance",
  "symbol": "BTC/USDT",
  "limit": 200
}
```

### Step 3: Fetch Market Data

Call `get_ticker` to retrieve current spot and perpetual prices:

```json
{
  "provider": "binance",
  "symbol": "BTC/USDT"
}
```

Call `get_orderbook` to estimate entry/exit slippage (bid-ask spread and depth):

```json
{
  "provider": "binance",
  "symbol": "BTC/USDT",
  "limit": 20
}
```

Do this for both spot and perpetual orderbooks.

### Step 4: Rank by Net Carry

For each asset, compute:

**Net Daily Carry** (after fees and slippage):

```text
daily_funding_usdt = spot_value × (current_funding_rate × 3 periods/day)
estimated_entry_fee = (spot_amount × taker_fee) + (futures_contracts × taker_fee × mark_price)
estimated_entry_slippage = spot_amount × (bid_ask_spread / 2) + ...
estimated_exit_fee = same as entry
estimated_exit_slippage = same as entry
round_trip_cost = entry_fee + entry_slippage + exit_fee + exit_slippage

net_daily_carry = daily_funding - (round_trip_cost / holding_days)
breakeven_days = round_trip_cost / daily_funding
annualized_rate = (daily_funding / capital) × 365 × 100
```

Rank opportunities by:
1. **Net carry** (highest first)
2. **Funding stability** (lowest volatility first)
3. **Liquidity** (tightest spread, deepest orderbook first)

### Step 5: Warn When Data Is Stale or Unavailable

- **Stale funding data**: If funding history has gaps or the last rate is > 1 hour old, warn "funding data may be stale".
- **Partial data**: If one exchange has funding but the other doesn't, show as "spot-only opportunity on X, futures available on Y".
- **Unavailable symbol**: If futures symbol is not active on an exchange, exclude it from ranking with a reason.
- **Exchange rate-limited**: If an API call fails due to rate limit, note "fetch was rate-limited; try again in N seconds".

### Example Scan Output

```
=== Delta-Neutral Opportunity Scan ===

Watchlist: BTC, ETH, SOL, BNB, XRP, ADA, DOGE, AVAX, LINK, TON

Asset: ETH/USDT
—— Binance ——
  Current funding: 0.0085% / 8h
  3D avg:           0.0076%
  7D avg:           0.0082%
  14D avg:          0.0078%
  Volatility:       ±0.0015%
  Positive ratio:   92%
  Annualized:       ~26.5%
  Spread (spot):    0.01% (tight)
  Spread (perp):    0.015% (tight)
  Status:           ATTRACTIVE

—— OKX ——
  Current funding: 0.0082% / 8h
  3D avg:           0.0075%
  7D avg:           0.0079%
  14D avg:          0.0076%
  Volatility:       ±0.0018%
  Positive ratio:   88%
  Annualized:       ~25.5%
  Spread (spot):    0.012%
  Spread (perp):    0.02%
  Status:           ATTRACTIVE

Best same-exchange pair: Binance
Cross-exchange option: Binance spot + OKX futures (25.95% annualized, cross-exchange warning applies)

Estimated daily carry (Binance, 10k USDT capital):
  Funding (gross):          2.31 USDT/day
  Entry cost (0.05% taker, 0.3% slippage): 38 USDT
  Exit cost (same):                         38 USDT
  Round-trip cost:                          76 USDT
  Daily carry (net):                        2.03 USDT/day
  Breakeven:                                37 days
  Liquidation buffer (2x leverage):         50%

Status: ATTRACTIVE — positive funding, tight spreads, good liquidation buffer.
Next: Create a plan and select portfolios.

—————————

Asset: SOL/USDT
[... similar breakdown ...]

Status: WATCH — funding turning negative over last 7D; monitor before opening.

—————————

Asset: DOGE/USDT
Binance: Status BLOCKED — funding is negative (-0.002% / 8h).
OKX:     Status WATCH — funding near zero.
Recommendation: Skip for now. Carry risk outweighs benefits.
```

## Bulk Opportunity Scan (`scan_delta_neutral_opportunities`)

When you ask to scan **many pairs at once**—e.g. "scan arbitrage opportunities on the top 300 pairs", "find the best funding plays", "what's the highest funding right now"—use this tool instead of the per-symbol workflow. It executes a **two-stage batch screen** that ranks hundreds of candidates efficiently.

### When To Use

- **Broad market screen**: You want to discover the top N funding opportunities across 100–500 coins without deep analysis on each.
- **Fast initial ranking**: Stage 1 fetches all funding rates in one batch call (cheap even for 300 symbols).
- **Stability validation (optional)**: Stage 2 adds 7d/14d funding statistics for only the top K, skipping API calls on low-ranking opportunities.

This is a **funding-only screen**. After it returns the ranked shortlist, **drill down on the top picks** with per-symbol tools (`get_orderbook` for liquidity, `futures_risk_summary` for margin health) before committing capital.

### How It Works

**Stage 1: Rank all candidates by annualized funding APR.**

1. Fetch the top N crypto assets by market-cap rank from CoinMarketCap (default: top 100, cap 500).
2. For each asset, build a perpetual futures symbol (e.g., `BTC/USDT:USDT` for Binance).
3. Batch-fetch current funding rates for all candidates using `FetchFuturesFundingRates` (one API call).
4. Compute annualized APR: `APR = funding_rate × periods_per_day × 365 × 100` (percent).
5. **Filter and sort**:
   - Exclude assets with no perp on the chosen exchange.
   - Exclude assets below `min_abs_funding_apr` threshold (if set).
   - Sort by absolute APR descending (highest funding opportunities first).

**Stage 2 (optional): Add stability statistics for top K.**

When `include_stability=true` (default), fetch 7d and 14d funding history for only the top K ranked assets:
- Compute 7-day mean + standard deviation.
- Compute 14-day mean + standard deviation.
- Use this data to label opportunities: **attractive** (positive + stable), **watch** (near-zero or volatile), or **blocked** (no data).

### Funding Direction

The tool identifies both directions:
- **Positive funding** (e.g., +0.01% per 8h): Longs pay shorts. You go **spot-long + short-perp** to earn funding.
- **Negative funding** (e.g., −0.003% per 8h): Shorts pay longs. You go **long-perp + spot-short** (inverse strategy) to earn funding.

Each opportunity is labeled with its direction: **"short perp"** (positive funding, your short perp receives funding) or **"long perp"** (negative funding, your long perp receives funding).

### Parameters

```json
{
  "provider": "binance",
  "account": "",
  "top_n": 100,
  "quote": "USDT",
  "limit_results": 20,
  "include_stability": true,
  "top_k_stability": 15,
  "min_abs_funding_apr": 0,
  "cmc_base_url": ""
}
```

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `provider` | string | | "" / "all" | Exchange: `"binance"`, `"okx"`, or empty/`"all"` to scan **every** supported exchange and combine the ranked results (each row tagged with its exchange) |
| `account` | string | | "" | Account name (empty = default account) |
| `top_n` | integer | | 100 | Top N coins by market cap to screen (max 500) |
| `quote` | string | | "USDT" | Quote currency for futures symbols |
| `limit_results` | integer | | 20 | Limit output table rows to top N |
| `include_stability` | boolean | | true | Fetch 7d/14d funding stats for top K (Stage 2) |
| `include_earn` | boolean | | true | Fetch flexible spot-earn APY and show Earn%/Combined% columns (Stage 1.5). Earn data is account-scoped on Binance and public on OKX; rows without earn show '-'. |
| `top_k_stability` | integer | | 15 | Stage 2: fetch funding history for top K ranked assets |
| `sort_by` | string | | "funding_rate" | Field to sort by: `funding_rate`, `apr`, `7d_avg`, `14d_avg`, `combined_apy`, or `combined_apy_30d`. Sorting by `7d_avg`/`14d_avg` requires computing stability for all candidates (more API calls). |
| `sort_order` | string | | "desc" | Sort direction: `asc` (most-negative first) or `desc` (most-positive first) |
| `min_abs_funding_apr` | number | | 0 | Filter: exclude \|APR\| below this % (optional) |
| `cmc_base_url` | string | | CoinMarketCap API | Override CMC endpoint (testing/custom) |

### Example: Scan Top 300 Pairs on Binance

```json
{
  "provider": "binance",
  "top_n": 300,
  "include_stability": true,
  "limit_results": 30
}
```

This fetches top 300 coins by CMC market cap, ranks them by APR on Binance perpetuals, and returns the top 30 in the output table.

### Example: Scan All Supported Exchanges

```json
{
  "top_n": 300,
  "limit_results": 30
}
```

Omitting `provider` (or passing `"all"`) scans **every** supported exchange (currently Binance and OKX) and merges them into one ranked table, so the same coin can appear once per exchange and you see where the funding is richest. Each row's **Exch** column shows which exchange it came from; if one exchange can't be reached, the others still return with a partial-results caution. (New exchanges are picked up automatically as they're added to the scanner's supported list.)

### Sample Output

```
=== Delta-Neutral Funding Carry Scan ===
Exchanges scanned: binance, okx
Sorted by: combined_apy desc

Rank  Exch    Asset Futures         Spot     Funding%   APR%   Direction   7d Mean%   7d Std%  14d Mean%  14d Std%     Earn%  Combined% Label
—————————————————————————————————————————————————————————————————————————————————————————————————————————————————————————————
1     okx     ETH   ETH/USDT:USDT   yes      0.008500   26.22  short perp  +0.0082    0.0015    +0.0078    0.0014   +3.5000     +29.72 attractive
2     binance BTC   BTC/USDT:USDT   yes      0.006200   22.77  short perp  +0.0061    0.0010    +0.0059    0.0012   +2.0000     +24.77 attractive
3     binance SOL   SOL/USDT:USDT   yes      0.005100   18.67  short perp  +0.0048    0.0025    +0.0050    0.0028   +4.5000     +23.17 watch
4     binance XYZ   XYZ/USDT:USDT   NO-SPOT  0.005400   19.71  short perp  +0.0050    0.0030    +0.0052    0.0031        -      +19.71 watch
5     okx     ADA   ADA/USDT:USDT   yes      0.003200   11.72  short perp  +0.0031    0.0020    +0.0030    0.0021        -      +11.72 watch
...

⚠️  No spot pair on its exchange for: XYZ — funding rank is still valid, but the delta-neutral spot leg cannot be opened there (source spot elsewhere, or treat as futures-only).
Spot column: 'yes' = spot pair available | 'no-spot' = perp only on that exchange | 'unknown' = could not verify.
Earn% column: '-' = no earn product available | percentage = flexible-savings APY (tiered; varies by deposit amount).
Combined% = Funding APR + Earn APY (your total annualized yield if holding both legs).
Note: Funding+earn screen — drill into top picks with get_orderbook/futures_risk_summary before building a plan.
Legend: 'attractive' = positive funding + stable | 'watch' = near-zero/unstable | 'blocked' = no perp or no funding
```

The **Exch** column shows which exchange each row came from (when scanning a single exchange it's constant). The **Spot** column flags whether the asset also has a spot pair on that exchange. Assets with a perp but **no spot** are **kept** in the ranked list (so you still see correct sorted funding data) and marked `NO-SPOT` — their funding rank is real, but the delta-neutral spot leg can't be opened on that exchange (source the spot elsewhere, or treat as futures-only). `unknown` means spot markets couldn't be verified.

### After The Scan: Drill Down Before Opening

A high funding APR alone does not guarantee a good trade. Before creating a plan with `create_delta_neutral_plan`:

1. **Check liquidity** for the top picks: Call `get_orderbook` on both spot and perpetual. Verify spreads are tight (< 0.5%) and orderbook depth is sufficient for your planned capital.
2. **Check margin health**: Call `futures_risk_summary` to estimate liquidation distance with your target leverage. Ensure liquidation buffer ≥ 25%.
3. **Check stable funding**: If `include_stability` was false, call `funding_rate_history` manually for the top picks to confirm recent trend (not flipping negative).
4. **Estimate total costs**: Use the per-symbol workflow (Steps 1–5 in Opportunity Scanning) to compute round-trip costs, net carry, and breakeven days.
5. **Select and confirm**: Only then create a plan via `create_delta_neutral_plan` with your chosen spot and futures portfolios.

## Projected Yield — Dry-Run Mode (§7.1.4b)

When a user asks **"how much would I earn per year from X if I put Y USDT into delta-neutral?"**, call `get_delta_neutral_summary` directly — **no plan creation needed**:

```
get_delta_neutral_summary(
  spot_provider = "binance",   // or "okx"
  spot_symbol   = "DOT/USDT",
  capital_usdt  = 100
)
```

The tool fetches live funding rate and earn APY from the exchange and returns:
- `Funding APY%` — annualised funding rate (using the actual settlement interval: 1h/4h/8h)
- `Earn APY%` — best flexible-earn product APY for the base asset
- `Combined APY%` — total annualised yield
- `Annual combined` — projected USDT income per year (based on ~50% spot notional of capital)
- `Daily combined` — projected USDT income per day
- `Breakeven (funding only)` — days to recover entry + exit costs from funding alone
- `Breakeven (funding + earn)` — days to break even including earn yield

Optional parameters: `futures_symbol` (auto-derived as `DOT/USDT:USDT`), `futures_provider`, `spot_account`, `futures_account`, `leverage` (default 1).

### Example answer for "how much from DOT at 100 USDT on Binance?"

```
Dry-run yield projection: DOT/USDT on binance · 100.00 USDT capital

⚡ PROJECTED YIELD (live market rates)
  Funding APY:        21.90%  (rate 0.000100, interval 4h)
  Earn APY:            3.20%
  Combined APY:       25.10%

  Annual combined:   +12.55 USDT  (25.10% × 50.00 USDT spot notional)
  Breakeven (funding + earn): 2.93 days
```

> Note: projections use live rates which change each funding period. Actual returns will vary.
> The notional used is `capital × 0.5` (50 USDT of 100), as ~half the capital goes to spot
> and the other half covers futures margin at 1× leverage.

## Earning the Spot Leg (§7.1.5)

The spot-long leg of a delta-neutral strategy can earn additional yield from flexible-savings products offered by the exchange, **on top of the funding APR from the futures short leg**. This earn APY compounds the total return, though it is **variable, tiered, and not guaranteed**.

### How It Works

- **Binance**: The spot leg can be enrolled in a flexible savings product (e.g., BTC Flexible Savings). Funds are swept daily (at 02:00 UTC and 16:00 UTC) into the product and earn APY. **Auto-subscribe is configurable via API** using `set_auto_subscribe`; once enabled, new deposits are swept automatically.
- **OKX**: Flexible earn sweeps are pulled from the **Funding account** (not Spot directly). Use `Transfer` to move funds from Trading → Funding first, then subscribe. OKX's **auto-subscribe toggle is app-only** (no API method); you must configure it in the OKX mobile app or web console.

### Key Rules

- **Redeem before selling**: If unwinding a position, **redeem the spot leg from earn FIRST**, then sell the underlying. Failing to redeem means your spot holdings remain locked in earn while the futures short is already closed, leaving you with unhedged long exposure and ongoing fund locks.
- **APY is tiered by amount**: Higher deposited amounts typically earn higher APY. The tool shows the **product-level APY** when available; your actual APY depends on which tier your deposit amount falls into.
- **Rewards compound**: Earned rewards are automatically reinvested (auto-compound on most products), so your balance grows faster than the stated APY.

### Scanner Integration

When you scan opportunities with `include_earn=true` (default), the scanner fetches flexible-earn APY for each asset and shows:

- **Earn% column**: APY of the flexible-earn product for that asset on that exchange (e.g., 3.5%). Shows "-" if no product is available.
- **Combined% column**: Funding APR + Earn APY (e.g., 5.2% funding + 3.5% earn = 8.7% combined). This is the total annualized yield if you hold both legs. In `get_delta_neutral_summary` and the web Yield card, the 3M/6M/12M combined values pair their respective funding and earn window averages for accuracy (3M combined = 3M funding avg + 3M earn avg, not the current earn rate).
- **combined_apy sort field**: Sort opportunities by `combined_apy` (desc by default) to rank by total return, not funding alone. This often changes which opportunities rank at the top.

### Tools

- `earn_overview` (read): Fetch and display all available products and your current positions. Degrades gracefully if positions require API keys you don't have.
- `manage_earn_position` (write, confirm-gated): Subscribe, redeem, or toggle auto-subscribe for an earn product. Dry-run with `confirm=false`; execute with `confirm=true`.

## Spread (Price Basis) Monitoring (§7.1.6)

In addition to funding rates, delta-neutral strategies depend on the **price basis spread** between spot and futures markets. A favorable spread at entry and exit can significantly improve returns, while an unfavorable spread can erode expected profit. KhunQuant monitors and gates on these spread metrics to help you capture better entry prices and exit opportunistically.

### Key Concepts

**Entry Spread** (futures relative to spot):
```
entry_spread % = (futuresPrice - spotPrice) / spotPrice × 100
```

- **Positive spread** = Futures premium (carry exists; you collect funding from this premium).
- **Negative spread** = Futures discount (most common; futures trading at a discount to spot).
- **Higher (less negative) = more favorable**: For example, -0.148% is more attractive than -0.300% because you pay less to open the hedge.

Example: Spot price 0.7435 USD, futures price 0.7424 USD → entry spread = (0.7424 - 0.7435) / 0.7435 × 100 = **-0.148%**

**Exit Spread** (spot relative to futures; basis convergence):
```
exit_spread % = (spotPrice - futuresPrice) / futuresPrice × 100
```

- **Positive spread** = Basis has converged or inverted; spot is now above futures (favorable exit opportunity).
- **Example**: Futures 0.7424, spot 0.7435 → exit spread = (0.7435 - 0.7424) / 0.7424 × 100 = **+0.148%**

### Entry Spread Gating

When you set `entry_rules.min_entry_spread_pct` in a plan (default: 0 = disabled), the system enforces a **minimum spread threshold** before opening a position.

**How it works:**
- When you call `open_delta_neutral_position`, the tool fetches live spot and futures prices.
- It computes the current entry spread.
- If live spread ≥ `min_entry_spread_pct`: Position opens (green light).
- If live spread < `min_entry_spread_pct`: Opening is blocked with a clear error; no orders are placed.

**Use cases:**
- **Avoid expensive entries**: Set `min_entry_spread_pct = -0.1` to ensure you only open when the futures discount is at least -0.1%. Skip trades where the spread is too wide or has inverted.
- **Wait for better prices**: In choppy markets, spreads fluctuate. Set a threshold and try opening again when the spread improves.
- **Dry-run to scout**: Call `open_delta_neutral_position` with `confirm=false` to see the live spread vs. your target without placing orders.

Example: You want to trade ETH but only if the futures discount is at least -0.05%. Set `entry_rules.min_entry_spread_pct = -0.05`. The tool will block entry if the current spread is -0.3% or worse.

### Exit Spread Targeting

When you set `exit_rules.target_exit_spread_pct` in a plan (default: 0 = disabled), the monitor watches for a **favorable convergence** of the basis at which it recommends unwinding the position.

**How it works:**
- During each monitor tick, the system computes the current exit spread.
- If exit spread ≥ `target_exit_spread_pct`: Condition is met; the plan issues a recommendation or auto-unwinds (depending on execution mode).
- The breach code is `exit_spread_target_met`; recommended action is "unwind".

**Execution mode behavior:**
- **`monitor`** / **`approval`**: Alert + recommend unwind; wait for your approval.
- **`semi_auto`** / **`full_auto`**: Automatically unwind the position when spread target is reached.

**Use cases:**
- **Lock in gains early**: Set `target_exit_spread_pct = 0.05` to exit automatically if the basis converges 0.05% or more in your favor, capturing quick wins without waiting for full funding payoff.
- **Scalp basis spreads**: Use tight thresholds (e.g., 0.01%) for high-frequency exits on stable coins.
- **Let funding accrue**: Set `target_exit_spread_pct = 0` (disabled) to ignore spread entirely and close only on funding targets or manual signal.

Example: You opened a 5,000 USDT ETH position with entry spread -0.15%. Set `target_exit_spread_pct = 0.05` to exit when the spread has converged 0.20% (to +0.05%), locking in basis gains. The monitor will recommend or auto-close at that point.

### Spread Monitoring in the Web UI

Active plans display live entry and exit spreads in the **Spread card**:
- **Entry spread (%)**: Current market spread vs. your entry point.
- **Exit spread (%)**: Basis convergence window (how much profit is available from spread alone).
- **Historical chart**: 30-day history of both spreads and funding APR for correlation analysis.

Monitor snapshots also capture `entry_spread_pct` and `exit_spread_pct` at each tick for historical reference.

### Fetching Spread Data On-Demand

Use the `get_delta_neutral_spread` tool to fetch current and historical spreads for any symbol pair:

```json
{
  "spot_provider": "binance",
  "spot_symbol": "ETH/USDT",
  "futures_provider": "binance",
  "futures_symbol": "ETH/USDT:USDT"
}
```

Returns:
- **Current entry spread %** and **current exit spread %**
- **3-day, 7-day, 14-day, 30-day funding APR windows** (for context)
- Formatted output showing spread percentages and APR history

Use this to scout opportunities, validate spread assumptions before creating a plan, or evaluate historical spread behavior on a symbol pair.

## Funding Analysis (§7.2)

The funding interpretation labels guide you toward safe opportunities:

### Attractive

- Current funding rate is **positive and above your configured minimum** (default: 0.01% per period).
- **Recent trend is stable or rising** (3D, 7D, 14D averages are all positive and similar).
- **Volatility is low** (standard deviation < 0.003% for example).
- **Positive-funding ratio is high** (> 80% of recent periods are positive).
- **No reversal warning** (not approaching zero or negative territory).

*Recommendation*: Good candidate for position opening.

### Watch

- Current funding is **positive but unstable** (high volatility, ±0.002%+).
- OR funding is **near zero** (between -0.0005% and +0.0005%) and decaying.
- OR **positive-funding ratio is dropping** (was 90%, now 70%).
- OR **reversal pattern detected** (flipped from positive to negative in the last 1–2 cycles).
- OR **spread is wide** (entry/exit slippage estimated > 0.5%).

*Recommendation*: Monitor before opening. Consider waiting for stability. If opening, reduce size or tighten stop-loss.

### Blocked

- Current funding is **negative** (longs paying shorts; no carry for you).
- OR funding is **unavailable** (no recent data, exchange API error).
- OR **volatility is extreme** (std dev > 0.005% or reversal is consistent).
- OR **your risk policy minimum** has not been met (e.g., you require min 0.02% funding; current is 0.015%).

*Recommendation*: Do not open. Wait for market to turn positive, or choose a different asset.

## Plan Creation (§7.3)

### Portfolio Discovery

Before creating a plan, call `list_portfolios` to show the user which accounts are available:

```json
list_portfolios({})
```

This returns:
```json
[
  { "provider": "binance", "account": "spot-trading", "balance_btc": 0.5 },
  { "provider": "binance", "account": "futures-trading", "balance_usdt": 10000 },
  { "provider": "okx", "account": "default", "balance_usdt": 5000 },
  { "provider": "bitkub", "account": "primary", "balance_thb": 50000 }
]
```

### User Portfolio Selection

1. **Show available portfolios** grouped by provider.
2. **Ask user: "Which portfolio do you want to use for the spot long leg?"**
   - Must be on a provider that supports spot: `binance` or `okx`.
   - `bitkub` and `binanceth` are spot-only; they cannot be the futures leg.
3. **Ask user: "Which portfolio do you want to use for the futures short leg?"**
   - Must be on a provider that supports futures: `binance` or `okx`.
   - Reject `bitkub` and `binanceth` with "This provider does not support perpetual futures in KhunQuant."

**Critical**: Never infer a money-moving account from provider name alone. If the user has multiple Binance accounts, ask which one.

### Validation And Sizing

1. Validate futures market exists with `futures_validate_market`:
   ```json
   {
     "provider": "binance",
     "symbol": "ETH/USDT:USDT"
   }
   ```
   Capture: contract size, min notional, max leverage, settlement currency.

2. Fetch spot portfolio balances with `get_assets_list`:
   ```json
   {
     "provider": "binance",
     "account": "spot-trading"
   }
   ```
   Confirm the spot account has capital to fund the position.

3. Ask user for **total capital to allocate** (e.g., 10,000 USDT for spot long, same 10,000 USDT notional for futures short).

4. Estimate **spot leg sizing**:
   ```
   spot_amount = capital / spot_price
   spot_notional = capital
   ```

5. Estimate **futures leg sizing** (typically 1x leverage for delta-neutral):
   ```
   futures_contracts = capital / (mark_price × contract_multiplier)
   futures_notional = capital (at 1x leverage)
   ```

### Cost Estimation (§7.3, item 9)

Call `futures_estimate_funding_fee` to estimate the next funding payment:

```json
{
  "provider": "binance",
  "symbol": "ETH/USDT:USDT"
}
```

Returns next funding timestamp and estimated fee (positive = you pay, negative = you receive).

Compute:

```text
estimated_entry_taker_fee_pct = 0.0005   [Binance maker: 0.0002, taker: 0.0005]
estimated_entry_slippage_pct = 0.003     [0.3%, conservative mid-cap spread estimate]

spot_entry_cost = capital × (estimated_entry_taker_fee_pct + estimated_entry_slippage_pct)
futures_entry_cost = capital × (estimated_entry_taker_fee_pct + estimated_entry_slippage_pct)
total_entry_cost = spot_entry_cost + futures_entry_cost

spot_exit_cost = capital × (estimated_exit_taker_fee_pct + estimated_exit_slippage_pct)
futures_exit_cost = capital × (estimated_exit_taker_fee_pct + estimated_exit_slippage_pct)
total_exit_cost = spot_exit_cost + futures_exit_cost

round_trip_cost = total_entry_cost + total_exit_cost

daily_funding = capital × current_funding_rate × 3 periods/day
expected_daily_funding = daily_funding - (round_trip_cost / holding_days)

breakeven_days = round_trip_cost / daily_funding
```

### Risk Policy

Set or accept default thresholds:

- **Min liquidation distance** (default 25%): Alert if liquidation buffer drops below 25%.
- **Max delta drift** (default 3%): Alert if spot value and futures notional diverge by > 3%.
- **Max slippage expected** (default 20 bps = 0.2%): Warn if estimated slippage exceeds this.
- **Funding reversal cycles** (default 2): Alert if funding is negative for 2+ consecutive monitoring periods.
- **Max leverage** (for spot/futures balance; default 1x): Reject plans that require unsafe leverage.
- **Min entry spread** (`entry_rules.min_entry_spread_pct`, default 0 = disabled): Minimum entry spread (%) required to open a position. Example: -0.1 means "only open if futures discount is at least -0.1%". Set to 0 to disable spread gating.
- **Target exit spread** (`exit_rules.target_exit_spread_pct`, default 0 = disabled): Target exit spread (%) that triggers an unwind recommendation or auto-close. Example: 0.05 means "close when basis converges 0.05% in my favor". Set to 0 to disable spread-based exit targeting.

### Monitor Interval

Ask user for monitoring frequency. **Default: 5 minutes.**

Supported intervals (all deterministic; no LLM invocation unless threshold breach):

- 30 seconds ⚠️ (shows rate-limit warning in UI)
- 1 minute ⚠️ (shows rate-limit warning in UI)
- 3 minutes
- 5 minutes (default)
- 10 minutes
- 15 minutes
- 30 minutes
- 1 hour
- 2 hours
- 3 hours
- 4 hours
- 8 hours
- 1 day

### Same-Exchange Warning

If `spot_provider != futures_provider`, show this warning before confirmation:

```text
⚠️ Cross-Exchange Warning

This plan uses spot on Binance and futures on OKX. This is allowed, but it has higher 
execution and unwind risk than running both legs on the same exchange. Recommendation: 
use the same exchange for both legs when available.

Do you want to proceed? [Yes/No]
```

Allow the user to proceed after explicit acknowledgment.

### Plan Summary (Before Confirmation)

Display this before asking for final approval:

```text
=== Delta-Neutral Plan Summary ===

Name:                     ETH Funding Carry (10k USDT)
Asset:                    ETH/USDT
Status:                   draft

Spot Leg
  Provider:               Binance
  Account:                spot-trading
  Symbol:                 ETH/USDT
  Action:                 Buy 5.2 ETH
  Capital:                10,000 USDT
  Entry price (current):  1,923 USDT
  Entry slippage:         ~30 USDT (0.3%)
  Reserve margin:         500 USDT (5% safety buffer)

Futures Leg
  Provider:               Binance
  Account:                futures-trading
  Symbol:                 ETH/USDT:USDT
  Action:                 Short 5.2 ETH @ 1x leverage
  Contracts:              5.2 (contract size = 10)
  Entry slippage:         ~30 USDT (0.3%)
  Mark price (current):   1,923 USDT

Costs & Carry
  Est. entry cost:        ~60 USDT (both legs, 0.05% taker + slippage)
  Est. exit cost:         ~60 USDT (both legs)
  Total round-trip cost:  ~120 USDT
  Current funding:        0.0085% / 8h → ~2.44 USDT/day
  Expected daily carry:   ~2.04 USDT/day (after costs)
  Breakeven:              ~59 days
  Liquidation buffer:     50% (with 1x leverage)

Risk Policy
  Min liquidation dist:   25%
  Max delta drift:        3%
  Max slippage:           0.2%
  Monitor interval:       5 minutes
  Funding reversal alert: 2 consecutive negative periods

Exchange:                 Same (Binance both legs) ✓
Recommendation:           Ready to open. Funding is attractive, costs are manageable.

Ready to activate? [Confirm/Cancel]
```

Upon confirmation, persist the plan and register cron monitor job.

## Monitoring (§7.5)

Once a plan is active, the monitor job fires at the configured interval (default 5 minutes).

### Deterministic Gate

The monitor does **not call the LLM** for routine ticks. It:

1. Loads the plan from SQLite.
2. Skips if plan is disabled, archived, or closed.
3. Fetches live data:
   - Spot balance: `get_assets_list`
   - Spot price: `get_ticker`
   - Futures position: `futures_get_positions`
   - Futures funding: `futures_get_funding`
   - Futures risk: `futures_risk_summary`
4. Computes health metrics:
   - **Delta drift**: `abs(spot_value - abs(futures_notional)) / max(spot_value, abs(futures_notional)) × 100`
   - **Funding state**: current rate, trend, volatility, positive-funding ratio
   - **Liquidation distance**: `abs(mark_price - liquidation_price) / mark_price × 100`
   - **Margin state**: margin ratio, health label (safe / warn / critical)
   - **Health score**: 0–100 weighted by funding (20pts) + margin (25pts) + delta (20pts) + liquidity (10pts) + exchange-risk (10pts) + profit progress (15pts)
5. Writes monitor snapshot to SQLite.
6. **Compares snapshot against risk policy**:
   - Is liquidation distance below min threshold? → breach
   - Is delta drift above max threshold? → breach
   - Is funding negative when it was positive last tick? → breach (reversal)
   - Is funding unavailable? → breach
   - Is spot balance missing or insufficient? → breach
   - Is futures position missing or side-mismatched? → breach
7. **If NO breach**: Return silently. No alert, no LLM call.
8. **If breach or data error**: Write alert row, send notification, invoke agent for explanation.

### Data Unavailability Is Not Silent

If required data cannot be fetched (exchange API down, rate-limited, account permission error):

- Write failed snapshot with `data_status=error`.
- Create alert immediately.
- Invoke agent with explanation: "Monitor data unavailable for plan XYZ: [error details]."
- Do **not** silently skip the tick for active plans.

This ensures you are never left unaware of monitoring failures.

## Safety Rules

1. **Always confirm before execution**: No live open, close, reduce, or transfer without explicit user approval. Display exact order details (symbol, side, amount, price, fees, slippage) and require "yes" to proceed.

2. **Use live tools**: Every plan check and execution uses live market and portfolio data via tools like `get_ticker`, `get_orderbook`, `futures_get_positions`, `get_assets_list`. Never rely on stale assumptions.

3. **State stale or unavailable data clearly**: If funding data is > 1 hour old, say so. If an exchange API is slow, say so. Do not guess or hide fetch failures.

4. **Futures mutations require permission**: All futures operations require `trading_risk.allow_leverage=true` in the agent config. If not set, reject with "Futures trading is disabled in your config."

5. **Spot-only providers cannot be futures legs**: Reject `bitkub` and `binanceth` for the futures leg. Accept `binance` and `okx` only.

6. **Validate portfolios before binding**: Always call `list_portfolios` and let the user select. Never infer "the user probably meant this account."

7. **Cross-exchange hedge warning**: If spot and futures are on different exchanges, always show the warning and require acknowledgment.

8. **No silent skip on data failure**: If active-plan monitoring cannot fetch required data, alert immediately rather than silently skipping the tick.

9. **Recovery on execution failure**: If placing the first leg fails, abort the second leg. If the first leg fills and the second fails, enter recovery state and propose a reduce-only close of the first leg.

10. **Never log secrets**: Do not log API keys, account numbers, or seed phrases. Log only non-sensitive plan metrics and status.

## Tools Reference

This skill uses the following existing tools. Note that **dedicated delta-neutral tools** (`create_delta_neutral_plan`, `list_delta_neutral_plans`, `get_delta_neutral_plan`, `update_delta_neutral_plan`, `delete_delta_neutral_plan`, `get_delta_neutral_summary`, `get_delta_neutral_history`, `open_delta_neutral_position`, `unwind_delta_neutral_position`) will be available once the delta-neutral backend is complete. Until then, this skill works using the tools listed below:

### Funding & Futures

- `funding_rate_history` — Fetch public funding rate history and compute rolling statistics (3d/7d/14d mean, max, min, volatility, annualized rate).
- `futures_get_funding` — Current funding rate and next funding timestamp for a symbol (public API, no credentials needed).
- `futures_validate_market` — Validate that a futures symbol is active and tradeable; returns contract size, leverage limits, settlement currency (public API).
- `futures_estimate_funding_fee` — Estimate the next funding payment for a symbol or all open positions.
- `futures_risk_summary` — Summarize all open futures positions: margin health, liquidation distance, unrealized PnL, margin ratio (read-only, no trading).
- `futures_get_positions` — List current futures positions with contracts, leverage, entry, mark, unrealized PnL.

### Market Data

- `get_delta_neutral_spread` — Fetch live entry/exit spread + 3d/7d/14d/30d funding APR for a symbol pair. Parameters: `spot_provider`, `spot_symbol`, `futures_provider`, `futures_symbol` (+ optional `spot_account`, `futures_account`). Returns formatted text with spread percentages and APR windows. Use to scout entries and validate spread assumptions before creating a plan.
- `get_delta_neutral_earn` — Fetch flexible-savings (earn) APY for a spot asset: the current best rate plus trailing **3M/6M/12M averages** and min/max from the exchange's earn rate history (paginated: hourly on OKX, daily on Binance, up to ~1y). Parameters: `provider`, `asset` (e.g. `"ZEC"` or `"ZEC/USDT"`), optional `account`. OKX rates are **public** (no credentials needed); Binance Simple Earn requires credentials. Use to gauge the earn leg independently before building a plan, or to verify the earn windows shown in `get_delta_neutral_summary`.
- `get_ticker` — Current bid/ask price and 24h volume for a symbol.
- `get_orderbook` — Order book snapshot; use to estimate slippage (spread, depth).
- `get_ohlcv` — Historical price bars; use to correlate funding spikes with price moves.

### Portfolio & Assets

- `list_portfolios` — Discover all configured portfolio accounts (provider + account name) and their balances.
- `get_assets_list` — Retrieve asset balances from a specific portfolio (provider + account).
- `get_total_value` — Estimate total portfolio value in a quote currency by fetching all balances and live prices.
- `get_pnl_summary` — Compute plan-level PnL if the delta-neutral plan storage is available; otherwise, use generic account PnL.
- `take_snapshot` — Capture portfolio snapshot for historical tracking.

### Flexible Earn (Optional Spot-Leg Yield)

- `earn_overview` — Fetch and display available flexible-savings products and your current positions across one or more exchanges. Degrades gracefully if position data requires API keys. Read-only, no confirm gate.
- `manage_earn_position` — Subscribe, redeem, or toggle auto-subscribe for a flexible-earn product. Write tool; confirm-gated (dry-run with `confirm=false`; execute with `confirm=true`). Supports `provider` (single exchange, no "all"), `account`, `action` (subscribe|redeem|set_auto_subscribe), `asset`, and optional `amount`, `redeem_all`, `auto_subscribe`.

### Execution (Once Available)

- `create_delta_neutral_plan` — Create and persist a plan; registers cron monitor job. Accepts `leverage` parameter and `risk_policy.max_leverage`; sizes both legs to equal notional; rejects if leverage exceeds max. Supports `entry_rules.min_entry_spread_pct` and `exit_rules.target_exit_spread_pct` for spread-based entry gating and exit targeting.
- `list_delta_neutral_plans` — List all active and inactive plans with health status.
- `get_delta_neutral_plan` — Retrieve a specific plan and its recent monitor snapshots, alerts, and execution history. For draft/ready plans with no open position, automatically fetches live funding rate and earn APY and shows projected annual yield and breakeven days.
- `update_delta_neutral_plan` — Update plan name, monitor interval, risk thresholds, spread parameters, or pause/resume. Accepts `leverage` parameter: for draft/ready plans, stored for next open; for active plans, requires `confirm=true` and applies live on exchange with liquidation-distance re-validation.
- `delete_delta_neutral_plan` — Delete a draft or closed plan.
- `get_delta_neutral_summary` — Get plan P&L summary, or **dry-run yield projection with no plan needed** (pass `spot_provider` + `spot_symbol` + `capital_usdt` instead of `plan_id`). Active plans: live health, P&L breakdown, daily yield, and breakeven. Draft/ready plans: live-fetched projection. Dry-run: instant projection for any asset/exchange/capital without creating anything. The projection reports **Earn APY for current / 3M / 6M / 12M windows** (from earn rate history) and **Combined APY that pairs the matched funding and earn windows** (3M combined = 3M funding avg + 3M earn avg, etc.) — not a windowed funding rate mixed with a point-in-time earn rate.
- `get_delta_neutral_history` — Fetch execution history, monitor snapshots, and alerts for a plan.
- `render_delta_neutral_yield_chart` — Render and send the Yield History chart (funding rate, funding APY%, earn APY%, combined APY%) as a PNG image to the chat. Selectable time period (7d/14d/30d/3m/6m/all) and optional column filter.
- `open_delta_neutral_position` — Execute a two-leg opening (spot long + futures short) with full approval workflow and state machine. Checks live entry spread against `entry_rules.min_entry_spread_pct` before placing orders; blocks if spread is below the configured minimum.
- `unwind_delta_neutral_position` — Execute a two-leg closing (sell spot + close futures short) with recovery path.
- `resize_delta_neutral_position` — Adjust the notional size of an active plan by resizing both legs equally. Accepts `delta_pct` or `delta_notional_usdt` (exactly one required); active plans only. Dry-run with `confirm=false`; execute with `confirm=true`. Partial fill transitions plan to `recovery_required`.

## Worked Example: Find The Best ETH Opportunity

```
User: "Find me the best ETH delta-neutral opportunity on Binance and OKX."

Agent:
1. Calls futures_validate_market for ETH on both exchanges → both active.
2. Calls futures_get_funding for current rates:
   Binance: 0.0085% / 8h
   OKX:     0.0082% / 8h
3. Calls funding_rate_history (limit=200) for both:
   Binance 3d/7d/14d: 0.0076% / 0.0082% / 0.0078% | volatility: ±0.0015%
   OKX     3d/7d/14d: 0.0075% / 0.0079% / 0.0076% | volatility: ±0.0018%
4. Calls get_ticker (spot + perp) for current prices:
   Binance spot: 1,920 USDT | Binance perp: 1,922 USDT (3 USDT spread)
   OKX spot:     1,921 USDT | OKX perp:     1,920 USDT (1 USDT spread)
5. Calls get_orderbook for both spot and perp on both exchanges to estimate slippage.
6. Ranks:
   - Binance (both legs): 26.5% annualized, tight spreads, 92% positive ratio → ATTRACTIVE
   - OKX (both legs):     25.5% annualized, tighter spreads, 88% positive ratio → ATTRACTIVE
   - Cross-exchange (Binance spot + OKX futures): 25.95% annualized → WATCH (cross-exchange warning)

Output summary with breakeven estimates for each scenario.

User: "Create a 10k USDT plan on Binance."

Agent:
1. Calls list_portfolios → shows available accounts.
2. User selects "Binance spot-trading" for spot leg, "Binance futures-trading" for futures leg.
3. Validates with futures_validate_market and get_assets_list.
4. Estimates:
   - Spot: Buy 5.2 ETH @ 1,920 USDT = 10,000 USDT
   - Futures: Short 5.2 ETH @ 1x leverage
   - Entry cost: ~60 USDT
   - Exit cost: ~60 USDT
   - Daily carry: ~2.04 USDT (after costs)
   - Breakeven: 59 days
5. Sets default risk policy (25% min liquidation, 3% max delta drift, 5 min monitor).
6. Shows full plan summary, asks for confirmation.
7. User confirms → Plan stored, cron monitor registered as "dn:1:ETH Funding Carry".
8. Suggests: "Plan created. When you're ready, I'll prepare it (`prepare_delta_neutral_plan`) and then open the position." A plan opens only when it is `ready`: a draft, or a failed plan being retried, must be prepared first.
```

## Sample Execution Review (Before Opening)

Once a plan is ready, the execution review shows:

```text
=== Execution Review: ETH Funding Carry (10k USDT) ===

Spot Leg
  Provider:      Binance
  Account:       spot-trading
  Symbol:        ETH/USDT
  Side:          Buy
  Amount:        5.2 ETH
  Order type:    Market
  Est. price:    1,920 USDT
  Est. total:    9,984 USDT
  Est. fee:      ~5 USDT (0.05% taker)
  Est. slippage: ~30 USDT (0.3%)

Futures Leg
  Provider:      Binance
  Account:       futures-trading
  Symbol:        ETH/USDT:USDT
  Side:          Short
  Amount:        5.2 ETH
  Contracts:     52 (10 ETH per contract)
  Leverage:      1x
  Margin mode:   Cross
  Order type:    Market
  Est. mark:     1,922 USDT
  Est. total:    10,034 USDT notional
  Est. fee:      ~5 USDT (0.05% taker)
  Est. slippage: ~30 USDT (0.3%)

Summary
  Spot value:           9,984 USDT
  Futures notional:     10,034 USDT
  Delta (futures / spot): 100.5% ← rebalance to <103% after fills
  Est. total entry cost: ~70 USDT
  Est. exit cost:       ~70 USDT
  Est. round-trip cost: ~140 USDT
  Daily carry (gross):  ~2.44 USDT
  Daily carry (net):    ~2.04 USDT
  Breakeven:            ~69 days
  Liquidation buffer:   ~50% (with 1x leverage)

Risk
  Min liquidation dist: 25%
  Max delta drift:      3%
  Max slippage bps:     20 (0.2%)

Exchange:               Binance (same for both legs) ✓

⚠️ IMPORTANT: You are about to place real orders. Spot and futures are not atomic; if 
the first leg fills and the second fails, you'll have unhedged exposure. Confirm you 
understand the risk.

Proceed? [Confirm/Cancel]
```

## Common Recipes

### Safe, Same-Exchange Carry (Lowest Risk)

1. Select spot and futures on the **same exchange** (Binance or OKX).
2. Choose conservative capital allocation (< 20% of total portfolio).
3. Set monitor interval to 5 minutes (default).
4. Liquidation buffer must be ≥ 25%.
5. Delta drift max ≤ 3%.
6. Only open when funding is above 0.01% and positive trend is stable (3d/7d/14d all positive).

### Multi-Exchange Carry (Higher Risk, Higher Reward)

1. Spot leg on Binance, futures on OKX (or vice versa).
2. Use 1x leverage only (no amplified margin).
3. Monitor interval: 1–3 minutes (requires rate-limit warning acknowledgment).
4. Keep liquidation buffer ≥ 30% (extra buffer for cross-exchange risk).
5. Confirm the cross-exchange warning before opening.
6. Have a manual close plan ready if exchanges diverge unexpectedly.

### Scalping High Volatility Funding

1. Wait for funding to spike (e.g., > 0.015% per 8h).
2. Open position with short holding period (target: 3–5 days).
3. Close early if:
   - Funding reverses (negative for 1 cycle).
   - Liquidation buffer drops below 25%.
   - Delta drift exceeds 5%.
4. Do not extend beyond breakeven + 30 days unless funding remains exceptional.

## Managing Active Plans

### Pause a Plan

If monitoring becomes unreliable or you want to stop capturing funding temporarily:

```
"Pause the ETH Funding Carry plan."

Agent: Calls update_delta_neutral_plan(plan_id, enabled=false).
Plan status: active → paused
Monitor job: disabled (no cron tick)
Position: stays open; no unwind.

User can resume later.
```

### Rebalance Delta If It Drifts

During a monitor tick, if delta drift exceeds your max threshold:

```
Plan alert: "ETH plan delta drift 5.2% (max 3%). Recommend rebalancing."

Agent: Suggests either:
1. Buy more spot (if futures is over-hedged).
2. Close some futures (if spot is under-hedged).

User reviews and approves exact order before it executes.
```

### Setting Leverage at Create

When creating a new delta-neutral plan via `create_delta_neutral_plan`, you specify:

- **`leverage`** (optional, default 1): The futures leverage multiplier for the position.
- **`risk_policy.max_leverage`** (optional): The maximum leverage allowed by your risk policy.

The plan sizes both legs to **equal notional**:

```text
Formula: N = (capital - reserve) × leverage / (leverage + 1)

Example with capital=10,000 USDT, reserve=500 USDT, leverage=2:
  N = (10,000 - 500) × 2 / (2 + 1) = 9,500 × 0.667 = 6,333.33 USDT
  
  Spot notional:     6,333.33 USDT
  Futures notional:  6,333.33 USDT (at 2x leverage)
  Total margin use:  3,166.67 USDT (half the notional on futures)
```

The tool **rejects** `leverage > max_leverage` at creation time. This ensures you cannot exceed your configured safety limit.

Example call:

```json
{
  "plan_name": "BTC High-Leverage Carry",
  "asset": "BTC",
  "spot_provider": "binance",
  "spot_symbol": "BTC/USDT",
  "futures_provider": "binance",
  "futures_symbol": "BTC/USDT:USDT",
  "capital_usdt": 10000,
  "leverage": 2,
  "futures_margin_mode": "isolated",
  "risk_policy": {
    "max_leverage": 3,
    "min_liquidation_distance_pct": 25
  }
}
```

### Futures Margin Mode (cross vs isolated)

The `futures_margin_mode` parameter controls how margin is allocated on the futures leg:

| Mode | Behavior | When to use |
|------|----------|-------------|
| `cross` (default) | Shares the entire futures wallet as margin. A liquidation event on this position draws from all funds in the futures account, putting other open positions at risk. | Safe only at 1–2× leverage when no other positions are open. |
| `isolated` | Caps exposure to the margin posted on this position only. Liquidation does **not** affect other futures positions or wallet balance. | **Strongly recommended at leverage > 2×**, or whenever other positions are open. |

**Rule of thumb**: if the user says "use 5× leverage" or asks to protect other positions, always use `isolated`.

Set at plan creation:
```json
{
  "plan_name": "ALGO Funding Harvest",
  "futures_margin_mode": "isolated",
  "leverage": 5,
  ...
}
```

Change before opening (draft/ready plans only):
```json
{
  "plan_id": 42,
  "futures_margin_mode": "isolated"
}
```

**Cannot be changed on an active position** — you must unwind and reopen to change margin mode on the exchange.

`prepare_delta_neutral_plan` will emit a warning if `cross` margin is detected at leverage > 2×.

---

### Editing Leverage on Existing Plans

Use `update_delta_neutral_plan` with the `leverage` parameter to adjust the leverage of an existing plan.

**For draft or ready plans**: The new leverage is stored and applied when the plan is next opened. No live exchange action occurs.

**For active plans**: Leverage change applies **immediately** on the exchange:

1. **Requires `confirm=true`**: You must explicitly approve a live leverage change.
2. **Applied via `SetFuturesLeverage`**: The exchange's leverage setting is updated in real-time.
3. **Re-validated against liquidation distance**: After the lever change, the tool computes the new liquidation distance and **rejects** the change if it would breach `risk_policy.min_liquidation_distance_pct`.
4. **Does not change delta**: Leverage adjusts **margin requirement and liquidation risk**, NOT the matched notional. Delta remains the same (equal spot and futures notional).

Example calls:

```json
{
  "plan_id": 42,
  "leverage": 3,
  "confirm": true
}
```

If the leverage change would drop liquidation distance below your policy minimum, you'll see:

```
Error: leverage 3 would drop liquidation distance to 18%, below policy minimum 25% — reverting
```

**Note**: A leverage *change* on an active position **does not rebalance the legs**. Both legs retain their current notional. What changes is the margin requirement and liquidation price. If you want to resize the notional, use `resize_delta_neutral_position` instead.

### Resizing a Position

Use `resize_delta_neutral_position` to adjust the notional size of an active delta-neutral plan. This maintains equal notional on both legs (spot and futures).

**Parameters:**

- `plan_id` (required): ID of the active plan.
- Exactly one of:
  - `delta_pct`: Percentage change (e.g., `-10` to remove 10%, `+20` to add 20%).
  - `delta_notional_usdt`: Absolute USDT change (e.g., `-500` to reduce by $500, `+1000` to add $1000).
- `confirm` (optional, default false): Set `confirm=true` to execute. Without it, dry-run shows the proposed resize.

**Behavior:**

- **Active plans only**: Draft or ready plans cannot be resized; they have no open position.
- **Equal notional on both legs**: If you resize by −10%, both spot and futures notional decrease by 10%. If you resize by +20%, both increase by 20%.
- **Decrease path**: Uses reduce-only close for futures (close short position) and sell for spot (sell holdings).
- **Increase path**: Adds to both futures (using the plan's leverage setting) and spot (buy more).
- **Dry-run (confirm=false)**: Shows a resize review with the proposed notionals and direction, but does not place orders.
- **Live (confirm=true)**: Places orders on both legs. If one leg fails, the plan enters `recovery_required` status with a CRITICAL alert. You must run `unwind_delta_neutral_position` or manually rebalance to recover.

Example call to dry-run a 10% reduction:

```json
{
  "plan_id": 42,
  "delta_pct": -10,
  "confirm": false
}
```

Response (dry-run):

```
Resize review (DRY-RUN):
  Plan:                    ETH Funding Carry (ID 42)
  Status:                  active

Current Position:
  Spot Notional (USDT):    5000.00
  Futures Notional (USDT): 5000.00

Resize Parameters:
  Delta (USDT):            -500.00
  Direction:               decrease

New Position (both legs equal):
  Spot Notional (USDT):    4500.00
  Futures Notional (USDT): 4500.00

Set confirm=true to execute.
```

To execute:

```json
{
  "plan_id": 42,
  "delta_pct": -10,
  "confirm": true
}
```

**Validation:**

- Both `delta_pct` and `delta_notional_usdt` cannot be provided at the same time.
- At least one must be provided; neither is an error.
- Cannot decrease by more than the current notional.

**Failure modes:**

- **First leg fails**: Plan transitions to `failed` and second leg is not placed. Position unchanged.
- **Second leg fails**: Plan transitions to `recovery_required` with a CRITICAL alert. First leg has executed; you have partial, unhedged exposure. **Immediately run `unwind_delta_neutral_position` to close or rebalance.**

### Close a Plan Early

```
"Close the ETH Funding Carry plan."

Agent:
1. Shows current plan state: PnL, funding received, unrealized hedge PnL.
2. Estimates exit costs and final net PnL.
3. Displays execution review for both legs.
4. Requires confirmation.
5. Closes futures (reduce-only market order).
6. Sells spot holdings.
7. Records final PnL and closes plan.
```

## Troubleshooting

### "Funding rate is unavailable."

Possible causes:
- Exchange API is rate-limited.
- Exchange is in maintenance window.
- The symbol is not recognized on that exchange.

Remedy: Check the exchange status page. Retry in 1–2 minutes. If the symbol is new, call `futures_validate_market` again to confirm it is active.

### "Liquidation distance dropped below 25%."

Alert has fired. Review your:
- Current leverage (reduce if > 1.5x).
- Unrealized loss (if marked against you, close position early).
- Margin balance (deposit more capital if below 30% threshold).

Remedy: Close position early or increase reserve margin in your portfolio.

### "Delta drift exceeded 3%."

Spot and futures notional are out of sync. This happens if:
- One leg filled and the other didn't (execution issue).
- Price moved significantly between legs.
- You manually added to one leg without rebalancing the other.

Remedy: Rebalance by buying/selling spot or adjusting futures position size.

### "Exchange API returned an error."

Monitor snapshot has `data_status=error`. No monitor tick will be silent; an alert is sent immediately.

Remedy: Check your API key permissions, rate limits, and account status. If the error persists, pause the plan until the exchange recovers.

### "Plan entered recovery_required state."

One leg filled, the other failed. You have partial exposure.

Remedy:
- Do **not** manually close the open leg; wait for agent guidance.
- Review the execution attempt details in the plan history.
- Follow the recovery action suggested by the system.
- Once recovery is complete, you can manually close if needed.

## See Also

- **funding-rate-analysis**: Analyze funding history and sentiment trends across exchanges.
- **trading**: Place, monitor, and cancel orders; understand order lifecycle and confirmation rules.
- **dca**: Set up recurring investments with trigger-based execution; useful pattern for rebalancing delta drift.
