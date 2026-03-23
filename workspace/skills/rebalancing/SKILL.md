---
name: rebalancing
description: Rebalance a portfolio to target allocation weights. Computes required trades from current allocation and executes them with user confirmation.
---

# Portfolio Rebalancing

Systematically rebalance a portfolio to target weights using the allocation and trading tools.

## Rebalancing Workflow

```
portfolio_allocation → compute target trades → confirm with user → execute via create_order
```

### Step 1: Assess Current Allocation
```
portfolio_allocation [quote=USDT]
```
Review per-asset weights and concentration warnings (>30%).

### Step 2: Define Target Allocation
Ask the user what target weights they want, e.g.:
- BTC: 50%, ETH: 30%, USDT: 20%

### Step 3: Compute Required Trades
For each asset:
```
trade_value = (target_weight - current_weight) × total_portfolio_value
trade_amount = trade_value / current_price (from get_ticker)
```
Positive = buy; negative = sell.

### Step 4: Review and Confirm with User
Present the trade plan:
- List all proposed trades with symbols, sides, amounts, and estimated costs
- Flag any trade exceeding 10% of portfolio value
- Ask for explicit confirmation before executing

### Step 5: Execute Trades
For each confirmed trade, call `create_order`:
- Use limit orders at mid-price (bid/ask from `get_orderbook`) for large trades
- Use market orders for small trades (<$500 notional)
- Verify each fill with `get_order` before proceeding to the next

### Step 6: Verify Final Allocation
Call `portfolio_allocation` again to confirm the rebalance completed as intended.

## Safety Rules

1. **Never exceed 5 orders per minute** (rate limit — check `get_order_rate_status`).
2. **Always confirm with the user** before executing any real order.
3. **Start with paper_trade** for each planned trade to preview the notional value.
4. **Preserve at least 5% USDT/stablecoin** as a buffer — never rebalance to 100% risk assets.
5. **Stop immediately** if any order fails — call `get_open_orders` to check for dangling orders.

## Notes
- Use `transfer_funds` if assets need to be moved between spot and futures wallets first.
- Consider transaction fees when computing trade amounts (typically 0.1% per trade).
- For large portfolios, break rebalancing into multiple sessions across different days.
