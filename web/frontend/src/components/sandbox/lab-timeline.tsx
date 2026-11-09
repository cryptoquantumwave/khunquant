"use client"

import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"

import { type LabTimelineEntry, getLabTimeline } from "@/api/sandbox"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

// One kind family per chip, as the journal names them: "x.*" is a family, a name
// without a dot is one exact kind.
const FAMILIES: Record<string, string> = {
  event: "event.*",
  chat: "chat.*",
  llm: "llm.*",
  workflow: "workflow.step",
  proposal: "proposal",
  policy: "policy.decision",
  tool: "tool.call",
  exchange: "exchange.*",
  fault: "fault.*",
  clock: "clock.*",
  settle: "settle,crash",
}

const POLL_MS = 2000
const IDLE_POLL_MS = 30000 // not a locked sandbox, or unreachable
const PAGE = 500
const MAX_ROWS = 5000

const SEVERITY_CLASS: Record<string, string> = {
  info: "bg-muted text-muted-foreground",
  notice: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400",
  warning:
    "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  critical: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
}

type State = "loading" | "locked" | "unlocked" | "error"

// A chat.out message carries its severity in the payload.
function severityOf(e: LabTimelineEntry): string | undefined {
  if (e.kind !== "chat.out" || typeof e.payload !== "object" || !e.payload) {
    return undefined
  }
  const s = (e.payload as Record<string, unknown>).severity
  return typeof s === "string" ? s : undefined
}

// The payload as text. It is rendered only inside a <pre>, as a text node: a message
// or a tool argument written by a person or a model is never markup here.
function payloadText(e: LabTimelineEntry): string {
  return typeof e.payload === "string"
    ? e.payload
    : JSON.stringify(e.payload, null, 2)
}

export function LabTimeline() {
  const { t } = useTranslation()
  const [selected, setSelected] = useState<string[]>([])
  const [utc, setUtc] = useState(true)
  const [open, setOpen] = useState<Set<number>>(new Set())
  const [state, setState] = useState<State>("loading")
  const [instance, setInstance] = useState("")
  const [drops, setDrops] = useState(0)
  // Rows are keyed by the filter they were read with, so a chip change shows no stale rows.
  const [data, setData] = useState<{ key: string; rows: LabTimelineEntry[] }>({
    key: "",
    rows: [],
  })

  const kinds = selected.map((f) => FAMILIES[f]).join(",")
  const rows = data.key === kinds ? data.rows : []

  useEffect(() => {
    let stopped = false
    let timer: number | undefined
    let cursor = 0
    const tick = async () => {
      let delay = POLL_MS
      try {
        const r = await getLabTimeline({
          afterSeq: cursor,
          kinds: kinds || undefined,
          limit: PAGE,
        })
        if (stopped) return
        if (!r.locked) {
          setState("unlocked")
          delay = IDLE_POLL_MS
        } else {
          setState("locked")
          setInstance(r.instance)
          setDrops(r.drops)
          cursor = r.next_seq
          if (r.entries.length > 0) {
            setData((prev) => ({
              key: kinds,
              rows: [
                ...(prev.key === kinds ? prev.rows : []),
                ...r.entries,
              ].slice(-MAX_ROWS),
            }))
          }
          if (r.entries.length >= PAGE) delay = 50 // more backlog
        }
      } catch {
        if (stopped) return
        setState("error")
        delay = IDLE_POLL_MS
      }
      timer = window.setTimeout(tick, delay)
    }
    void tick()
    return () => {
      stopped = true
      window.clearTimeout(timer)
    }
  }, [kinds])

  const toggleFamily = (f: string) =>
    setSelected((prev) =>
      prev.includes(f) ? prev.filter((x) => x !== f) : [...prev, f],
    )
  const toggleRow = (seq: number) =>
    setOpen((prev) => {
      const next = new Set(prev)
      if (!next.delete(seq)) next.add(seq)
      return next
    })
  const clock = (iso: string) =>
    utc
      ? iso.slice(0, 23).replace("T", " ") + "Z"
      : new Date(iso).toLocaleString()

  return (
    <Card>
      <CardHeader>
        <CardTitle>
          {t("pages.sandbox.timeline.title", "Lab timeline")}
          {instance ? ` · ${instance}` : ""}
        </CardTitle>
        <CardDescription>
          {t(
            "pages.sandbox.timeline.desc",
            "Read-only journal of one locked sandbox instance. Start one launcher per instance with KHUNQUANT_CONFIG=<instance home>/config.json.",
          )}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {state === "unlocked" ? (
          <p className="text-muted-foreground text-sm">
            {t(
              "pages.sandbox.timeline.not_locked",
              "This gateway is not a locked sandbox, so there is no lab journal to show.",
            )}
          </p>
        ) : state === "loading" ? (
          <p className="text-muted-foreground text-sm">{t("labels.loading")}</p>
        ) : (
          <>
            {state === "error" && (
              <p className="text-destructive text-sm">
                {t(
                  "pages.sandbox.timeline.error",
                  "The timeline could not be read from the gateway.",
                )}
              </p>
            )}
            {drops > 0 && (
              <p className="rounded bg-amber-100 px-2 py-1 text-sm text-amber-800 dark:bg-amber-900/30 dark:text-amber-400">
                {t(
                  "pages.sandbox.timeline.drops",
                  "The journal dropped {{count}} entries; assertions may be incomplete.",
                  { count: drops },
                )}
              </p>
            )}
            <div className="flex flex-wrap items-center gap-1.5">
              {Object.keys(FAMILIES).map((f) => (
                <Button
                  key={f}
                  size="sm"
                  variant={selected.includes(f) ? "default" : "outline"}
                  onClick={() => toggleFamily(f)}
                >
                  {t(`pages.sandbox.timeline.family.${f}`, f)}
                </Button>
              ))}
              <Button size="sm" variant="ghost" onClick={() => setUtc(!utc)}>
                {utc
                  ? t("pages.sandbox.timeline.utc", "UTC")
                  : t("pages.sandbox.timeline.local", "Local")}
              </Button>
            </div>
            {rows.length === 0 ? (
              <p className="text-muted-foreground text-sm">
                {t("pages.sandbox.timeline.empty", "No journal entries yet.")}
              </p>
            ) : (
              <ul className="max-h-[480px] divide-y overflow-auto rounded border text-xs">
                {[...rows].reverse().map((e) => (
                  <li key={e.seq} className="px-2 py-1.5">
                    <button
                      type="button"
                      className="flex w-full flex-wrap items-baseline gap-2 text-left"
                      onClick={() => toggleRow(e.seq)}
                    >
                      <span className="text-muted-foreground font-mono">
                        {clock(e.sim_time)}
                      </span>
                      <span
                        className={`rounded px-1.5 py-0.5 font-mono ${SEVERITY_CLASS[severityOf(e) ?? "info"] ?? SEVERITY_CLASS.info}`}
                      >
                        {e.kind}
                      </span>
                      <span className="font-medium">{e.subject}</span>
                      <span className="text-muted-foreground min-w-0 break-words">
                        {e.summary}
                      </span>
                    </button>
                    {open.has(e.seq) && (
                      <>
                        {e.truncated && (
                          <p className="text-muted-foreground mt-1">
                            {t(
                              "pages.sandbox.timeline.truncated",
                              "Payload cut at 4 KiB.",
                            )}
                          </p>
                        )}
                        <pre className="bg-muted mt-1 max-h-64 overflow-auto rounded p-2 font-mono break-all whitespace-pre-wrap">
                          {payloadText(e)}
                        </pre>
                      </>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
