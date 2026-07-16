// API client for the Webull re-authentication flow — a web-UI alternative
// to the chat webull_reconnect tool. Both share the same on-disk session
// cache on the backend, so approving via either surface satisfies both.

export type WebullSessionStatus =
  | ""
  | "NORMAL"
  | "PENDING"
  | "INVALID"
  | "EXPIRED"

export interface WebullConnectResponse {
  status: WebullSessionStatus
}

export interface WebullStatusResponse {
  status: WebullSessionStatus
  expires_at?: number
  days_remaining?: number
}

const BASE_URL = ""

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, options)
  if (!res.ok) {
    let message = `API error: ${res.status} ${res.statusText}`
    try {
      const text = (await res.text()).trim()
      if (text) {
        message = text
      }
    } catch {
      // Keep default fallback message if response body can't be read.
    }
    throw new Error(message)
  }
  return res.json() as Promise<T>
}

export async function connectWebull(
  account: string,
): Promise<WebullConnectResponse> {
  return request<WebullConnectResponse>(
    `/api/exchanges/webull/${encodeURIComponent(account)}/connect`,
    { method: "POST" },
  )
}

export async function getWebullStatus(
  account: string,
): Promise<WebullStatusResponse> {
  return request<WebullStatusResponse>(
    `/api/exchanges/webull/${encodeURIComponent(account)}/status`,
  )
}
