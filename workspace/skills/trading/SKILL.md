---
name: trading
description: Place, monitor, and cancel orders on configured exchanges. Includes safety-first order lifecycle, paper trading simulation, and emergency stop.
---

# Trading

Follow the order lifecycle below whenever executing trades.

## Order Lifecycle

```
validate → [confirm if large] → execute → verify fill
```

1. **Validate first**: Always check balance with `get_assets_list` before placing a buy order.
2. **Check rate limit**: Use `get_order_rate_status` to confirm tokens are available.
3. **Confirmation threshold**:
   - Notional value **< $200 (or equivalent)**: Do NOT ask for confirmation. Summarize the order details (symbol, side, amount, price, estimated notional) and immediately execute with `confirm=true`.
   - Notional value **≥ $200**: Ask the user for explicit confirmation before executing.
4. **Execute**: Call `create_order` with `confirm=true`.
5. **Verify**: Call `get_order` to confirm the fill status.

## Paper Trading Rules

- **If the `paper_trade` tool is NOT available/enabled**: Assume the user always wants real execution. Never suggest simulating or paper trading — just proceed with real orders.
- **If the `paper_trade` tool IS available/enabled**: Only offer to simulate when the user's message contains words like "simulate", "test", "paper", "pretend", "practice", or "what if". Otherwise default to real execution.

## Tools

### create_order
Place a new order. Runs 7 internal safety gates before executing.
- `provider`, `account`, `symbol`: as per market-data skill
- `type`: "limit" | "market" | "stop_loss" | "take_profit"
- `side`: "buy" | "sell"
- `amount`: quantity in base currency
- `price`: limit price (required for limit orders)
- `confirm`: set true to execute; false for dry-run

### cancel_order
Cancel an open order.
- `provider`, `account`, `symbol`: as above
- `order_id`: the order ID to cancel

### get_order
Retrieve a single order by ID.
- `provider`, `account`, `symbol`, `order_id`

### get_open_orders
List all currently open orders.
- `provider`, `account`: as above
- `symbol`: optional filter — **required for Bitkub** (Bitkub API does not support fetching all open orders without a specific trading pair)

### get_order_history
Retrieve closed/filled order history.
- `provider`, `account`, `symbol`: optional
- `since`: Unix milliseconds start time
- `limit`: max 200

### get_trade_history
Retrieve personal trade execution history.
- `provider`, `account`, `symbol`: optional
- `since`, `limit`: as above

### paper_trade
Simulate an order using live market price. Does NOT place a real order.
- `provider`, `account`, `symbol`, `type`, `side`, `amount`, `price`

### get_order_rate_status
Show current rate-limit token counts per provider. No parameters.

### emergency_stop
Cancel ALL open orders across all configured providers. Irreversible.
- `confirm`: must be true to execute

## Notes
- **Bitkub**: `get_open_orders` and `get_order_history` require a `symbol` parameter (e.g. `BTC/THB`). Calling without a symbol will fail — always ask the user which trading pair to check, or iterate over known pairs.
- **Settrade (SET equity)**: supports limit and market (ATO) orders. Amount is in shares. PIN is required in config.
- The default rate limit is 5 orders per minute per provider.
- Always confirm with the user before placing orders with notional ≥ $200. Never place orders over $3,000 without explicit confirmation regardless of context.

## Settrade Order Notes
- `symbol`: use SET ticker format e.g. "PTT/THB" or just "PTT"
- `type`: "limit" (Limit order) or "market" (ATO — At The Open)
- `amount`: number of shares
- `price`: required for limit orders, ignored for market/ATO
- Settrade orders require `pin` to be set in config — it is sent automatically
- Price is automatically rounded to the SET tick size grid (e.g. ฿0.01 below ฿2, ฿1.00 above ฿100)
- To modify a pending order (change price or volume), use `cancel_order` then place a new one — Settrade's change_order is handled internally
