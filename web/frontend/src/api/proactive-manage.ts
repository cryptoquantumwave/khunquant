// API client for the Proactive page's Intents, Proposals and Mandates tabs.
//
// The launcher proxies these routes to the gateway, which owns the data and decides
// every action. Text written by a model (an intent's title, a proposal's quote) is
// data: render it as plain text only.

const BASE = "/api/agent/proactive"

export class ManageError extends Error {
  status: number
  // Set on a 403 that means "the web launcher is not the owner right now".
  webOwnerMissing?: string

  constructor(message: string, status: number, webOwnerMissing?: string) {
    super(message)
    this.status = status
    this.webOwnerMissing = webOwnerMissing
  }
}

export async function manageRequest<T>(
  path: string,
  method = "GET",
  body?: unknown,
): Promise<T> {
  let res: Response
  try {
    res = await fetch(`${BASE}${path}`, {
      method,
      ...(body === undefined
        ? {}
        : {
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
          }),
    })
  } catch {
    throw new ManageError("network error", 0)
  }
  if (!res.ok) {
    let message = `API error: ${res.status} ${res.statusText}`
    let missing: string | undefined
    try {
      const err = (await res.json()) as {
        error?: string
        web_owner_missing?: string
      }
      if (typeof err.error === "string" && err.error.trim() !== "") {
        message = err.error
      }
      if (res.status === 403 && typeof err.web_owner_missing === "string") {
        missing = err.web_owner_missing
      }
    } catch {
      // not a JSON error body; keep the status line
    }
    throw new ManageError(message, res.status, missing)
  }
  return res.json() as Promise<T>
}

// What every read carries. When `available` is false the gateway has no such
// service (proactive off, approvals off, ...) and `reason` says why.
export interface ManageEnvelope {
  available: boolean
  reason?: string
  server_time?: string
}

// Whether the web launcher may take risk-increasing actions (approve, reject,
// confirm, resume) right now, and what is missing when it may not.
export interface WebOwnerState {
  web_owner_active?: boolean
  web_owner_missing?: string
}

export interface IntentRow {
  id: string
  type: string
  title: string
  status: string
  source: string
  turn_kind: string
  via_web: boolean
  triggers: number
  fires_used: number
  fire_budget: number
  created_at: string
  expires_at: string | null
}

export interface IntentsResponse extends ManageEnvelope, WebOwnerState {
  intents?: IntentRow[]
}

export interface ProposalRow {
  code: string
  kind: string
  status: string
  summary: string
  reducing: boolean
  created_at: string
  expires_at: string
  expires_in_s: number
  quote?: string
  quote_kind?: string
}

export interface ProposalsResponse extends ManageEnvelope, WebOwnerState {
  proposals?: ProposalRow[]
}

export interface MandateRow {
  id: string
  symbol: string
  side: string
  provider: string
  account: string
  status: string
  period: string
  max_per_period_usd: number
  max_per_run_usd: number
  used_usd: number
  usage_known: boolean
  valid_from: string
  expires_at: string | null
}

export interface MandatesResponse extends ManageEnvelope {
  mandates?: MandateRow[]
}

export type IntentAction = "pause" | "resume" | "cancel" | "confirm"

export function getIntents(): Promise<IntentsResponse> {
  return manageRequest<IntentsResponse>("/intents")
}

export function getProposals(): Promise<ProposalsResponse> {
  return manageRequest<ProposalsResponse>("/proposals")
}

export function getMandates(): Promise<MandatesResponse> {
  return manageRequest<MandatesResponse>("/mandates")
}

export function actOnIntent(
  id: string,
  action: IntentAction,
): Promise<unknown> {
  return manageRequest(`/intents/${encodeURIComponent(id)}/${action}`, "POST")
}

export function decideProposal(
  code: string,
  action: "approve" | "reject",
): Promise<{ text?: string }> {
  return manageRequest(
    `/proposals/${encodeURIComponent(code)}/${action}`,
    "POST",
  )
}

export function revokeMandate(id: string): Promise<unknown> {
  return manageRequest(`/mandates/${encodeURIComponent(id)}/revoke`, "POST")
}
