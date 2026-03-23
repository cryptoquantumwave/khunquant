---
name: trading
description: Place, monitor, and cancel orders on configured exchanges. Includes safety-first order lifecycle, paper trading simulation, and emergency stop.
---

# Trading

Follow the safety-first order lifecycle whenever executing trades. Never skip validation steps.

## Safety-First Order Lifecycle

```
validate → confirm with user → execute → verify fill
```

1. **Validate first**: Always check balance with `get_assets_list` before placing a buy order.
2. **Check rate limit**: Use `get_order_rate_status` to confirm tokens are available.
3. **Paper trade**: Use `paper_trade` to simulate the order and check notional value.
4. **Confirm with user**: Report the planned trade and ask for explicit confirmation before setting `confirm=true`.
5. **Execute**: Call `create_order` with `confirm=true`.
6. **Verify**: Call `get_order` to confirm the fill status.

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
- `symbol`: optional filter

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
- Binance and OKX support full order execution. Bitkub and BinanceTH do not.
- The default rate limit is 5 orders per minute per provider.
- Never place orders over $10,000 notional without explicit user confirmation.
