import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import { ManageError } from "@/api/proactive-manage"

// How often the three tabs refresh. React Query pauses a refetch interval while the
// browser tab is hidden, so nothing polls in the background.
export const MANAGE_REFRESH_MS = 10_000

export const MANAGE_QUERY_KEY = ["proactive", "manage"] as const

export function formatTime(iso: string | null | undefined): string {
  if (!iso) return "-"
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

// useNow re-renders its component every second, for a countdown.
export function useNow(): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(id)
  }, [])
  return now
}

export function formatCountdown(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const two = (n: number) => String(n).padStart(2, "0")
  return h > 0 ? `${h}:${two(m)}:${two(sec)}` : `${m}:${two(sec)}`
}

// useManageAction runs one action, refreshes the three tabs and says what happened.
export function useManageAction<A>(run: (arg: A) => Promise<unknown>) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: run,
    onSuccess: () => {
      toast.success(t("pages.agent.proactive.manage.done"))
    },
    onError: (err: Error) => {
      const missing =
        err instanceof ManageError ? err.webOwnerMissing : undefined
      toast.error(t("pages.agent.proactive.manage.failed"), {
        description:
          missing !== undefined
            ? t("pages.agent.proactive.manage.web_owner_blocked", {
                reason: missing,
              })
            : err.message,
      })
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: MANAGE_QUERY_KEY })
    },
  })
}
