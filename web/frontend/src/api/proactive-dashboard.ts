// API client for the Proactive dashboard: the interventions timeline, the metrics, the
// autonomy dial, the mutes and the stop state. The launcher proxies each of these to the
// gateway, which owns the data. Any string a model or a person wrote (a title, a mute's
// key) is data: render it as plain text only.
import {
  type ManageEnvelope,
  type WebOwnerState,
  manageRequest,
} from "@/api/proactive-manage"

export interface FeedbackResponse {
  kind: string
  value: string
}

// A missing value is null: nothing reliable was found, which is not zero.
export interface HindsightRow {
  horizon: string
  pct_change: number | null
  portfolio_pct: number | null
  trades: number | null
}

export interface Intervention {
  id: string
  at: string
  trigger_kind: string
  severity: number
  severity_name: string
  disposition: string
  reason: string
  title: string
  delivered: boolean
  shadow: boolean
  holdout: boolean
  responses: FeedbackResponse[]
  hindsight: HindsightRow[]
}

export interface InterventionsResponse extends ManageEnvelope {
  items?: Intervention[]
  next_cursor?: string
}

export interface InterventionFilter {
  from?: string
  to?: string
  disposition?: string
  trigger?: string
  cursor?: string
  limit?: number
}

export function getInterventions(
  f: InterventionFilter,
): Promise<InterventionsResponse> {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(f)) {
    if (v !== undefined && v !== "") q.set(k, String(v))
  }
  const qs = q.toString()
  return manageRequest<InterventionsResponse>(
    `/interventions${qs ? `?${qs}` : ""}`,
  )
}

// A metric with no data has value null and says why; it is never 0.
export interface Metric<T> {
  value: T | null
  reason?: string
}

export interface SendsPerDay {
  total: number
  average: number
  series: { day: string; sends: number }[]
}

export interface ApprovalRate {
  group: string
  approved: number
  rejected: number
  expired: number
  pending: number
  rate: number | null
}

export interface HarmMetric {
  window_days: number
  alert_days: number
  quiet_days: number
  post_alert_trades: number
  quiet_trades: number
  post_alert_contract_trades: number
  quiet_contract_trades: number
  post_alert_per_alert_day: number | null
  quiet_per_quiet_day: number | null
  post_alert_contract_per_alert_day: number | null
  quiet_contract_per_quiet_day: number | null
}

export interface DashboardMetrics {
  days: number
  zone: string
  from: string
  to: string
  sends_per_day: Metric<SendsPerDay>
  precision_at_sent: Metric<number>
  not_useful_rate: Metric<number>
  mute_rate: Metric<number>
  no_response_rate: Metric<number>
  responses_by_kind: Metric<Record<string, number>>
  critical_recall: Metric<number>
  median_queue_latency_seconds: Metric<number>
  llm_usage_per_day: Metric<{ day: string; wakes: number; tokens: number }[]>
  post_alert_trading: Metric<HarmMetric>
  approval_rates: Metric<ApprovalRate[]>
  learning_per_day: Metric<
    { day: string; low_acceptance: number; holdout: number }[]
  >
}

export interface MetricsResponse extends ManageEnvelope {
  metrics?: DashboardMetrics
}

export function getMetrics(days: number): Promise<MetricsResponse> {
  return manageRequest<MetricsResponse>(`/metrics?days=${days}`)
}

// ---- stop state ------------------------------------------------------------

export interface StopState extends ManageEnvelope, WebOwnerState {
  killed?: boolean
}

export function getStopState(): Promise<StopState> {
  return manageRequest<StopState>("/state")
}

// Clears the kill switch. The gateway answers 403 with the missing condition when the
// web is not the owner.
export function resumeAgent(): Promise<unknown> {
  return manageRequest("/resume", "POST")
}

// ---- autonomy dial ----------------------------------------------------------

export type DialSetting = "default" | "ask" | "notify" | "off"

export interface DialColumn {
  column: string
  label: string
  default: string
  default_text: string
  effective: string
  effective_text: string
}

export interface DialTier {
  tier: string
  name: string
  about: string
  setting: string
  columns: DialColumn[]
}

export interface AutonomyResponse extends ManageEnvelope, WebOwnerState {
  policy_mode?: string
  tiers?: DialTier[]
  settings?: string[]
  note?: string
}

export function getAutonomy(): Promise<AutonomyResponse> {
  return manageRequest<AutonomyResponse>("/autonomy")
}

export function putAutonomy(
  change: Record<string, DialSetting>,
): Promise<AutonomyResponse> {
  return manageRequest<AutonomyResponse>("/autonomy", "PUT", change)
}

// ---- mutes -------------------------------------------------------------------

export interface MuteRow {
  id: string
  kind: "prefix" | "exact"
  key: string
  reason: string
  until: string
  created: string
}

export interface MutesResponse extends ManageEnvelope {
  mutes?: MuteRow[]
}

export function getMutes(): Promise<MutesResponse> {
  return manageRequest<MutesResponse>("/mutes")
}

export function unmute(id: string): Promise<unknown> {
  return manageRequest(`/mutes/${encodeURIComponent(id)}`, "DELETE")
}
