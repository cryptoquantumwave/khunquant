export type JsonRecord = Record<string, unknown>

export interface CoreConfigForm {
  workspace: string
  restrictToWorkspace: boolean
  allowRemote: boolean
  temperature: string
  maxTokens: string
  maxToolIterations: string
  summarizeMessageThreshold: string
  summarizeTokenPercent: string
  contextManager: string
  dmScope: string
  heartbeatEnabled: boolean
  heartbeatInterval: string
  devicesEnabled: boolean
  monitorUSB: boolean
  followUpNudge: boolean
  injectFinancialContext: boolean
  financialContextTtlMinutes: string
  maxContextAssets: string
  maxContextDcaPlans: string
  maxContextDnPlans: string
  allowLeverage: boolean
  paperTradingMode: boolean
  debugDevMcpEnabled: boolean
}

export const CONTEXT_MANAGER_OPTIONS = [
  {
    value: "legacy",
    labelKey: "pages.config.context_manager_legacy",
    labelDefault: "Legacy (Summarization)",
    descKey: "pages.config.context_manager_legacy_desc",
    descDefault: "Summarizes old messages when context is full.",
  },
  {
    value: "seahorse",
    labelKey: "pages.config.context_manager_seahorse",
    labelDefault: "Seahorse (SQLite DAG)",
    descKey: "pages.config.context_manager_seahorse_desc",
    descDefault: "Budget-aware SQLite-backed memory with lossless compression.",
  },
] as const

export interface LauncherForm {
  port: string
  publicAccess: boolean
  allowedCIDRsText: string
}

export const DM_SCOPE_OPTIONS = [
  {
    value: "per-channel-peer",
    labelKey: "pages.config.session_scope_per_channel_peer",
    labelDefault: "Per Channel + Peer",
    descKey: "pages.config.session_scope_per_channel_peer_desc",
    descDefault: "Separate context for each user in each channel.",
  },
  {
    value: "per-channel",
    labelKey: "pages.config.session_scope_per_channel",
    labelDefault: "Per Channel",
    descKey: "pages.config.session_scope_per_channel_desc",
    descDefault: "One shared context per channel.",
  },
  {
    value: "per-peer",
    labelKey: "pages.config.session_scope_per_peer",
    labelDefault: "Per Peer",
    descKey: "pages.config.session_scope_per_peer_desc",
    descDefault: "One context per user across channels.",
  },
  {
    value: "global",
    labelKey: "pages.config.session_scope_global",
    labelDefault: "Global",
    descKey: "pages.config.session_scope_global_desc",
    descDefault: "All messages share one global context.",
  },
] as const

export const EMPTY_FORM: CoreConfigForm = {
  workspace: "",
  restrictToWorkspace: true,
  allowRemote: true,
  temperature: "0.7",
  maxTokens: "32768",
  maxToolIterations: "50",
  summarizeMessageThreshold: "20",
  summarizeTokenPercent: "75",
  contextManager: "seahorse",
  dmScope: "per-channel-peer",
  heartbeatEnabled: true,
  heartbeatInterval: "30",
  devicesEnabled: false,
  monitorUSB: true,
  followUpNudge: false,
  injectFinancialContext: false,
  financialContextTtlMinutes: "30",
  maxContextAssets: "5",
  maxContextDcaPlans: "3",
  maxContextDnPlans: "3",
  allowLeverage: true,
  paperTradingMode: false,
  debugDevMcpEnabled: false,
}

export const EMPTY_LAUNCHER_FORM: LauncherForm = {
  port: "18800",
  publicAccess: false,
  allowedCIDRsText: "",
}

function asRecord(value: unknown): JsonRecord {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return value as JsonRecord
  }
  return {}
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : ""
}

function asBool(value: unknown): boolean {
  return value === true
}

function asNumberString(value: unknown, fallback: string): string {
  if (typeof value === "number" && Number.isFinite(value)) {
    return String(value)
  }
  if (typeof value === "string" && value.trim() !== "") {
    return value
  }
  return fallback
}

export function buildFormFromConfig(config: unknown): CoreConfigForm {
  const root = asRecord(config)
  const agents = asRecord(root.agents)
  const defaults = asRecord(agents.defaults)
  const session = asRecord(root.session)
  const heartbeat = asRecord(root.heartbeat)
  const devices = asRecord(root.devices)
  const tools = asRecord(root.tools)
  const exec = asRecord(tools.exec)
  const tradingRisk = asRecord(root.trading_risk)
  const debug = asRecord(root.debug)
  const devMcp = asRecord(debug.dev_mcp)
  return {
    workspace: asString(defaults.workspace) || EMPTY_FORM.workspace,
    restrictToWorkspace:
      defaults.restrict_to_workspace === undefined
        ? EMPTY_FORM.restrictToWorkspace
        : asBool(defaults.restrict_to_workspace),
    allowRemote:
      exec.allow_remote === undefined
        ? EMPTY_FORM.allowRemote
        : asBool(exec.allow_remote),
    temperature: asNumberString(defaults.temperature, EMPTY_FORM.temperature),
    maxTokens: asNumberString(defaults.max_tokens, EMPTY_FORM.maxTokens),
    maxToolIterations: asNumberString(
      defaults.max_tool_iterations,
      EMPTY_FORM.maxToolIterations,
    ),
    summarizeMessageThreshold: asNumberString(
      defaults.summarize_message_threshold,
      EMPTY_FORM.summarizeMessageThreshold,
    ),
    summarizeTokenPercent: asNumberString(
      defaults.summarize_token_percent,
      EMPTY_FORM.summarizeTokenPercent,
    ),
    contextManager:
      asString(defaults.context_manager) || EMPTY_FORM.contextManager,
    dmScope: asString(session.dm_scope) || EMPTY_FORM.dmScope,
    heartbeatEnabled:
      heartbeat.enabled === undefined
        ? EMPTY_FORM.heartbeatEnabled
        : asBool(heartbeat.enabled),
    heartbeatInterval: asNumberString(
      heartbeat.interval,
      EMPTY_FORM.heartbeatInterval,
    ),
    devicesEnabled:
      devices.enabled === undefined
        ? EMPTY_FORM.devicesEnabled
        : asBool(devices.enabled),
    monitorUSB:
      devices.monitor_usb === undefined
        ? EMPTY_FORM.monitorUSB
        : asBool(devices.monitor_usb),
    followUpNudge:
      defaults.follow_up_nudge === undefined
        ? EMPTY_FORM.followUpNudge
        : asBool(defaults.follow_up_nudge),
    injectFinancialContext:
      defaults.inject_financial_context === undefined
        ? EMPTY_FORM.injectFinancialContext
        : asBool(defaults.inject_financial_context),
    financialContextTtlMinutes: asNumberString(
      defaults.financial_context_ttl_minutes,
      EMPTY_FORM.financialContextTtlMinutes,
    ),
    maxContextAssets: asNumberString(
      defaults.max_context_assets,
      EMPTY_FORM.maxContextAssets,
    ),
    maxContextDcaPlans: asNumberString(
      defaults.max_context_dca_plans,
      EMPTY_FORM.maxContextDcaPlans,
    ),
    maxContextDnPlans: asNumberString(
      defaults.max_context_dn_plans,
      EMPTY_FORM.maxContextDnPlans,
    ),
    allowLeverage:
      tradingRisk.allow_leverage === undefined
        ? EMPTY_FORM.allowLeverage
        : asBool(tradingRisk.allow_leverage),
    paperTradingMode:
      tradingRisk.paper_trading_mode === undefined
        ? EMPTY_FORM.paperTradingMode
        : asBool(tradingRisk.paper_trading_mode),
    debugDevMcpEnabled:
      devMcp.enabled === undefined
        ? EMPTY_FORM.debugDevMcpEnabled
        : asBool(devMcp.enabled),
  }
}

export function parseIntField(
  rawValue: string,
  label: string,
  options: { min?: number; max?: number } = {},
): number {
  const value = Number(rawValue)
  if (!Number.isInteger(value)) {
    throw new Error(`${label} must be an integer.`)
  }
  if (options.min !== undefined && value < options.min) {
    throw new Error(`${label} must be >= ${options.min}.`)
  }
  if (options.max !== undefined && value > options.max) {
    throw new Error(`${label} must be <= ${options.max}.`)
  }
  return value
}

export function parseCIDRText(raw: string): string[] {
  if (!raw.trim()) {
    return []
  }
  return raw
    .split(/[\n,]/)
    .map((v) => v.trim())
    .filter((v) => v.length > 0)
}
