import { IconAlertTriangle, IconLoader2, IconTrash } from "@tabler/icons-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"

import {
  type DeltaNeutralPlanListItem,
  type DeltaNeutralMonitorSnapshot,
  type DeltaNeutralAlert,
  type DeltaNeutralExecution,
  deleteDeltaNeutralPlan,
  getDeltaNeutralSnapshots,
  getDeltaNeutralAlerts,
  getDeltaNeutralExecutions,
  listDeltaNeutralPlans,
} from "@/api/agent-delta-neutral"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { DeltaNeutralPnLCard, DeltaNeutralYieldCard, DeltaNeutralSpreadCard } from "./delta-neutral-position-cards"
import { DeltaNeutralYieldChart } from "./delta-neutral-yield-chart"
import { InfoHint, LabelWithHint } from "./info-hint"

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString()
  } catch {
    return iso
  }
}

function formatNum(n: number, digits = 4): string {
  return n.toLocaleString(undefined, { maximumFractionDigits: digits, minimumFractionDigits: 2 })
}

// The API says whether the margin ratio is a reading. A response from before the flag has
// none: a non-zero value is a reading, an exact 0 is not (the old writer stored 0 for both).
function marginRatioKnown(snap: DeltaNeutralMonitorSnapshot): boolean {
  return snap.margin_ratio_known ?? snap.margin_ratio_pct !== 0
}

function healthLabelColor(label: string): string {
  if (!label) return "bg-gray-100 text-gray-700 dark:bg-gray-900/30 dark:text-gray-400"
  const lower = label.toLowerCase()
  if (lower.includes("excellent") || lower.includes("healthy")) {
    return "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
  }
  if (lower.includes("watch") || lower.includes("warning")) {
    return "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400"
  }
  if (lower.includes("danger") || lower.includes("critical")) {
    return "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"
  }
  return "bg-gray-100 text-gray-700 dark:bg-gray-900/30 dark:text-gray-400"
}

function healthBarColor(label: string): string {
  if (!label) return "bg-gray-400"
  const lower = label.toLowerCase()
  if (lower.includes("excellent") || lower.includes("healthy")) return "bg-green-500"
  if (lower.includes("watch") || lower.includes("warning")) return "bg-amber-500"
  if (lower.includes("danger") || lower.includes("critical")) return "bg-red-500"
  return "bg-gray-400"
}

function healthScoreTextColor(label: string): string {
  if (!label) return "text-gray-500"
  const lower = label.toLowerCase()
  if (lower.includes("excellent") || lower.includes("healthy")) return "text-green-600 dark:text-green-400"
  if (lower.includes("watch") || lower.includes("warning")) return "text-amber-600 dark:text-amber-400"
  if (lower.includes("danger") || lower.includes("critical")) return "text-red-600 dark:text-red-400"
  return "text-gray-500"
}

function SectionHeader({ title, count, hint }: { title: string; count?: number; hint?: string }) {
  return (
    <div className="border-border/50 flex items-center border-b px-3 py-2">
      <span className="text-foreground/80 text-xs font-medium uppercase tracking-wide">{title}</span>
      {hint && (
        <span className="ml-1.5">
          <InfoHint text={hint} />
        </span>
      )}
      {count !== undefined && (
        <span className="ml-2 rounded-full bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
          {count}
        </span>
      )}
    </div>
  )
}

// ─── stat cell used in plan summary ─────────────────────────────────────────

function StatCell({
  label,
  hint,
  value,
}: {
  label: string
  hint: string
  value: string
}) {
  return (
    <div className="rounded-lg border p-3 min-w-0">
      <div className="text-muted-foreground mb-1 flex items-center gap-1 text-xs">
        <LabelWithHint label={label} hint={hint} className="text-muted-foreground text-xs" />
      </div>
      <div className="font-mono text-sm font-medium truncate" title={value}>
        {value}
      </div>
    </div>
  )
}

// ─── plan summary ────────────────────────────────────────────────────────────

function PlanSummary({ plan }: { plan: DeltaNeutralPlanListItem }) {
  return (
    <div className="flex flex-col gap-3">
      {/* Plan header with inline health score */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-foreground font-semibold">{plan.name}</span>
        <span className="text-muted-foreground text-xs">{plan.asset}</span>
        <span
          className={`rounded-full px-2 py-0.5 text-xs font-medium ${
            plan.enabled
              ? "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
              : "bg-muted text-muted-foreground"
          }`}
        >
          {plan.enabled ? "Active" : "Paused"}
        </span>
        {plan.cross_exchange && (
          <span className="rounded-full px-2 py-0.5 text-xs font-medium bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400">
            Cross-Exchange
          </span>
        )}
        {/* Health indicator */}
        <span className={`font-mono text-sm font-semibold ${healthScoreTextColor(plan.health_label)}`}>
          {plan.health_score}
          <span className="text-muted-foreground font-normal">/100</span>
        </span>
        <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${healthLabelColor(plan.health_label)}`}>
          {plan.health_label}
        </span>
        <InfoHint text="Health score (0–100) evaluates delta drift, margin safety, liquidation distance, and funding rate status each monitor tick. Labels: excellent ≥85, healthy ≥65, watch ≥45, warning ≥25, danger <25." />
      </div>

      {/* Provider routing */}
      <div className="text-muted-foreground text-xs">
        {plan.spot_provider} ({plan.spot_account}) ↔ {plan.futures_provider} ({plan.futures_account})
      </div>

      {/* Health progress bar */}
      <div className="h-1 w-full overflow-hidden rounded-full bg-muted">
        <div
          className={`h-full rounded-full transition-all ${healthBarColor(plan.health_label)}`}
          style={{ width: `${Math.min(100, Math.max(0, plan.health_score))}%` }}
        />
      </div>

      {/* Stats grid — wraps on narrow widths */}
      <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
        <StatCell
          label="Capital"
          hint="Target USDT capital allocated to this strategy. Split roughly 50/50 between spot buy and futures margin."
          value={`${formatNum(plan.capital_usdt, 2)} USDT`}
        />
        <StatCell
          label="Spot Symbol"
          hint="The spot trading pair used for the long leg (e.g. ALGO/USDT). Bought at open; sold on unwind."
          value={plan.spot_symbol}
        />
        <StatCell
          label="Futures Symbol"
          hint="The perpetual futures contract used for the short hedge (e.g. ALGO/USDT:USDT). Provides funding income when longs pay shorts."
          value={plan.futures_symbol}
        />
        <StatCell
          label="Monitor Interval"
          hint="How often the agent checks positions, funding, and risk metrics. Shorter intervals react faster but consume more compute."
          value={plan.monitor_interval}
        />
      </div>

      {/* Spread targets */}
      <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
        <StatCell
          label="Entry Spread Gate"
          hint="Minimum spread to trigger rebalancing from entry. Edit in the Spread card below."
          value={plan.min_entry_spread_pct !== 0 ? `${plan.min_entry_spread_pct.toFixed(4)}%` : "disabled"}
        />
        <StatCell
          label="Exit Spread Target"
          hint="Target spread to trigger exit rebalancing. Edit in the Spread card below."
          value={plan.target_exit_spread_pct !== 0 ? `${plan.target_exit_spread_pct.toFixed(4)}%` : "disabled"}
        />
      </div>

      {/* Accumulated fees */}
      {plan.fee_snapshot && (
        <div className="overflow-hidden rounded-lg border">
          <div className="border-border/50 flex items-center justify-between border-b px-3 py-2">
            <div className="flex items-center gap-1.5">
              <span className="text-foreground/80 text-xs font-medium uppercase tracking-wide">
                Accumulated Fees
              </span>
              <InfoHint text="Running totals of all fees since the strategy was opened. Trading fees are always negative (cost). Funding fees are positive when shorts receive payment from longs (typical in a bullish market)." />
            </div>
            <span className="text-muted-foreground text-xs">
              {formatDate(plan.fee_snapshot.fetched_at)}
            </span>
          </div>
          <div className="grid grid-cols-2 gap-0 divide-x divide-border/40">
            <div className="p-3">
              <div className="text-muted-foreground mb-1 flex items-center gap-1 text-xs">
                <LabelWithHint
                  label="Trading Fee"
                  hint="Sum of taker fees paid across all filled legs (open, resize, and close executions). Always negative — a cost. Stored in fee_usdt per leg."
                  className="text-muted-foreground text-xs"
                />
              </div>
              <div
                className={`font-mono text-sm font-semibold ${
                  plan.fee_snapshot.trading_fee_usdt < 0
                    ? "text-red-500 dark:text-red-400"
                    : "text-green-600 dark:text-green-400"
                }`}
              >
                {plan.fee_snapshot.trading_fee_usdt >= 0 ? "+" : ""}
                {formatNum(plan.fee_snapshot.trading_fee_usdt, 4)} USDT
              </div>
            </div>
            <div className="p-3">
              <div className="text-muted-foreground mb-1 flex items-center gap-1 text-xs">
                <LabelWithHint
                  label="Funding Fee"
                  hint="Accumulated funding payments. Positive means the strategy is net receiving (shorts paid by longs). Negative means the strategy is paying (rare in a bullish market)."
                  className="text-muted-foreground text-xs"
                />
              </div>
              <div
                className={`font-mono text-sm font-semibold ${
                  plan.fee_snapshot.funding_fee_usdt < 0
                    ? "text-red-500 dark:text-red-400"
                    : "text-green-600 dark:text-green-400"
                }`}
              >
                {plan.fee_snapshot.funding_fee_usdt >= 0 ? "+" : ""}
                {formatNum(plan.fee_snapshot.funding_fee_usdt, 4)} USDT
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

// ─── Snapshot table ──────────────────────────────────────────────────────────

function SnapshotTable({
  planId,
  snapshots,
}: {
  planId: number
  snapshots?: DeltaNeutralMonitorSnapshot[]
}) {
  const isLoading = snapshots === undefined
  void planId // kept for future use (e.g. load-more)

  if (isLoading) {
    return (
      <div className="flex h-24 items-center justify-center">
        <IconLoader2 className="text-muted-foreground size-4 animate-spin" />
      </div>
    )
  }

  if (!snapshots || snapshots.length === 0) {
    return (
      <div className="overflow-hidden rounded-lg border">
        <SectionHeader title="Monitor Snapshots" count={0} hint="Periodic health checks captured by the agent. Each row shows the position state at one point in time." />
        <div className="text-muted-foreground py-4 text-center text-sm">No snapshots yet.</div>
      </div>
    )
  }

  return (
    <div className="overflow-hidden rounded-lg border">
      <SectionHeader
        title="Monitor Snapshots"
        count={snapshots.length}
        hint="Periodic health checks captured by the agent. Each row shows the position state at one point in time."
      />
      <div className="max-h-96 overflow-y-auto overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="sticky top-0 z-10">
            <tr className="bg-muted/60 text-muted-foreground border-b text-xs">
              <th className="px-3 py-2 text-left font-medium">Checked At</th>
              <th className="px-3 py-2 text-right font-medium">
                <span className="inline-flex items-center justify-end gap-1">
                  Delta Drift
                  <InfoHint text="% imbalance between spot quantity and futures notional. Near zero is ideal. A large positive drift means spot has grown relative to futures (price rose); negative means the opposite." />
                </span>
              </th>
              <th className="px-3 py-2 text-right font-medium">
                <span className="inline-flex items-center justify-end gap-1">
                  Spread%
                  <InfoHint text="Current entry spread: difference between spot price and futures mark price as a percentage. Used to trigger rebalancing." />
                </span>
              </th>
              <th className="px-3 py-2 text-right font-medium">
                <span className="inline-flex items-center justify-end gap-1">
                  Funding Rate
                  <InfoHint text="Current perpetual funding rate at the time of this snapshot. Positive = longs pay shorts (we receive). Negative = shorts pay longs (we pay)." />
                </span>
              </th>
              <th className="px-3 py-2 text-right font-medium">
                <span className="inline-flex items-center justify-end gap-1">
                  Liq Dist
                  <InfoHint text="Liquidation distance: how far the mark price must move (as a % of current price) before the futures position is liquidated. Higher is safer." />
                </span>
              </th>
              <th className="px-3 py-2 text-right font-medium">
                <span className="inline-flex items-center justify-end gap-1">
                  Margin
                  <InfoHint text="Margin ratio: used margin ÷ total equity. Lower is safer. The exchange may issue a margin call when this rises above a threshold." />
                </span>
              </th>
              <th className="px-3 py-2 text-center font-medium">
                <span className="inline-flex items-center justify-center gap-1">
                  Health
                  <InfoHint text="Composite health score (0–100) at this snapshot. Factors: delta drift, liquidation distance, margin ratio, funding state." />
                </span>
              </th>
              <th className="px-3 py-2 text-center font-medium">
                <span className="inline-flex items-center justify-center gap-1">
                  Status
                  <InfoHint text="'ok' = data fetched successfully. Other values indicate a partial fetch or exchange error. 'agent invoked' means the agent was triggered to rebalance." />
                </span>
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border/20">
            {snapshots.map((snap: DeltaNeutralMonitorSnapshot) => (
              <tr key={snap.id} className="hover:bg-muted/20 transition-colors">
                <td className="text-muted-foreground px-3 py-2 font-mono text-xs whitespace-nowrap">
                  {formatDate(snap.checked_at)}
                </td>
                <td className="px-3 py-2 text-right font-mono text-xs">
                  {formatNum(snap.delta_drift_pct, 2)}%
                </td>
                <td className="px-3 py-2 text-right font-mono text-xs">
                  {snap.entry_spread_pct ? `${formatNum(snap.entry_spread_pct, 4)}%` : "—"}
                </td>
                <td className="px-3 py-2 text-right font-mono text-xs">
                  {formatNum(snap.current_funding_rate, 4)}
                </td>
                <td className="px-3 py-2 text-right font-mono text-xs">
                  {formatNum(snap.liquidation_distance_pct, 2)}%
                </td>
                <td className="px-3 py-2 text-right font-mono text-xs">
                  {marginRatioKnown(snap) ? `${formatNum(snap.margin_ratio_pct, 2)}%` : "unknown"}
                </td>
                <td className="px-3 py-2 text-center">
                  <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${healthLabelColor(snap.health_label)}`}>
                    {snap.health_score}
                  </span>
                </td>
                <td className="px-3 py-2 text-center">
                  <div className="flex flex-col items-center gap-1">
                    <span
                      className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                        snap.data_status === "ok"
                          ? "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
                          : "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"
                      }`}
                    >
                      {snap.data_status}
                    </span>
                    {snap.agent_invoked && (
                      <span className="text-muted-foreground text-xs">agent invoked</span>
                    )}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// ─── Alert table ─────────────────────────────────────────────────────────────

function AlertTable({ planId }: { planId: number }) {
  const { data: alerts, isLoading } = useQuery({
    queryKey: ["dn-alerts", planId],
    queryFn: () => getDeltaNeutralAlerts(planId, { limit: 50 }),
  })

  if (isLoading) {
    return (
      <div className="flex h-24 items-center justify-center">
        <IconLoader2 className="text-muted-foreground size-4 animate-spin" />
      </div>
    )
  }

  if (!alerts || alerts.length === 0) {
    return (
      <div className="overflow-hidden rounded-lg border">
        <SectionHeader title="Alerts" count={0} hint="Risk alerts triggered when a health threshold was breached. The agent may auto-invoke to rebalance on critical alerts." />
        <div className="text-muted-foreground py-4 text-center text-sm">No alerts yet.</div>
      </div>
    )
  }

  return (
    <div className="overflow-hidden rounded-lg border">
      <SectionHeader
        title="Alerts"
        count={alerts.length}
        hint="Risk alerts triggered when a health threshold was breached. The agent may auto-invoke to rebalance on critical alerts."
      />
      <div className="max-h-80 overflow-y-auto overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="sticky top-0 z-10">
            <tr className="bg-muted/60 text-muted-foreground border-b text-xs">
              <th className="px-3 py-2 text-left font-medium">Triggered At</th>
              <th className="px-3 py-2 text-center font-medium">
                <span className="inline-flex items-center justify-center gap-1">
                  Severity
                  <InfoHint text="info = informational, warning = monitor closely, critical = agent may auto-rebalance." />
                </span>
              </th>
              <th className="px-3 py-2 text-left font-medium">Code</th>
              <th className="px-3 py-2 text-left font-medium">Message</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border/20">
            {alerts.map((alert: DeltaNeutralAlert) => (
              <tr key={alert.id} className="hover:bg-muted/20 transition-colors">
                <td className="text-muted-foreground px-3 py-2 font-mono text-xs whitespace-nowrap">
                  {formatDate(alert.triggered_at)}
                </td>
                <td className="px-3 py-2 text-center">
                  <span
                    className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                      alert.severity === "critical"
                        ? "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"
                        : alert.severity === "warning"
                          ? "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400"
                          : "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"
                    }`}
                  >
                    {alert.severity}
                  </span>
                </td>
                <td className="text-muted-foreground px-3 py-2 font-mono text-xs whitespace-nowrap">
                  {alert.code}
                </td>
                <td className="px-3 py-2 text-xs">
                  <span className="block max-w-sm truncate" title={alert.message}>
                    {alert.message}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// ─── Execution table ──────────────────────────────────────────────────────────

function ExecutionTable({ planId }: { planId: number }) {
  const { data: execs, isLoading } = useQuery({
    queryKey: ["dn-executions", planId],
    queryFn: () => getDeltaNeutralExecutions(planId, { limit: 50 }),
  })

  if (isLoading) {
    return (
      <div className="flex h-24 items-center justify-center">
        <IconLoader2 className="text-muted-foreground size-4 animate-spin" />
      </div>
    )
  }

  if (!execs || execs.length === 0) {
    return (
      <div className="overflow-hidden rounded-lg border">
        <SectionHeader title="Execution History" count={0} hint="Records of open, resize, and close operations. Each execution has one or more legs (spot buy/sell, futures buy/sell)." />
        <div className="text-muted-foreground py-4 text-center text-sm">No executions yet.</div>
      </div>
    )
  }

  return (
    <div className="overflow-hidden rounded-lg border">
      <SectionHeader
        title="Execution History"
        count={execs.length}
        hint="Records of open, resize, and close operations. Each execution has one or more legs (spot buy/sell, futures buy/sell)."
      />
      <div className="max-h-80 overflow-y-auto overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="sticky top-0 z-10">
            <tr className="bg-muted/60 text-muted-foreground border-b text-xs">
              <th className="px-3 py-2 text-left font-medium">Attempt</th>
              <th className="px-3 py-2 text-left font-medium">Requested</th>
              <th className="px-3 py-2 text-center font-medium">
                <span className="inline-flex items-center justify-center gap-1">
                  State
                  <InfoHint text="completed = all legs filled. approved = agent approved but not yet executed. Other states indicate partial fills or failures." />
                </span>
              </th>
              <th className="px-3 py-2 text-left font-medium">Legs</th>
            </tr>
          </thead>
          <tbody>
            {execs.map((exec: DeltaNeutralExecution) => (
              <>
                <tr key={exec.id} className="border-border/30 border-b hover:bg-muted/20 transition-colors">
                  <td className="text-muted-foreground px-3 py-2 font-mono text-xs whitespace-nowrap">
                    {exec.attempt_id}
                  </td>
                  <td className="text-muted-foreground px-3 py-2 font-mono text-xs whitespace-nowrap">
                    {formatDate(exec.requested_at)}
                  </td>
                  <td className="px-3 py-2 text-center">
                    <span
                      className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                        exec.state === "completed"
                          ? "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
                          : exec.state === "approved"
                            ? "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"
                            : "bg-muted text-muted-foreground"
                      }`}
                    >
                      {exec.state}
                    </span>
                  </td>
                  <td className="px-3 py-2 text-xs text-muted-foreground">{exec.legs.length} leg(s)</td>
                </tr>
                {exec.legs.map((leg) => (
                  <tr key={`${exec.id}-leg-${leg.id}`} className="border-border/20 border-b last:border-0 bg-muted/20 hover:bg-muted/30 transition-colors">
                    <td className="px-6 py-1.5 text-xs font-medium">{leg.leg_type}</td>
                    <td className="px-3 py-1.5 text-xs whitespace-nowrap text-muted-foreground">
                      {leg.side} {leg.symbol} @ {leg.provider}
                    </td>
                    <td className="px-3 py-1.5 text-center">
                      <span
                        className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                          leg.state === "filled"
                            ? "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
                            : "bg-muted text-muted-foreground"
                        }`}
                      >
                        {leg.state}
                      </span>
                    </td>
                    <td className="px-3 py-1.5 font-mono text-xs whitespace-nowrap">
                      {formatNum(leg.filled_quantity, 6)} @ {formatNum(leg.avg_fill_price, 4)}
                      <span className="text-muted-foreground ml-2">
                        ≈ {formatNum(leg.filled_quantity * leg.avg_fill_price, 2)} USDT
                      </span>
                      {leg.fee_usdt !== 0 && (
                        <span className="text-muted-foreground ml-2">
                          fee{" "}
                          <span className={leg.fee_usdt < 0 ? "text-red-500 dark:text-red-400" : "text-green-500 dark:text-green-400"}>
                            {leg.fee_usdt >= 0 ? "+" : ""}{formatNum(leg.fee_usdt, 4)} USDT
                          </span>
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
                {exec.error_msg && (
                  <tr key={`${exec.id}-err`} className="border-border/20 border-b last:border-0">
                    <td colSpan={4} className="px-3 pb-2 text-xs text-red-500">
                      ↳ {exec.error_msg}
                    </td>
                  </tr>
                )}
              </>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// ─── Main panel ───────────────────────────────────────────────────────────────

export function DeltaNeutralPanel() {
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<DeltaNeutralPlanListItem | null>(null)
  const [deleteAnywayTarget, setDeleteAnywayTarget] = useState<DeltaNeutralPlanListItem | null>(null)
  const [unwindFailureMessage, setUnwindFailureMessage] = useState("")
  const queryClient = useQueryClient()

  const { data: plans, isLoading } = useQuery({
    queryKey: ["dn-plans"],
    queryFn: () => listDeltaNeutralPlans(),
  })

  // Hoist snapshot query so P&L/Yield cards and SnapshotTable share one fetch
  const { data: snapshots } = useQuery({
    queryKey: ["dn-snapshots", selectedId],
    queryFn: () => getDeltaNeutralSnapshots(selectedId!, { limit: 50 }),
    enabled: selectedId !== null,
  })

  const selectedPlan = plans?.find((p) => p.id === selectedId)
  const latestSnap = snapshots?.[0] ?? null
  const deleteRequiresUnwind =
    deleteTarget?.status === "active" || deleteTarget?.status === "recovery_required"

  const finishDelete = (plan: DeltaNeutralPlanListItem) => {
    setDeleteTarget(null)
    setDeleteAnywayTarget(null)
    setUnwindFailureMessage("")
    if (selectedId === plan.id) setSelectedId(null)
    void queryClient.invalidateQueries({ queryKey: ["dn-plans"] })
    void queryClient.invalidateQueries({ queryKey: ["dn-snapshots"] })
    void queryClient.invalidateQueries({ queryKey: ["dn-alerts"] })
    void queryClient.invalidateQueries({ queryKey: ["dn-executions"] })
    void queryClient.invalidateQueries({ queryKey: ["dn-series"] })
  }

  const deleteMutation = useMutation({
    mutationFn: (plan: DeltaNeutralPlanListItem) =>
      deleteDeltaNeutralPlan(plan.id, {
        force_unwind: plan.status === "active" || plan.status === "recovery_required",
      }),
    onSuccess: (_result, plan) => {
      toast.success("Delta-neutral plan deleted")
      finishDelete(plan)
    },
    onError: (err, plan) => {
      const message = err instanceof Error ? err.message : "Failed to delete delta-neutral plan"
      if (plan.status === "active" || plan.status === "recovery_required") {
        setDeleteTarget(null)
        setDeleteAnywayTarget(plan)
        setUnwindFailureMessage(message)
        return
      }
      toast.error(message)
    },
  })

  const deleteAnywayMutation = useMutation({
    mutationFn: (plan: DeltaNeutralPlanListItem) =>
      deleteDeltaNeutralPlan(plan.id, {
        delete_without_unwind: true,
      }),
    onSuccess: (_result, plan) => {
      toast.success("Plan record deleted without unwind")
      finishDelete(plan)
    },
    onError: (err) => {
      toast.error(err instanceof Error ? err.message : "Failed to delete delta-neutral plan")
    },
  })

  const itemClass = (id: number) =>
    `min-w-0 flex-1 rounded-md px-3 py-2 text-left text-sm transition-colors ${
      selectedId === id
        ? "bg-accent/80 text-foreground font-medium"
        : "text-muted-foreground hover:bg-muted/60"
    }`

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      {/* Left panel: plan list — fixed width, independently scrollable */}
      <div className="border-border/40 flex w-64 shrink-0 flex-col border-r">
        <div className="flex-1 overflow-auto p-2">
          {isLoading ? (
            <div className="text-muted-foreground p-2 text-sm">Loading…</div>
          ) : !plans || plans.length === 0 ? (
            <div className="text-muted-foreground p-2 text-sm">No delta-neutral plans yet.</div>
          ) : (
            <ul className="space-y-0.5">
              {plans.map((plan: DeltaNeutralPlanListItem) => (
                <li key={plan.id}>
                  <div
                    className={`group flex items-start gap-1 rounded-md ${
                      selectedId === plan.id ? "bg-accent/80" : "hover:bg-muted/60"
                    }`}
                  >
                    <button onClick={() => setSelectedId(plan.id)} className={itemClass(plan.id)}>
                      <div className="flex min-w-0 items-center gap-1.5">
                        <span
                          className={`size-1.5 shrink-0 rounded-full ${plan.enabled ? "bg-green-500" : "bg-muted-foreground"}`}
                        />
                        <span className="min-w-0 truncate font-medium">{plan.name}</span>
                      </div>
                      <div className="text-muted-foreground mt-0.5 font-mono text-xs">
                        {plan.asset} · {plan.spot_symbol}
                      </div>
                      <div className="text-muted-foreground text-xs">
                        {plan.spot_provider} → {plan.futures_provider}
                      </div>
                    </button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-xs"
                      className="mt-1.5 mr-1 shrink-0 text-muted-foreground opacity-70 hover:text-destructive group-hover:opacity-100"
                      title={`Delete ${plan.name}`}
                      aria-label={`Delete ${plan.name}`}
                      onClick={() => setDeleteTarget(plan)}
                    >
                      <IconTrash />
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>

      {/* Right panel: scrolls as one continuous column */}
      <div className="flex-1 overflow-y-auto p-4">
        <div className="flex flex-col gap-6">
          {selectedId === null ? (
            <div className="text-muted-foreground flex h-64 items-center justify-center text-sm">
              Select a delta-neutral plan to view details.
            </div>
          ) : selectedPlan ? (
            <>
              <PlanSummary plan={selectedPlan} />
              {latestSnap && (
                <>
                  <DeltaNeutralPnLCard plan={selectedPlan} snap={latestSnap} />
                  <DeltaNeutralYieldCard plan={selectedPlan} snap={latestSnap} />
                  <DeltaNeutralSpreadCard plan={selectedPlan} snapshot={latestSnap} />
                </>
              )}
              <DeltaNeutralYieldChart planId={selectedPlan.id} />
              <SnapshotTable planId={selectedPlan.id} snapshots={snapshots} />
              <AlertTable planId={selectedPlan.id} />
              <ExecutionTable planId={selectedPlan.id} />
            </>
          ) : null}
        </div>
      </div>

      {/* Delete dialogs (unchanged) */}
      <AlertDialog open={deleteTarget !== null} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            {deleteRequiresUnwind && (
              <div className="mb-2 inline-flex size-12 items-center justify-center rounded-md bg-destructive/10 text-destructive">
                <IconAlertTriangle className="size-5" />
              </div>
            )}
            <AlertDialogTitle>
              {deleteRequiresUnwind ? "Force unwind and delete plan?" : "Delete plan?"}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {deleteRequiresUnwind
                ? `This plan is ${deleteTarget?.status}. KhunQuant will unwind the open legs first, then delete the plan, its SQLite history, and its cron schedule.`
                : `Delete ${deleteTarget?.name ?? "this plan"} from SQLite and remove its cron schedule.`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter className="gap-2">
            <AlertDialogCancel className="min-w-24" disabled={deleteMutation.isPending}>
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              className="min-w-48"
              disabled={deleteMutation.isPending || deleteTarget === null}
              onClick={(event) => {
                event.preventDefault()
                if (deleteTarget) deleteMutation.mutate(deleteTarget)
              }}
            >
              {deleteMutation.isPending && <IconLoader2 className="animate-spin" />}
              {deleteRequiresUnwind ? "Force unwind and delete" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog
        open={deleteAnywayTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteAnywayTarget(null)
            setUnwindFailureMessage("")
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <div className="mb-2 inline-flex size-12 items-center justify-center rounded-md bg-destructive/10 text-destructive">
              <IconAlertTriangle className="size-5" />
            </div>
            <AlertDialogTitle>Delete plan record anyway?</AlertDialogTitle>
            <AlertDialogDescription className="space-y-3">
              <span className="block">
                The unwind step failed. This will skip live position closure and only remove the plan from
                SQLite plus its cron schedule.
              </span>
              {unwindFailureMessage && (
                <span className="block rounded-md bg-muted p-3 font-mono text-xs text-muted-foreground">
                  {unwindFailureMessage}
                </span>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter className="gap-2">
            <AlertDialogCancel className="min-w-24" disabled={deleteAnywayMutation.isPending}>
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              className="min-w-56"
              disabled={deleteAnywayMutation.isPending || deleteAnywayTarget === null}
              onClick={(event) => {
                event.preventDefault()
                if (deleteAnywayTarget) deleteAnywayMutation.mutate(deleteAnywayTarget)
              }}
            >
              {deleteAnywayMutation.isPending && <IconLoader2 className="animate-spin" />}
              Delete record anyway
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
