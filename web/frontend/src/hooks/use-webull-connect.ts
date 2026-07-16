import { useCallback, useEffect, useRef, useState } from "react"

import {
  type WebullSessionStatus,
  connectWebull,
  getWebullStatus,
} from "@/api/webull"

const STATUS_POLL_MS = 3000

// enabled lets callers that render this hook unconditionally (e.g. a shared
// account-card component used by every exchange, where Rules of Hooks bars
// calling the hook only when isWebull is true) skip all network activity
// for non-Webull accounts instead of polling a status endpoint that doesn't
// apply to them.
export function useWebullConnect(account: string, enabled: boolean) {
  const [status, setStatus] = useState<WebullSessionStatus>("")
  const [expiresAt, setExpiresAt] = useState<number | undefined>(undefined)
  const [daysRemaining, setDaysRemaining] = useState<number | undefined>(
    undefined,
  )
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState("")
  const [loaded, setLoaded] = useState(false)

  const applyStatus = useCallback(
    (resp: {
      status: WebullSessionStatus
      expires_at?: number
      days_remaining?: number
    }) => {
      setStatus(resp.status)
      setExpiresAt(resp.expires_at)
      setDaysRemaining(resp.days_remaining)
    },
    [],
  )

  const refresh = useCallback(async () => {
    if (!enabled) {
      return
    }
    try {
      const resp = await getWebullStatus(account)
      applyStatus(resp)
      setError("")
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoaded(true)
    }
  }, [account, applyStatus, enabled])

  // Initial load.
  useEffect(() => {
    if (!enabled) {
      return
    }
    setLoaded(false)
    void refresh()
  }, [refresh, enabled])

  // Poll while a login is awaiting in-app approval — from either this tab's
  // own Connect click or a webull_reconnect started in chat; both write to
  // the same shared session cache, so this picks up either.
  useEffect(() => {
    if (!enabled || status !== "PENDING") {
      return
    }

    let canceled = false
    const timer = setInterval(() => {
      if (!canceled) {
        void refresh()
      }
    }, STATUS_POLL_MS)

    return () => {
      canceled = true
      clearInterval(timer)
    }
  }, [status, refresh, enabled])

  const connectingRef = useRef(false)

  const connect = useCallback(async () => {
    if (connectingRef.current) {
      return
    }
    connectingRef.current = true
    setConnecting(true)
    setError("")
    try {
      const resp = await connectWebull(account)
      setStatus(resp.status)
      // Follow up with a full status fetch so expiry/days_remaining are
      // populated immediately on an already-NORMAL result.
      await refresh()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      connectingRef.current = false
      setConnecting(false)
    }
  }, [account, refresh])

  return {
    status,
    expiresAt,
    daysRemaining,
    connecting,
    error,
    loaded,
    connect,
  }
}
