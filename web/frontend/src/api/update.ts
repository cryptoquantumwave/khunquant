// API client for update availability checks.

export interface UpdateStatus {
  is_outdated: boolean
  current_version: string
  latest_version: string
  release_url: string
}

async function request<T>(path: string): Promise<T> {
  const res = await fetch(path)
  if (!res.ok) {
    throw new Error(`API error: ${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<T>
}

export async function getUpdateStatus(): Promise<UpdateStatus> {
  return request<UpdateStatus>("/api/update/status")
}

export async function getVersion(): Promise<string> {
  const res = await request<{ version: string }>("/api/version")
  return res.version
}

export interface ApplyUpdateResult {
  success: boolean
  up_to_date?: boolean
  version: string
}

export async function applyUpdate(): Promise<ApplyUpdateResult> {
  const res = await fetch("/api/update/apply", { method: "POST" })
  if (!res.ok) {
    const text = await res.text()
    throw new Error(text || `HTTP ${res.status}`)
  }
  return res.json() as Promise<ApplyUpdateResult>
}
