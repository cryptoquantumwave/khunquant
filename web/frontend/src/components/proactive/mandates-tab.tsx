import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import {
  type MandateRow,
  getMandates,
  revokeMandate,
} from "@/api/proactive-manage"
import {
  ConfirmButton,
  ManageQuery,
} from "@/components/proactive/manage-shared"
import {
  MANAGE_QUERY_KEY,
  MANAGE_REFRESH_MS,
  formatTime,
  useManageAction,
} from "@/components/proactive/manage-utils"

function usd(n: number): string {
  return `$${n.toLocaleString(undefined, { maximumFractionDigits: 2 })}`
}

export function MandatesTab() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: [...MANAGE_QUERY_KEY, "mandates"],
    queryFn: getMandates,
    refetchInterval: MANAGE_REFRESH_MS,
  })
  const revoke = useManageAction((id: string) => revokeMandate(id))

  return (
    <ManageQuery query={query}>
      {(data) => {
        // Active mandates first, then the ended ones as history.
        const rows = [...(data.mandates ?? [])].sort(
          (a, b) =>
            Number(b.status === "active") - Number(a.status === "active"),
        )
        return rows.length === 0 ? (
          <p className="text-muted-foreground py-6 text-center text-sm">
            {t("pages.agent.proactive.manage.mandates.empty")}
          </p>
        ) : (
          <div className="space-y-3">
            {rows.map((row) => (
              <MandateCard
                key={row.id}
                row={row}
                pending={revoke.isPending && revoke.variables === row.id}
                onRevoke={() => revoke.mutate(row.id)}
              />
            ))}
          </div>
        )
      }}
    </ManageQuery>
  )
}

function MandateCard({
  row,
  pending,
  onRevoke,
}: {
  row: MandateRow
  pending: boolean
  onRevoke: () => void
}) {
  const { t } = useTranslation()
  const m = (key: string, opts?: Record<string, unknown>) =>
    t(`pages.agent.proactive.manage.mandates.${key}`, opts)
  const cap = row.max_per_period_usd
  const pct =
    row.usage_known && cap > 0
      ? Math.min(100, Math.round((row.used_usd / cap) * 100))
      : 0
  const plan = `${row.side} ${row.symbol} · ${row.provider}${row.account ? ` / ${row.account}` : ""}`

  return (
    <div className="rounded-lg border p-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-sm font-medium">{plan}</p>
            <span className="bg-muted text-muted-foreground rounded-full px-2 py-0.5 text-xs font-medium">
              {row.status}
            </span>
          </div>
          <div>
            <div className="text-muted-foreground mb-1 flex justify-between text-xs">
              <span>
                {row.usage_known
                  ? `${m("used")}: ${usd(row.used_usd)} / ${usd(cap)}`
                  : m("usage_unknown")}
              </span>
              <span>{m("per_period", { period: row.period })}</span>
            </div>
            <div
              className="bg-muted h-2 overflow-hidden rounded-full"
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={pct}
            >
              <div
                className={`h-full rounded-full ${pct >= 90 ? "bg-red-500" : pct >= 70 ? "bg-amber-500" : "bg-primary"}`}
                style={{ width: `${pct}%` }}
              />
            </div>
          </div>
          <p className="text-muted-foreground text-xs">
            {m("per_run")}: {usd(row.max_per_run_usd)} · {m("expires")}:{" "}
            {formatTime(row.expires_at)}
          </p>
        </div>
        {row.status === "active" && (
          <ConfirmButton
            label={m("revoke")}
            title={m("revoke_title")}
            description={m("revoke_description")}
            detail={<p className="text-sm font-medium">{plan}</p>}
            variant="destructive"
            pending={pending}
            onConfirm={onRevoke}
          />
        )}
      </div>
    </div>
  )
}
