// Pure helpers for the profile form: draft <-> profile conversion, client-side
// validation and the minimal patch to send. The server validates again and its
// message is shown if it disagrees.

import type { Profile, ProfileField, ProfilePatch } from "@/api/proactive"

// The allocation buckets the agent knows about; any other key already stored in
// the profile is kept and shown too.
export const STANDARD_TARGETS = ["crypto", "th_equity", "us_equity", "cash"] as const

export const LANGUAGES = ["th", "en"] as const
export const RISK_LEVELS = ["conservative", "moderate", "aggressive"] as const

export interface ProfileDraft {
  timezone: string
  hoursStart: string
  hoursEnd: string
  language: string
  risk: string
  targets: Record<string, string>
  avoid: string
  budget: string
  digestTime: string
}

export function draftFromProfile(p: Profile): ProfileDraft {
  const targets: Record<string, string> = {}
  for (const key of STANDARD_TARGETS) targets[key] = ""
  for (const [key, value] of Object.entries(p.allocation_targets ?? {})) {
    targets[key] = String(value)
  }
  return {
    timezone: p.timezone,
    hoursStart: p.active_hours.start,
    hoursEnd: p.active_hours.end,
    language: p.language,
    risk: p.risk_tolerance,
    targets,
    avoid: (p.assets_to_avoid ?? []).join(", "),
    budget: String(p.notification_budget_per_day),
    digestTime: p.digest_time,
  }
}

export function isValidTimezone(tz: string): boolean {
  if (tz.trim() === "") return false
  try {
    new Intl.DateTimeFormat("en", { timeZone: tz.trim() })
    return true
  } catch {
    return false
  }
}

let cachedZones: string[] | null = null

// timezoneOptions lists the IANA zones the browser knows, for the suggestion
// list. The field stays free text, so an unusual zone can still be typed.
export function timezoneOptions(): string[] {
  if (cachedZones) return cachedZones
  const zones = new Set<string>(["UTC", "Asia/Bangkok"])
  if (typeof Intl.supportedValuesOf === "function") {
    for (const z of Intl.supportedValuesOf("timeZone")) zones.add(z)
  }
  cachedZones = [...zones].sort()
  return cachedZones
}

export function parseAssets(raw: string): string[] {
  const seen = new Set<string>()
  for (const part of raw.split(/[\s,;]+/)) {
    const s = part.trim().toUpperCase()
    if (s) seen.add(s)
  }
  return [...seen]
}

// parseTargets reads the non-blank allocation inputs. A value that is not a
// number from 0 to 100 is reported through `invalid`.
export function parseTargets(targets: Record<string, string>): {
  values: Record<string, number>
  invalid: string[]
  sum: number
} {
  const values: Record<string, number> = {}
  const invalid: string[] = []
  let sum = 0
  for (const [key, raw] of Object.entries(targets)) {
    const s = raw.trim()
    if (s === "") continue
    const n = Number(s)
    if (!Number.isFinite(n) || n < 0 || n > 100) {
      invalid.push(key)
      continue
    }
    values[key] = n
    sum += n
  }
  // Avoid 0.1 + 0.2 style noise in the displayed total.
  return { values, invalid, sum: Math.round(sum * 1e6) / 1e6 }
}

export function parseBudget(raw: string): number | null {
  const s = raw.trim()
  return /^\d+$/.test(s) ? Number(s) : null
}

export type ProfileProblem =
  | "timezone"
  | "hours"
  | "targets_invalid"
  | "targets_sum"
  | "budget"
  | "digest_time"

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/

export function profileProblems(d: ProfileDraft): ProfileProblem[] {
  const problems: ProfileProblem[] = []
  if (!isValidTimezone(d.timezone)) problems.push("timezone")
  if (!HHMM.test(d.hoursStart) || !HHMM.test(d.hoursEnd)) problems.push("hours")
  const t = parseTargets(d.targets)
  if (t.invalid.length > 0) problems.push("targets_invalid")
  if (t.sum > 100) problems.push("targets_sum")
  if (parseBudget(d.budget) === null) problems.push("budget")
  if (!HHMM.test(d.digestTime)) problems.push("digest_time")
  return problems
}

function sameTargets(a: Record<string, number>, b: Record<string, number>): boolean {
  const ak = Object.keys(a).sort()
  const bk = Object.keys(b).sort()
  return ak.length === bk.length && ak.every((k, i) => k === bk[i] && a[k] === b[k])
}

function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

// buildProfilePatch returns only what differs from the saved profile. An emptied
// optional field is sent as a reset (back to its default), not as an empty value.
// An empty patch means there is nothing to save.
export function buildProfilePatch(saved: Profile, d: ProfileDraft): ProfilePatch {
  const patch: ProfilePatch = {}
  const reset: ProfileField[] = []

  const tz = d.timezone.trim()
  if (tz !== saved.timezone) patch.timezone = tz

  if (d.hoursStart !== saved.active_hours.start || d.hoursEnd !== saved.active_hours.end) {
    patch.active_hours = { start: d.hoursStart, end: d.hoursEnd }
  }

  if (d.language !== saved.language) {
    if (d.language === "") reset.push("language")
    else patch.language = d.language
  }
  if (d.risk !== saved.risk_tolerance) {
    if (d.risk === "") reset.push("risk_tolerance")
    else patch.risk_tolerance = d.risk
  }

  const targets = parseTargets(d.targets).values
  if (!sameTargets(targets, saved.allocation_targets ?? {})) {
    if (Object.keys(targets).length === 0) reset.push("allocation_targets")
    else patch.allocation_targets = targets
  }

  const avoid = parseAssets(d.avoid)
  if (!sameList(avoid, saved.assets_to_avoid ?? [])) {
    if (avoid.length === 0) reset.push("assets_to_avoid")
    else patch.assets_to_avoid = avoid
  }

  const budget = parseBudget(d.budget)
  if (budget !== null && budget !== saved.notification_budget_per_day) {
    patch.notification_budget_per_day = budget
  }

  if (d.digestTime !== saved.digest_time) patch.digest_time = d.digestTime

  if (reset.length > 0) patch.reset = reset
  return patch
}

// applyFieldReset copies the freshly saved value of one profile field into the
// draft, leaving the user's other unsaved edits alone.
export function applyFieldReset(
  draft: ProfileDraft,
  fresh: ProfileDraft,
  field: ProfileField,
): ProfileDraft {
  switch (field) {
    case "timezone":
      return { ...draft, timezone: fresh.timezone }
    case "active_hours":
      return { ...draft, hoursStart: fresh.hoursStart, hoursEnd: fresh.hoursEnd }
    case "language":
      return { ...draft, language: fresh.language }
    case "risk_tolerance":
      return { ...draft, risk: fresh.risk }
    case "allocation_targets":
      return { ...draft, targets: fresh.targets }
    case "assets_to_avoid":
      return { ...draft, avoid: fresh.avoid }
    case "notification_budget_per_day":
      return { ...draft, budget: fresh.budget }
    case "digest_time":
      return { ...draft, digestTime: fresh.digestTime }
    default:
      return draft
  }
}
