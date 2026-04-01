import { IconDownload, IconLoader2, IconX } from "@tabler/icons-react"
import * as React from "react"
import { toast } from "sonner"

import { applyUpdate } from "@/api/update"
import { useUpdateCheck } from "@/hooks/use-update-check"

const DISMISS_KEY = "update-banner-dismissed"

type UpdateState = "idle" | "updating" | "launcher-restarting" | "error"

async function pollUntilReachable(timeoutMs = 30_000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 1000))
    try {
      const res = await fetch("/api/update/status")
      if (res.ok) {
        window.location.reload()
        return
      }
    } catch {
      // server not yet up — keep polling
    }
  }
  window.location.reload()
}

export function UpdateBanner() {
  const { status: update, refetch } = useUpdateCheck()
  const [dismissed, setDismissed] = React.useState<boolean>(() => {
    try {
      return localStorage.getItem(DISMISS_KEY) !== null
    } catch {
      return false
    }
  })
  const [state, setState] = React.useState<UpdateState>("idle")
  const [errorMsg, setErrorMsg] = React.useState<string>("")

  // Reset dismiss when a newer version supersedes the previously dismissed one.
  React.useEffect(() => {
    if (!update) return
    const prev = localStorage.getItem(DISMISS_KEY)
    if (prev && prev !== update.latest_version) {
      localStorage.removeItem(DISMISS_KEY)
      setDismissed(false)
    }
  }, [update])

  if (!update?.is_outdated || dismissed) return null

  const handleDismiss = () => {
    try {
      localStorage.setItem(DISMISS_KEY, update.latest_version)
    } catch {
      // ignore
    }
    setDismissed(true)
  }

  const handleUpdate = async () => {
    setState("updating")
    setErrorMsg("")
    try {
      const result = await applyUpdate()
      if (result.launcher_updated === true) {
        // The launcher process is restarting with a new binary that includes
        // the updated web UI. Poll until the new server responds, then reload.
        setState("launcher-restarting")
        await pollUntilReachable(30_000)
        return
      }
      toast.success(`Updated to ${result.version} — gateway is restarting…`)
      refetch()
      setState("idle")
    } catch (err) {
      setState("error")
      setErrorMsg(err instanceof Error ? err.message : String(err))
    }
  }

  return (
    <div className="flex items-center justify-between gap-2 bg-blue-600 px-4 py-2 text-sm text-white">
      <div className="flex items-center gap-2 flex-wrap">
        <IconDownload className="size-4 shrink-0" />
        <span>
          New version available:{" "}
          <span className="font-semibold">{update.latest_version}</span>
          {update.current_version && (
            <span className="opacity-75"> (current: {update.current_version})</span>
          )}
        </span>

        {state === "error" ? (
          <>
            <span className="opacity-90 text-red-200">{errorMsg}</span>
            <a
              href={update.release_url}
              target="_blank"
              rel="noreferrer"
              className="ml-1 rounded bg-white/20 px-2 py-0.5 font-medium hover:bg-white/30"
            >
              Download manually
            </a>
          </>
        ) : (
          <button
            onClick={handleUpdate}
            disabled={state === "updating" || state === "launcher-restarting"}
            className="ml-2 flex items-center gap-1 rounded bg-white/20 px-2 py-0.5 font-medium hover:bg-white/30 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {state === "launcher-restarting" ? (
              <>
                <IconLoader2 className="size-3 animate-spin" />
                Reconnecting…
              </>
            ) : state === "updating" ? (
              <>
                <IconLoader2 className="size-3 animate-spin" />
                Updating…
              </>
            ) : (
              "Update"
            )}
          </button>
        )}
      </div>

      <button
        onClick={handleDismiss}
        aria-label="Dismiss update banner"
        className="rounded p-0.5 hover:bg-white/20"
      >
        <IconX className="size-4" />
      </button>
    </div>
  )
}
