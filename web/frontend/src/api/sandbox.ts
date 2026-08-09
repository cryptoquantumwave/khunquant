// API client for sandbox mode configuration.

export interface FixtureEntry {
  method: string
  path_prefix: string
  query?: Record<string, string>
  response: {
    status: number
    // Go serializes this as json.RawMessage, so it arrives already parsed:
    // an object, array, or scalar — never a JSON string of the body.
    body?: unknown
    body_text?: string
    headers?: Record<string, string>
  }
}

export interface SandboxStatusResponse {
  enabled: boolean
  gateway_reachable?: boolean
  fixtures_dir?: string
  venues?: string[]
}

interface SandboxEnableResponse {
  status: string
}

const BASE_URL = ""

async function request<T>(
  path: string,
  options?: RequestInit,
): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, options)
  if (!res.ok) {
    throw new Error(`API error: ${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<T>
}

async function requestText(
  path: string,
  options?: RequestInit,
): Promise<string> {
  const res = await fetch(`${BASE_URL}${path}`, options)
  if (!res.ok) {
    throw new Error(`API error: ${res.status} ${res.statusText}`)
  }
  return res.text()
}

export async function getSandboxStatus(): Promise<SandboxStatusResponse> {
  return request<SandboxStatusResponse>("/api/sandbox/status")
}

export async function getFixtures(venue: string): Promise<FixtureEntry[]> {
  const rawText = await requestText(
    `/api/sandbox/fixtures?venue=${encodeURIComponent(venue)}`,
  )
  return JSON.parse(rawText) as FixtureEntry[]
}

export async function putFixtures(
  venue: string,
  fixtures: FixtureEntry[],
): Promise<SandboxEnableResponse> {
  // Important: serialize fixtures without reformatting to preserve body byte-for-byte
  const bodyText = JSON.stringify(fixtures)
  const res = await fetch(
    `${BASE_URL}/api/sandbox/fixtures?venue=${encodeURIComponent(venue)}`,
    {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
      },
      body: bodyText,
    },
  )
  if (!res.ok) {
    throw new Error(`API error: ${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<SandboxEnableResponse>
}

export async function reloadFixtures(): Promise<SandboxEnableResponse> {
  return request<SandboxEnableResponse>("/api/sandbox/reload", {
    method: "POST",
  })
}

export async function resetSandboxState(): Promise<SandboxEnableResponse> {
  return request<SandboxEnableResponse>("/api/sandbox/reset-state", {
    method: "POST",
  })
}

export type { SandboxEnableResponse }
