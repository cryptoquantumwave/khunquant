import { useEffect, useState } from "react"

import { type UpdateStatus, getUpdateStatus } from "@/api/update"

const POLL_INTERVAL_MS = 5 * 60 * 1000 // 5 minutes

export function useUpdateCheck(): UpdateStatus | null {
  const [status, setStatus] = useState<UpdateStatus | null>(null)

  useEffect(() => {
    const fetch = () => {
      getUpdateStatus()
        .then((data) => {
          if (data.is_outdated) setStatus(data)
        })
        .catch(() => {
          // silently ignore network errors
        })
    }

    fetch()
    const id = window.setInterval(fetch, POLL_INTERVAL_MS)
    return () => window.clearInterval(id)
  }, [])

  return status
}
