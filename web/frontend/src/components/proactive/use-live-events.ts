import { useEffect, useRef, useState } from "react"

export type LiveState = "connecting" | "live" | "unavailable" | "reconnecting"

const EVENTS_URL = "/api/agent/proactive/events"
const BASE_DELAY_MS = 1_000
const MAX_DELAY_MS = 30_000

// The event names the launcher sends besides "stream.status": one for a decision, and
// "approval.<status>" for a proposal's status.
const ITEM_EVENTS = [
  "attention.decision",
  "approval.pending",
  "approval.approved",
  "approval.rejected",
  "approval.expired",
  "approval.executing",
  "approval.executed",
  "approval.failed",
  "approval.cancelled",
]

// reconnectDelay is exponential backoff with jitter: 1 s, 2 s, 4 s ... capped at 30 s.
export function reconnectDelay(attempt: number, random = Math.random): number {
  const base = Math.min(MAX_DELAY_MS, BASE_DELAY_MS * 2 ** attempt)
  return Math.round(base * (0.75 + random() * 0.5))
}

// useLiveEvents follows the launcher's event stream and calls onChange for every item (a
// decision or an approval change), and once more after a reconnect, so the caller can
// refetch and close any gap. The stream is only a signal: what is shown always comes from
// the read routes, never from an event's text.
//
// A browser's EventSource retries on a fixed delay, so a failed source is closed and
// reopened here with exponential backoff, resuming from the last event id.
export function useLiveEvents(onChange: () => void): LiveState {
  const [state, setState] = useState<LiveState>("connecting")
  const callback = useRef(onChange)
  useEffect(() => {
    callback.current = onChange
  })

  useEffect(() => {
    let source: EventSource | null = null
    let timer: number | undefined
    let attempt = 0
    let cursor = ""
    let stopped = false
    let wasDown = false

    const connect = () => {
      const url = cursor
        ? `${EVENTS_URL}?cursor=${encodeURIComponent(cursor)}`
        : EVENTS_URL
      const es = new EventSource(url)
      source = es
      es.onopen = () => {
        attempt = 0
        if (wasDown) {
          wasDown = false
          callback.current()
        }
      }
      es.addEventListener("stream.status", (e) => {
        try {
          const body = JSON.parse((e as MessageEvent<string>).data) as {
            state?: string
          }
          setState(body.state === "live" ? "live" : "unavailable")
        } catch {
          setState("unavailable")
        }
      })
      for (const name of ITEM_EVENTS) {
        es.addEventListener(name, (e) => {
          const id = (e as MessageEvent<string>).lastEventId
          if (id) cursor = id
          callback.current()
        })
      }
      es.onerror = () => {
        es.close()
        if (stopped) return
        wasDown = true
        setState("reconnecting")
        timer = window.setTimeout(connect, reconnectDelay(attempt))
        attempt += 1
      }
    }
    connect()
    return () => {
      stopped = true
      window.clearTimeout(timer)
      source?.close()
    }
  }, [])

  return state
}
