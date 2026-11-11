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

export interface LabTimelineEntry {
  seq: number
  sim_time: string
  wall_time: string
  kind: string
  source: string
  subject: string
  summary: string
  // Any JSON value; a payload over 4 KiB arrives as a string with truncated set.
  payload: unknown
  truncated?: boolean
}

export interface LabTimelineResponse {
  locked: true
  instance: string
  entries: LabTimelineEntry[]
  next_seq: number
  drops: number
}

// Read-only. The gateway answers 404 when it is not a locked sandbox, and the launcher
// answers 502 (gateway not running or unreachable) or 503 (no view token): none of these
// is an error here, only "no locked sandbox reachable". The launcher adds the token;
// this page never holds one.
export async function getLabTimeline(p: {
  afterSeq: number
  kinds?: string
  limit?: number
}): Promise<LabTimelineResponse | { locked: false }> {
  const qs = new URLSearchParams({ after_seq: String(p.afterSeq) })
  if (p.kinds) qs.set("kinds", p.kinds)
  if (p.limit) qs.set("limit", String(p.limit))
  const res = await fetch(`${BASE_URL}/api/sandbox/timeline?${qs}`)
  if ([404, 502, 503].includes(res.status)) return { locked: false }
  if (!res.ok) {
    throw new Error(`API error: ${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<LabTimelineResponse>
}

export type { SandboxEnableResponse }
