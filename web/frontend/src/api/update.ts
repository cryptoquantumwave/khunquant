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
