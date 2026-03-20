---
name: snapshots
description: Capture, query, and analyze portfolio snapshots over time for growth tracking and anomaly detection.
---

# Portfolio Snapshots

Use these tools to periodically capture portfolio state and analyze growth over time.

## Workflow

1. Call `list_portfolios` to discover available exchange accounts.
2. Call `take_snapshot` to capture current balances and values.
3. Call `query_snapshots` to browse historical snapshots.
4. Call `snapshot_summary` to compute growth analytics (change, change%, min/max/avg).

## Tools

### take_snapshot
Capture current portfolio state from exchanges. Stores all positions with live prices. Automatically handles exchanges with different wallet type support — exchanges that support "all" use a single call, while others (e.g. bitkub, binanceth) have each wallet type queried individually and merged.
- `source`: exchange name (omit for all configured sources)
- `account`: account name (omit for all)
- `quote`: quote currency (default: "USDT")
- `label`: tag like "daily", "heartbeat", "pre-rebalance"
- `note`: optional free-text

### query_snapshots
Browse historical snapshots with filters.
- `since` / `until`: time range (ISO 8601 or relative: "24h", "7d", "30d")
- `label`, `source`, `asset`: filters
- `limit`: max results (default 10)
- `include_positions`: include per-asset details (default false)

### snapshot_summary
Compute aggregate analytics over snapshots.
- `since` / `until`: time range
- `label`, `source`: filters
- `group_by`: "day", "week", "month", or "source"

### delete_snapshots
Remove old snapshots. Always keeps N most recent as safety.
- `id`: delete specific snapshot
- `before`: delete all before date
- `label`: delete by label
- `keep_last`: safety floor (default 5)
- `confirm`: required true for bulk deletes

## Heartbeat Pattern

To automatically take snapshots on a schedule, add a task to your HEARTBEAT.md:

```markdown
## Portfolio Snapshot
Take a portfolio snapshot for tracking.
- Call `take_snapshot` with label "heartbeat"
- If total value changed more than 5% from last snapshot, note the change
```

## Analysis Patterns

- **Daily growth:** `snapshot_summary` with `since=7d` and `group_by=day`
- **Cross-source comparison:** `snapshot_summary` with `group_by=source`
- **Anomaly detection:** Compare `query_snapshots since=1h` against `snapshot_summary since=30d` avg
- **Pre/post rebalance:** Take snapshot with label "pre-rebalance", rebalance, take "post-rebalance"
