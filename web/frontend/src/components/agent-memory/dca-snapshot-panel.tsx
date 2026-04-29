import { IconLoader2 } from "@tabler/icons-react"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  type DCAPlanListItem,
  type DCAExecutionItem,
  getDCAExecutions,
  listDCAPlans,
} from "@/api/agent-dca"

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

function quoteCurrency(symbol: string): string {
  const i = symbol.lastIndexOf("/")
  return i >= 0 ? symbol.slice(i + 1) : ""
}

function PlanSummary({ plan }: { plan: DCAPlanListItem }) {
  const quote = quoteCurrency(plan.symbol)

  const statCell = (label: string, value: string) => (
    <div className="rounded-lg border p-3">
      <div className="text-muted-foreground mb-1 text-xs">{label}</div>
      <div className="font-mono text-sm font-medium">{value}</div>
    </div>
  )

  return (
    <div className="flex flex-col gap-4">
      {/* Plan header */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-foreground font-semibold">{plan.name}</span>
        <span className="text-muted-foreground text-xs">{plan.symbol}</span>
        <span
          className={`rounded-full px-2 py-0.5 text-xs font-medium ${
            plan.enabled
              ? "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
              : "bg-muted text-muted-foreground"
          }`}
        >
          {plan.enabled ? "Active" : "Paused"}
        </span>
      </div>

      <div className="text-muted-foreground text-xs">
        {plan.provider}
        {plan.account ? ` · ${plan.account}` : ""} · {plan.frequency_expr} ({plan.timezone})
      </div>

      {/* Stats grid */}
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {statCell("Amount / Order", `${formatNum(plan.amount_per_order, 2)} ${quote}`)}
        {statCell("Total Invested", `${formatNum(plan.total_invested, 2)} ${quote}`)}
        {statCell("Avg Cost (VWAP)", plan.avg_cost > 0 ? `${formatNum(plan.avg_cost, 4)} ${quote}` : "—")}
        {statCell("Total Acquired", plan.total_quantity > 0 ? `${plan.total_quantity.toFixed(8)} ${plan.symbol.split("/")[0]}` : "—")}
      </div>

    </div>
  )
}

function ExecutionTable({ planId }: { planId: number }) {
  const { data: execs, isLoading } = useQuery({
    queryKey: ["dca-executions", planId],
    queryFn: () => getDCAExecutions(planId, { limit: 50 }),
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
      <div className="text-muted-foreground py-4 text-center text-sm">No executions yet.</div>
    )
  }

  return (
    <div className="overflow-hidden rounded-lg border">
      <div className="border-border/50 border-b px-3 py-2">
        <span className="text-foreground/80 text-sm font-medium">Execution History</span>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="bg-muted/40 text-muted-foreground border-b text-xs uppercase tracking-wide">
              <th className="px-3 py-2 text-left">Date</th>
              <th className="px-3 py-2 text-right">Price</th>
              <th className="px-3 py-2 text-right">Qty</th>
              <th className="px-3 py-2 text-right">Spent</th>
              <th className="px-3 py-2 text-center">Status</th>
            </tr>
          </thead>
          <tbody>
            {execs.map((e: DCAExecutionItem) => (
              <>
                <tr key={e.id} className="border-border/30 border-b last:border-0">
                  <td className="text-muted-foreground px-3 py-2 font-mono text-xs">
                    {formatDate(e.executed_at)}
                  </td>
                  <td className="px-3 py-2 text-right font-mono">
                    {formatNum(e.filled_price, 4)}
                  </td>
                  <td className="px-3 py-2 text-right font-mono">
                    {e.filled_quantity.toFixed(6)}
                  </td>
                  <td className="px-3 py-2 text-right font-mono">
                    {formatNum(e.amount_quote, 2)}
                  </td>
                  <td className="px-3 py-2 text-center">
                    <span
                      className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                        e.status === "completed"
                          ? "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"
                          : "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"
                      }`}
                    >
                      {e.status}
                    </span>
                  </td>
                </tr>
                {e.error_msg && (
                  <tr key={`${e.id}-err`} className="border-border/20 border-b last:border-0">
                    <td colSpan={5} className="px-3 pb-2 text-xs text-red-500">
                      ↳ {e.error_msg}
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

export function DCASnapshotPanel() {
  const [selectedId, setSelectedId] = useState<number | null>(null)

  const { data: plans, isLoading } = useQuery({
    queryKey: ["dca-plans"],
    queryFn: () => listDCAPlans(),
  })

  const selectedPlan = plans?.find((p) => p.id === selectedId)

  const itemClass = (id: number) =>
    `w-full rounded-md px-3 py-2 text-left text-sm transition-colors ${
      selectedId === id
        ? "bg-accent/80 text-foreground font-medium"
        : "text-muted-foreground hover:bg-muted/60"
    }`

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      {/* Left panel: plan list */}
      <div className="border-border/40 flex w-64 shrink-0 flex-col border-r">
        <div className="flex-1 overflow-auto p-2">
          {isLoading ? (
            <div className="text-muted-foreground p-2 text-sm">Loading…</div>
          ) : !plans || plans.length === 0 ? (
            <div className="text-muted-foreground p-2 text-sm">No DCA plans found.</div>
          ) : (
            <ul className="space-y-0.5">
              {plans.map((plan: DCAPlanListItem) => (
                <li key={plan.id}>
                  <button onClick={() => setSelectedId(plan.id)} className={itemClass(plan.id)}>
                    <div className="flex items-center gap-1.5">
                      <span
                        className={`size-1.5 shrink-0 rounded-full ${plan.enabled ? "bg-green-500" : "bg-muted-foreground"}`}
                      />
                      <span className="truncate font-medium">{plan.name}</span>
                    </div>
                    <div className="text-muted-foreground mt-0.5 font-mono text-xs">
                      {plan.symbol} · {plan.amount_per_order.toFixed(0)}/order
                    </div>
                    <div className="text-muted-foreground text-xs">{plan.provider}</div>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>

      {/* Right panel: plan detail + executions */}
      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-auto p-4">
        {selectedId === null ? (
          <div className="text-muted-foreground flex h-full items-center justify-center text-sm">
            Select a DCA plan to view details.
          </div>
        ) : selectedPlan ? (
          <>
            <PlanSummary plan={selectedPlan} />
            <ExecutionTable planId={selectedPlan.id} />
          </>
        ) : null}
      </div>
    </div>
  )
}
