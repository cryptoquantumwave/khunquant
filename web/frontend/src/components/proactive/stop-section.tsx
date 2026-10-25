import { IconLoader2, IconPlayerStopFilled } from "@tabler/icons-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import { stopAgent } from "@/api/proactive"
import { getStopState, resumeAgent } from "@/api/proactive-dashboard"
import { ManageError } from "@/api/proactive-manage"
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

const STOP_STATE_KEY = ["proactive", "stop-state"] as const

// StopSection is the red bar at the top of every Proactive view: it stays in view while
// the page scrolls. It stops everything at once, and shows Resume while stopped. Resume
// lets the assistant act again, so it needs the web to be the owner: the button is
// disabled, with the reason, when it is not.
export function StopSection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [confirm, setConfirm] = useState(false)
  const [summary, setSummary] = useState("")

  const state = useQuery({
    queryKey: STOP_STATE_KEY,
    queryFn: getStopState,
    refetchInterval: 5_000,
  })
  const killed = state.data?.available === true && state.data.killed === true
  const canResume = state.data?.web_owner_active === true
  const reason = state.data?.web_owner_missing ?? ""

  const refresh = () =>
    void queryClient.invalidateQueries({ queryKey: STOP_STATE_KEY })

  const stop = useMutation({
    mutationFn: stopAgent,
    onSuccess: (res) => {
      setConfirm(false)
      setSummary(res.text)
      toast.success(t("pages.agent.proactive.stop.done"))
    },
    onError: (err: Error) => {
      setConfirm(false)
      toast.error(t("pages.agent.proactive.stop.error"), {
        description: err.message,
      })
    },
    onSettled: refresh,
  })

  const resume = useMutation({
    mutationFn: resumeAgent,
    onSuccess: () => {
      setSummary("")
      toast.success(t("pages.agent.proactive.stop.resumed"))
    },
    onError: (err: Error) => {
      const missing =
        err instanceof ManageError ? err.webOwnerMissing : undefined
      toast.error(t("pages.agent.proactive.stop.resume_error"), {
        description:
          missing !== undefined
            ? t("pages.agent.proactive.manage.web_owner_blocked", {
                reason: missing,
              })
            : err.message,
      })
    },
    onSettled: refresh,
  })

  return (
    <section
      className="sticky top-0 z-10 rounded-lg border border-red-300 bg-red-50 px-4 py-3 shadow-sm dark:border-red-500/40 dark:bg-red-950"
      aria-label={t("pages.agent.proactive.stop.title")}
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <p className="text-sm font-semibold text-red-800 dark:text-red-200">
            {killed
              ? t("pages.agent.proactive.stop.stopped_title")
              : t("pages.agent.proactive.stop.title")}
          </p>
          <p className="text-xs text-red-800/80 dark:text-red-200/80">
            {killed
              ? t("pages.agent.proactive.stop.stopped_description")
              : t("pages.agent.proactive.stop.description")}
          </p>
          {killed && !canResume && (
            <p className="mt-1 text-xs text-red-800 dark:text-red-200">
              {t("pages.agent.proactive.stop.resume_blocked", { reason })}
            </p>
          )}
        </div>
        <div className="flex shrink-0 gap-2">
          {killed && (
            <Button
              variant="outline"
              disabled={!canResume || resume.isPending}
              title={!canResume ? reason : undefined}
              onClick={() => resume.mutate()}
            >
              {resume.isPending && (
                <IconLoader2 className="size-4 animate-spin" />
              )}
              {t("pages.agent.proactive.stop.resume")}
            </Button>
          )}
          <Button
            variant="destructive"
            disabled={stop.isPending}
            onClick={() => setConfirm(true)}
          >
            {stop.isPending ? (
              <IconLoader2 className="size-4 animate-spin" />
            ) : (
              <IconPlayerStopFilled className="size-4" />
            )}
            {t("pages.agent.proactive.stop.button")}
          </Button>
        </div>
      </div>
      {summary !== "" && (
        <pre className="mt-2 max-h-40 overflow-auto rounded-lg border border-red-200 bg-white/60 p-3 text-xs whitespace-pre-wrap dark:bg-black/20">
          {summary}
        </pre>
      )}
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("pages.agent.proactive.stop.confirm_title")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("pages.agent.proactive.stop.confirm_description")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>
              {t("pages.agent.proactive.stop.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction onClick={() => stop.mutate()}>
              {t("pages.agent.proactive.stop.confirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  )
}
