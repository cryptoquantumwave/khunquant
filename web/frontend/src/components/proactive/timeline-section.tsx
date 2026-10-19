import { IconLoader2 } from "@tabler/icons-react"
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query"
import { useCallback, useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import {
  type FeedbackResponse,
  type HindsightRow,
  type Intervention,
  getInterventions,
} from "@/api/proactive-dashboard"
import { formatTime } from "@/components/proactive/manage-utils"
import {
  type LiveState,
  useLiveEvents,
} from "@/components/proactive/use-live-events"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"

const PAGE_SIZE = 50
const TRIGGER_PATTERN = /^[A-Za-z0-9_.-]{1,64}$/
const DISPOSITIONS = ["", "notify", "escalate", "digest", "log", "drop"]

const SEVERITY_CLASS: Record<string, string> = {
  info: "bg-muted text-muted-foreground",
  notice: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400",
  warning:
    "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  critical: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
}

const LIVE_CLASS: Record<LiveState, string> = {
  live: "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400",
  connecting: "bg-muted text-muted-foreground",
  reconnecting:
    "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  unavailable:
    "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
}

// dayStart parses a <input type="date"> value as the start of that day in the browser's zone.
function dayStart(value: string, addDays = 0): string | undefined {
  if (!value) return undefined
  const d = new Date(`${value}T00:00:00`)
  if (Number.isNaN(d.getTime())) return undefined
  d.setDate(d.getDate() + addDays)
  return d.toISOString()
}

function pct(v: number | null): string {
  if (v === null) return "-"
  return `${v > 0 ? "+" : ""}${v.toFixed(2)}%`
}

export function TimelineSection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [from, setFrom] = useState("")
  const [to, setTo] = useState("")
  const [disposition, setDisposition] = useState("")
  const [trigger, setTrigger] = useState("")
  const triggerOk = trigger === "" || TRIGGER_PATTERN.test(trigger)

  const filter = {
    from: dayStart(from),
    // The end day is included: the range stops at the start of the day after.
    to: dayStart(to, 1),
    disposition,
    trigger: triggerOk ? trigger : "",
  }
  const query = useInfiniteQuery({
    queryKey: ["proactive", "interventions", filter],
    queryFn: ({ pageParam }) =>
      getInterventions({ ...filter, cursor: pageParam, limit: PAGE_SIZE }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
  })

  // A live event only says "something changed": refetch (at most once a second), so what
  // is shown is always what the read route returns. After a reconnect it fills the gap.
  const timer = useRef<number | undefined>(undefined)
  const refresh = useCallback(() => {
    if (timer.current !== undefined) return
    timer.current = window.setTimeout(() => {
      timer.current = undefined
      void queryClient.invalidateQueries({
        queryKey: ["proactive", "interventions"],
      })
      void queryClient.invalidateQueries({ queryKey: ["proactive", "metrics"] })
    }, 1000)
  }, [queryClient])
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const live = useLiveEvents(refresh)

  const first = query.data?.pages[0]
  const items = query.data?.pages.flatMap((p) => p.items ?? []) ?? []

  return (
    <Card className="border-border/60">
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle className="text-base">
              {t("pages.agent.proactive.timeline.title")}
            </CardTitle>
            <CardDescription>
              {t("pages.agent.proactive.timeline.description")}
            </CardDescription>
          </div>
          <span
            className={`rounded-full px-2 py-0.5 text-xs font-medium ${LIVE_CLASS[live]}`}
          >
            {t(`pages.agent.proactive.timeline.live.${live}`)}
          </span>
        </div>
        <div className="mt-2 grid grid-cols-2 gap-2 sm:grid-cols-4">
          <label className="space-y-1 text-xs">
            <span className="text-muted-foreground">
              {t("pages.agent.proactive.timeline.from")}
            </span>
            <Input
              type="date"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
            />
          </label>
          <label className="space-y-1 text-xs">
            <span className="text-muted-foreground">
              {t("pages.agent.proactive.timeline.to")}
            </span>
            <Input
              type="date"
              value={to}
              onChange={(e) => setTo(e.target.value)}
            />
          </label>
          <label className="space-y-1 text-xs">
            <span className="text-muted-foreground">
              {t("pages.agent.proactive.decisions.disposition")}
            </span>
            <select
              className="border-input bg-background h-9 w-full rounded-md border px-2 text-sm"
              value={disposition}
              onChange={(e) => setDisposition(e.target.value)}
            >
              {DISPOSITIONS.map((d) => (
                <option key={d} value={d}>
                  {d === "" ? t("pages.agent.proactive.timeline.any") : d}
                </option>
              ))}
            </select>
          </label>
          <label className="space-y-1 text-xs">
            <span className="text-muted-foreground">
              {t("pages.agent.proactive.timeline.trigger")}
            </span>
            <Input
              value={trigger}
              aria-invalid={!triggerOk}
              placeholder="price_cross"
              onChange={(e) => setTrigger(e.target.value.trim())}
            />
          </label>
        </div>
      </CardHeader>
      <CardContent>
        {query.isLoading ? (
          <div className="text-muted-foreground flex items-center gap-2 py-4 text-sm">
            <IconLoader2 className="size-4 animate-spin" />
            {t("pages.agent.proactive.loading")}
          </div>
        ) : query.error ? (
          <p className="text-muted-foreground py-4 text-center text-sm">
            {t("pages.agent.proactive.manage.unreachable")}
          </p>
        ) : first && first.available === false ? (
          <div className="text-muted-foreground py-4 text-center text-sm">
            <p>{t("pages.agent.proactive.manage.unavailable")}</p>
            {first.reason && (
              <p className="mt-1 text-xs opacity-70">{first.reason}</p>
            )}
          </div>
        ) : items.length === 0 ? (
          <div className="text-muted-foreground py-8 text-center text-sm">
            <p>{t("pages.agent.proactive.decisions.empty")}</p>
            <p className="mt-1 text-xs opacity-70">
              {t("pages.agent.proactive.decisions.empty_hint")}
            </p>
          </div>
        ) : (
          <div className="space-y-2">
            {items.map((row) => (
              <TimelineRow key={row.id} row={row} />
            ))}
            {query.hasNextPage && (
              <div className="pt-2 text-center">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={query.isFetchingNextPage}
                  onClick={() => void query.fetchNextPage()}
                >
                  {query.isFetchingNextPage && (
                    <IconLoader2 className="size-3.5 animate-spin" />
                  )}
                  {t("pages.agent.proactive.timeline.more")}
                </Button>
              </div>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function Badge({
  children,
  className,
}: {
  children: string
  className?: string
}) {
  return (
    <span
      className={`rounded-full px-2 py-0.5 text-xs font-medium ${
        className ?? "bg-muted text-muted-foreground"
      }`}
    >
      {children}
    </span>
  )
}

function ResponseBadge({ r }: { r: FeedbackResponse }) {
  const { t } = useTranslation()
  const label = t(`pages.agent.proactive.timeline.response.${r.kind}`, {
    defaultValue: r.kind,
  })
  return (
    <Badge className="bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400">
      {r.value && r.kind !== "none" ? `${label}: ${r.value}` : label}
    </Badge>
  )
}

function HindsightBadge({ h }: { h: HindsightRow }) {
  const { t } = useTranslation()
  const parts = [`${h.horizon} ${pct(h.pct_change)}`]
  if (h.trades !== null) {
    parts.push(t("pages.agent.proactive.timeline.trades", { count: h.trades }))
  }
  return (
    <Badge className="bg-teal-100 text-teal-700 dark:bg-teal-900/30 dark:text-teal-400">
      {parts.join(" · ")}
    </Badge>
  )
}

// A row shows model- or person-written text (the title, a reason) as plain text, only.
function TimelineRow({ row }: { row: Intervention }) {
  const { t } = useTranslation()
  return (
    <div className="border-border/40 space-y-1.5 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground font-mono text-xs">
          {formatTime(row.at)}
        </span>
        <Badge
          className={SEVERITY_CLASS[row.severity_name] ?? SEVERITY_CLASS.info}
        >
          {row.severity_name}
        </Badge>
        <span className="text-sm font-medium">{row.disposition}</span>
        {row.shadow && (
          <span className="text-muted-foreground text-xs">
            ({t("pages.agent.proactive.decisions.shadow")})
          </span>
        )}
        <span className="text-muted-foreground font-mono text-xs">
          {row.trigger_kind || "-"}
        </span>
        <span
          className="ml-auto text-xs"
          aria-label={
            row.delivered
              ? t("pages.agent.proactive.decisions.yes")
              : t("pages.agent.proactive.decisions.no")
          }
        >
          {row.delivered
            ? `✓ ${t("pages.agent.proactive.decisions.yes")}`
            : t("pages.agent.proactive.decisions.no")}
        </span>
      </div>
      {row.title && <p className="text-sm break-words">{row.title}</p>}
      {row.reason && (
        <p className="text-muted-foreground text-xs break-words">
          {row.reason}
        </p>
      )}
      {(row.responses.length > 0 || row.hindsight.length > 0) && (
        <div className="flex flex-wrap gap-1.5">
          {row.responses.map((r, i) => (
            <ResponseBadge key={`r${i}`} r={r} />
          ))}
          {row.hindsight.map((h) => (
            <HindsightBadge key={h.horizon} h={h} />
          ))}
        </div>
      )}
    </div>
  )
}
