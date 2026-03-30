import { IconDownload, IconX } from "@tabler/icons-react"
import * as React from "react"

import { useUpdateCheck } from "@/hooks/use-update-check"

const DISMISS_KEY = "update-banner-dismissed"

export function UpdateBanner() {
  const update = useUpdateCheck()
  const [dismissed, setDismissed] = React.useState<boolean>(() => {
    try {
      return localStorage.getItem(DISMISS_KEY) !== null
    } catch {
      return false
    }
  })

  // Reset dismiss state when a new version becomes available so the banner
  // reappears after a previously dismissed version is superseded.
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

  return (
    <div className="flex items-center justify-between gap-2 bg-blue-600 px-4 py-2 text-sm text-white">
      <div className="flex items-center gap-2">
        <IconDownload className="size-4 shrink-0" />
        <span>
          New version available:{" "}
          <span className="font-semibold">{update.latest_version}</span>
          {update.current_version && (
            <span className="opacity-75"> (current: {update.current_version})</span>
          )}
        </span>
        <a
          href={update.release_url}
          target="_blank"
          rel="noreferrer"
          className="ml-2 rounded bg-white/20 px-2 py-0.5 font-medium hover:bg-white/30"
        >
          Download
        </a>
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
