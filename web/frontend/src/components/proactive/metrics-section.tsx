import { IconLoader2 } from "@tabler/icons-react"
import { useQuery } from "@tanstack/react-query"
import {
  BarElement,
  CategoryScale,
  Chart as ChartJS,
  Legend,
  LineElement,
  LinearScale,
  PointElement,
  Tooltip,
} from "chart.js"
import { type ReactNode, useState } from "react"
import { Bar, Line } from "react-chartjs-2"
import { useTranslation } from "react-i18next"

import {
  type ApprovalRate,
  type DashboardMetrics,
  type HarmMetric,
  type Metric,
  getMetrics,
} from "@/api/proactive-dashboard"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

ChartJS.register(
  CategoryScale,
  LinearScale,
  PointElement,
  LineElement,
  BarElement,
  Tooltip,
  Legend,
)

const RANGES = [7, 30, 90]
const REFRESH_MS = 30_000

function percent(v: number): string {
  return `${(v * 100).toFixed(0)}%`
}

function seconds(v: number): string {
  if (v < 120) return `${v.toFixed(0)} s`
  if (v < 7200) return `${(v / 60).toFixed(1)} min`
  return `${(v / 3600).toFixed(1)} h`
}

// MetricCard shows a number, or the reason there is none. A metric with no data is never
// shown as 0.
function MetricCard<T>({
  label,
  hint,
  metric,
  render,
}: {
  label: string
  hint?: string
  metric: Metric<T>
  render: (v: T) => ReactNode
}) {
  const { t } = useTranslation()
  return (
    <div className="border-border/40 space-y-1 rounded-lg border p-3">
      <p className="text-muted-foreground text-xs">{label}</p>
      {metric.value === null ? (
        <>
          <p className="text-muted-foreground text-lg font-semibold">
            {t("pages.agent.proactive.metrics.no_data")}
          </p>
          <p className="text-muted-foreground text-xs">{metric.reason}</p>
        </>
      ) : (
        <>
          <p className="text-lg font-semibold">{render(metric.value)}</p>
          {hint && <p className="text-muted-foreground text-xs">{hint}</p>}
        </>
      )}
    </div>
  )
}

export function MetricsSection() {
  const { t } = useTranslation()
  const [days, setDays] = useState(30)
  const query = useQuery({
    queryKey: ["proactive", "metrics", days],
    queryFn: () => getMetrics(days),
    refetchInterval: REFRESH_MS,
  })
  const data = query.data

  return (
    <Card className="border-border/60">
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle className="text-base">
              {t("pages.agent.proactive.metrics.title")}
            </CardTitle>
            <CardDescription>
              {t("pages.agent.proactive.metrics.description")}
            </CardDescription>
          </div>
          <div className="flex gap-1">
            {RANGES.map((d) => (
              <Button
                key={d}
                size="sm"
                variant={days === d ? "secondary" : "ghost"}
                onClick={() => setDays(d)}
              >
                {t("pages.agent.proactive.metrics.days", { count: d })}
              </Button>
            ))}
          </div>
        </div>
      </CardHeader>
      <CardContent>
        {query.isLoading ? (
          <div className="text-muted-foreground flex items-center gap-2 py-4 text-sm">
            <IconLoader2 className="size-4 animate-spin" />
            {t("pages.agent.proactive.loading")}
          </div>
        ) : query.error || !data ? (
          <p className="text-muted-foreground py-4 text-center text-sm">
            {t("pages.agent.proactive.manage.unreachable")}
          </p>
        ) : !data.available || !data.metrics ? (
          <div className="text-muted-foreground py-4 text-center text-sm">
            <p>{t("pages.agent.proactive.manage.unavailable")}</p>
            {data.reason && (
              <p className="mt-1 text-xs opacity-70">{data.reason}</p>
            )}
          </div>
        ) : (
          <MetricsBody m={data.metrics} />
        )}
      </CardContent>
    </Card>
  )
}

const RESPONSE_COLORS: Record<string, string> = {
  useful: "#10b981",
  not_useful: "#ef4444",
  mute: "#f59e0b",
  snooze: "#a3a3a3",
  none: "#d4d4d4",
}

function MetricsBody({ m }: { m: DashboardMetrics }) {
  const { t } = useTranslation()
  const label = (k: string) => t(`pages.agent.proactive.metrics.cards.${k}`)
  const sends = m.sends_per_day.value
  const kinds = m.responses_by_kind.value

  return (
    <div className="space-y-4">
      <p className="text-muted-foreground text-xs">
        {t("pages.agent.proactive.metrics.window", {
          from: m.from,
          to: m.to,
          zone: m.zone,
        })}
      </p>
      <div className="grid grid-cols-2 gap-2 md:grid-cols-3">
        <MetricCard
          label={label("sends")}
          metric={m.sends_per_day}
          render={(v) =>
            t("pages.agent.proactive.metrics.per_day", {
              value: v.average.toFixed(1),
              total: v.total,
            })
          }
        />
        <MetricCard
          label={label("precision")}
          hint={label("precision_hint")}
          metric={m.precision_at_sent}
          render={percent}
        />
        <MetricCard
          label={label("not_useful")}
          metric={m.not_useful_rate}
          render={percent}
        />
        <MetricCard
          label={label("mute")}
          metric={m.mute_rate}
          render={percent}
        />
        <MetricCard
          label={label("no_response")}
          metric={m.no_response_rate}
          render={percent}
        />
        <MetricCard
          label={label("latency")}
          metric={m.median_queue_latency_seconds}
          render={seconds}
        />
        <MetricCard
          label={label("critical_recall")}
          metric={m.critical_recall}
          render={percent}
        />
        <MetricCard
          label={label("llm")}
          metric={m.llm_usage_per_day}
          render={(rows) => {
            const wakes = rows.reduce((a, r) => a + r.wakes, 0)
            const tokens = rows.reduce((a, r) => a + r.tokens, 0)
            return t("pages.agent.proactive.metrics.llm_value", {
              wakes: (wakes / m.days).toFixed(1),
              tokens: Math.round(tokens / m.days).toLocaleString(),
            })
          }}
        />
        <MetricCard
          label={label("learning")}
          metric={m.learning_per_day}
          render={(rows) =>
            t("pages.agent.proactive.metrics.learning_value", {
              low: rows.reduce((a, r) => a + r.low_acceptance, 0),
              holdout: rows.reduce((a, r) => a + r.holdout, 0),
            })
          }
        />
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <div className="border-border/40 rounded-lg border p-3">
          <p className="mb-2 text-sm font-medium">
            {t("pages.agent.proactive.metrics.chart_sends")}
          </p>
          {sends ? (
            <Line
              data={{
                labels: sends.series.map((d) => d.day.slice(5)),
                datasets: [
                  {
                    label: label("sends"),
                    data: sends.series.map((d) => d.sends),
                    borderColor: "#6366f1",
                    backgroundColor: "rgba(99,102,241,0.1)",
                    tension: 0.3,
                  },
                ],
              }}
              options={{
                plugins: { legend: { display: false } },
                scales: { y: { beginAtZero: true, ticks: { precision: 0 } } },
              }}
            />
          ) : (
            <p className="text-muted-foreground text-xs">
              {m.sends_per_day.reason}
            </p>
          )}
        </div>
        <div className="border-border/40 rounded-lg border p-3">
          <p className="mb-2 text-sm font-medium">
            {t("pages.agent.proactive.metrics.chart_responses")}
          </p>
          {kinds && Object.keys(kinds).length > 0 ? (
            <Bar
              data={{
                labels: Object.keys(kinds).map((k) =>
                  t(`pages.agent.proactive.timeline.response.${k}`, {
                    defaultValue: k,
                  }),
                ),
                datasets: [
                  {
                    label: t("pages.agent.proactive.metrics.chart_responses"),
                    data: Object.values(kinds),
                    backgroundColor: Object.keys(kinds).map(
                      (k) => RESPONSE_COLORS[k] ?? "#6366f1",
                    ),
                  },
                ],
              }}
              options={{
                plugins: { legend: { display: false } },
                scales: { y: { beginAtZero: true, ticks: { precision: 0 } } },
              }}
            />
          ) : (
            <p className="text-muted-foreground text-xs">
              {kinds
                ? t("pages.agent.proactive.metrics.no_responses")
                : m.responses_by_kind.reason}
            </p>
          )}
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <HarmCard metric={m.post_alert_trading} />
        <ApprovalTable metric={m.approval_rates} />
      </div>
    </div>
  )
}

// HarmCard is the harm guardrail: trading after an alert against trading on quiet days.
// It is here so the owner can watch it. Nothing optimises for it.
function HarmCard({ metric }: { metric: Metric<HarmMetric> }) {
  const { t } = useTranslation()
  const rate = (v: number | null) => (v === null ? "-" : v.toFixed(2))
  const h = metric.value
  return (
    <div className="space-y-2 rounded-lg border border-amber-300/50 bg-amber-50/50 p-3 dark:border-amber-500/30 dark:bg-amber-900/10">
      <p className="text-sm font-medium">
        {t("pages.agent.proactive.metrics.harm.title")}
      </p>
      <p className="text-muted-foreground text-xs">
        {t("pages.agent.proactive.metrics.harm.note")}
      </p>
      {h === null ? (
        <p className="text-muted-foreground text-xs">{metric.reason}</p>
      ) : (
        <dl className="grid grid-cols-2 gap-x-3 gap-y-1 text-sm">
          <dt className="text-muted-foreground text-xs">
            {t("pages.agent.proactive.metrics.harm.after_alert")}
          </dt>
          <dd>
            {rate(h.post_alert_per_alert_day)} ({h.post_alert_trades} /{" "}
            {h.alert_days})
          </dd>
          <dt className="text-muted-foreground text-xs">
            {t("pages.agent.proactive.metrics.harm.quiet")}
          </dt>
          <dd>
            {rate(h.quiet_per_quiet_day)} ({h.quiet_trades} / {h.quiet_days})
          </dd>
          <dt className="text-muted-foreground text-xs">
            {t("pages.agent.proactive.metrics.harm.contracts")}
          </dt>
          <dd>
            {rate(h.post_alert_contract_per_alert_day)} /{" "}
            {rate(h.quiet_contract_per_quiet_day)}
          </dd>
        </dl>
      )}
      {h !== null && (
        <p className="text-muted-foreground text-xs">
          {t("pages.agent.proactive.metrics.harm.window", {
            days: h.window_days,
          })}
        </p>
      )}
    </div>
  )
}

function ApprovalTable({ metric }: { metric: Metric<ApprovalRate[]> }) {
  const { t } = useTranslation()
  return (
    <div className="space-y-2 rounded-lg border p-3">
      <p className="text-sm font-medium">
        {t("pages.agent.proactive.metrics.approvals.title")}
      </p>
      {metric.value === null ? (
        <p className="text-muted-foreground text-xs">{metric.reason}</p>
      ) : (
        <table className="w-full text-xs">
          <thead>
            <tr className="text-muted-foreground text-left">
              <th className="py-1">
                {t("pages.agent.proactive.metrics.approvals.group")}
              </th>
              <th>{t("pages.agent.proactive.metrics.approvals.approved")}</th>
              <th>{t("pages.agent.proactive.metrics.approvals.rejected")}</th>
              <th>{t("pages.agent.proactive.metrics.approvals.expired")}</th>
              <th>{t("pages.agent.proactive.metrics.approvals.rate")}</th>
            </tr>
          </thead>
          <tbody>
            {metric.value.map((a) => (
              <tr key={a.group} className="border-border/30 border-t">
                <td className="py-1">
                  {t(
                    `pages.agent.proactive.metrics.approvals.groups.${a.group}`,
                    {
                      defaultValue: a.group,
                    },
                  )}
                </td>
                <td>{a.approved}</td>
                <td>{a.rejected}</td>
                <td>{a.expired}</td>
                <td>{a.rate === null ? "-" : percent(a.rate)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
