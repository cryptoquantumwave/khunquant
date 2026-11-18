// API client for the proactive agent settings: owner, modes, profile and the
// recent attention decisions. Full owner IDs are never returned by the server;
// the status carries masked values only.

export interface ProactiveOwnerView {
  channel: string
  chat_id_masked: string
  sender_id_masked: string
}

export interface ProactiveStatus {
  enabled: boolean
  owner_configured: boolean
  owner: ProactiveOwnerView
  effective: {
    heartbeat_mode: string
    attention_mode: string
    triggers_mode: string
    policy_mode: string
    can_deliver: boolean
  }
  configured: {
    heartbeat_mode: string
    attention_mode: string
    daily_budget: number
    digest: boolean
  }
  downgrades: string[]
  validation_error: string
  subsystems: Subsystems
  // trading_risk.paper_trading_mode, read-only here; it is set on the Config page.
  paper_trading_mode: boolean
}

export interface PerceptionSettings {
  enabled: boolean
  market_poll_seconds: number
  reads_per_minute_limit: number
  snapshot_minutes: number
}

export interface CognitionSettings {
  enabled: boolean
  max_concurrent_llm: number
  wakes_per_hour: number
  tokens_per_day: number
  proactive_model: string
}

export interface NewsSettings {
  enabled: boolean
  interval_minutes: number
  max_assets: number
  max_parses_per_run: number
  max_parses_per_day: number
  run_seconds: number
}

export interface Subsystems {
  configured: {
    perception: PerceptionSettings
    triggers: { mode: string }
    approvals: { enabled: boolean; expiry_minutes: number }
    cognition: CognitionSettings
    news: NewsSettings
  }
  effective: {
    perception: boolean
    triggers_mode: string
    approvals: boolean
    cognition: boolean
    news: boolean
  }
  limits: {
    news_min_interval_minutes: number
    news_max_assets_cap: number
  }
  // Codes of unmet requirements, for switches that are on: proactive_off,
  // news_needs_cognition, news_needs_triggers, news_needs_web_search,
  // triggers_need_perception.
  notes: string[]
}

// A partial update; a missing group or field is left as it is.
export interface SubsystemsInput {
  perception?: Partial<PerceptionSettings>
  triggers?: { mode?: string }
  approvals?: { enabled?: boolean; expiry_minutes?: number }
  cognition?: Partial<CognitionSettings>
  news?: Partial<NewsSettings>
}

export interface OwnerInput {
  channel: string
  chat_id: string
  sender_id: string
}

export interface ModesInput {
  enabled?: boolean
  heartbeat_mode?: string
  attention_mode?: string
  daily_budget?: number
  digest?: boolean
}

export interface ProfileHours {
  start: string
  end: string
}

export interface Profile {
  timezone: string
  active_hours: ProfileHours
  language: string
  risk_tolerance: string
  horizon: string
  primary_goal: string
  allocation_targets: Record<string, number>
  assets_to_avoid: string[]
  notification_budget_per_day: number
  digest_time: string
}

export type ProfileField = keyof Profile

export interface ProfileState {
  has_profile: boolean
  version: number
  updated_at: string
  updated_by: string
  profile: Profile
}

// A partial update: the fields to set, plus the names of fields to put back to
// their default (or empty) in `reset`.
export type ProfilePatch = Partial<Profile> & { reset?: ProfileField[] }

export interface ProactiveDecision {
  id: string
  at: string
  dedup_key: string
  trigger_id: string
  severity: number
  severity_name: string
  disposition: string
  reason: string
  shadow: boolean
  delivered: boolean
  title: string
}

const BASE = "/api/agent/proactive"

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, options)
  if (!res.ok) {
    let message = `API error: ${res.status} ${res.statusText}`
    try {
      const body = (await res.json()) as { error?: string }
      if (typeof body.error === "string" && body.error.trim() !== "") {
        message = body.error
      }
    } catch {
      // not a JSON error body; keep the status line
    }
    throw new Error(message)
  }
  return res.json() as Promise<T>
}

function sendJSON<T>(path: string, method: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
}

export function getProactiveStatus(): Promise<ProactiveStatus> {
  return request<ProactiveStatus>("/status")
}

export function putProactiveOwner(owner: OwnerInput): Promise<ProactiveStatus> {
  return sendJSON<ProactiveStatus>("/owner", "PUT", owner)
}

export function clearProactiveOwner(): Promise<ProactiveStatus> {
  return request<ProactiveStatus>("/owner", { method: "DELETE" })
}

export function putProactiveModes(modes: ModesInput): Promise<ProactiveStatus> {
  return sendJSON<ProactiveStatus>("/modes", "PUT", modes)
}

export function putProactiveSubsystems(
  input: SubsystemsInput,
): Promise<ProactiveStatus> {
  return sendJSON<ProactiveStatus>("/subsystems", "PUT", input)
}

export function getProactiveProfile(): Promise<ProfileState> {
  return request<ProfileState>("/profile")
}

export function putProactiveProfile(patch: ProfilePatch): Promise<ProfileState> {
  return sendJSON<ProfileState>("/profile", "PUT", patch)
}

export function getProactiveDecisions(limit = 50): Promise<ProactiveDecision[]> {
  return request<ProactiveDecision[]>(`/decisions?limit=${limit}`)
}

export interface StopResult {
  killed: boolean
  changed: boolean
  // The same text /stop answers in chat.
  text: string
}

// Runs /stop on the gateway: engages the kill flag and cancels running turns,
// workflows, mandates, pending proposals and the agent's open orders.
export async function stopAgent(): Promise<StopResult> {
  const res = await fetch("/api/control/kill", { method: "POST" })
  if (!res.ok) {
    let message = `API error: ${res.status} ${res.statusText}`
    try {
      const body = (await res.json()) as { error?: string }
      if (typeof body.error === "string" && body.error.trim() !== "") {
        message = body.error
      }
    } catch {
      // not a JSON error body; keep the status line
    }
    throw new Error(message)
  }
  return res.json() as Promise<StopResult>
}
