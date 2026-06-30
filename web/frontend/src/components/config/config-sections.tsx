import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"

import {
  CONTEXT_MANAGER_OPTIONS,
  type CoreConfigForm,
  DM_SCOPE_OPTIONS,
  type LauncherForm,
} from "@/components/config/form-model"
import { Field, SwitchCardField } from "@/components/shared-form"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"

type UpdateCoreField = <K extends keyof CoreConfigForm>(
  key: K,
  value: CoreConfigForm[K],
) => void

type UpdateLauncherField = <K extends keyof LauncherForm>(
  key: K,
  value: LauncherForm[K],
) => void

interface ConfigSectionCardProps {
  title: string
  description?: string
  children: ReactNode
}

function ConfigSectionCard({
  title,
  description,
  children,
}: ConfigSectionCardProps) {
  return (
    <Card size="sm">
      <CardHeader className="border-border border-b">
        <CardTitle>{title}</CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      <CardContent className="pt-0">
        <div className="divide-border/70 divide-y">{children}</div>
      </CardContent>
    </Card>
  )
}

interface AgentDefaultsSectionProps {
  form: CoreConfigForm
  onFieldChange: UpdateCoreField
}

export function AgentDefaultsSection({
  form,
  onFieldChange,
}: AgentDefaultsSectionProps) {
  const { t } = useTranslation()

  return (
    <ConfigSectionCard title={t("pages.config.sections.agent")}>
      <Field
        label={t("pages.config.workspace")}
        hint={t("pages.config.workspace_hint")}
        layout="setting-row"
      >
        <Input
          value={form.workspace}
          onChange={(e) => onFieldChange("workspace", e.target.value)}
          placeholder="~/.khunquant/workspace"
        />
      </Field>

      <SwitchCardField
        label={t("pages.config.restrict_workspace")}
        hint={t("pages.config.restrict_workspace_hint")}
        layout="setting-row"
        checked={form.restrictToWorkspace}
        onCheckedChange={(checked) =>
          onFieldChange("restrictToWorkspace", checked)
        }
      />

      <SwitchCardField
        label={t("pages.config.allow_remote")}
        hint={t("pages.config.allow_remote_hint")}
        layout="setting-row"
        checked={form.allowRemote}
        onCheckedChange={(checked) => onFieldChange("allowRemote", checked)}
      />

      <Field
        label={t("pages.config.temperature")}
        hint={t("pages.config.temperature_hint")}
        layout="setting-row"
      >
        <Input
          type="number"
          min={0}
          max={2}
          step={0.01}
          value={form.temperature}
          onChange={(e) => onFieldChange("temperature", e.target.value)}
        />
      </Field>

      <SwitchCardField
        label={t("pages.config.follow_up_nudge")}
        hint={t("pages.config.follow_up_nudge_hint")}
        layout="setting-row"
        checked={form.followUpNudge}
        onCheckedChange={(checked) => onFieldChange("followUpNudge", checked)}
      />

      <Field
        label={t("pages.config.max_tokens")}
        hint={t("pages.config.max_tokens_hint")}
        layout="setting-row"
      >
        <Input
          type="number"
          min={1}
          value={form.maxTokens}
          onChange={(e) => onFieldChange("maxTokens", e.target.value)}
        />
      </Field>

      <Field
        label={t("pages.config.max_tool_iterations")}
        hint={t("pages.config.max_tool_iterations_hint")}
        layout="setting-row"
      >
        <Input
          type="number"
          min={1}
          value={form.maxToolIterations}
          onChange={(e) => onFieldChange("maxToolIterations", e.target.value)}
        />
      </Field>

      <Field
        label={t("pages.config.summarize_threshold")}
        hint={t("pages.config.summarize_threshold_hint")}
        layout="setting-row"
      >
        <Input
          type="number"
          min={1}
          value={form.summarizeMessageThreshold}
          onChange={(e) =>
            onFieldChange("summarizeMessageThreshold", e.target.value)
          }
        />
      </Field>

      <Field
        label={t("pages.config.summarize_token_percent")}
        hint={t("pages.config.summarize_token_percent_hint")}
        layout="setting-row"
      >
        <Input
          type="number"
          min={1}
          max={100}
          value={form.summarizeTokenPercent}
          onChange={(e) =>
            onFieldChange("summarizeTokenPercent", e.target.value)
          }
        />
      </Field>

      <Field
        label={t("pages.config.context_manager")}
        hint={t("pages.config.context_manager_hint")}
        layout="setting-row"
      >
        <Select
          value={form.contextManager}
          onValueChange={(value) => onFieldChange("contextManager", value)}
        >
          <SelectTrigger className="w-full">
            <SelectValue>
              {(() => {
                const opt = CONTEXT_MANAGER_OPTIONS.find(
                  (o) => o.value === form.contextManager,
                )
                return opt ? t(opt.labelKey, opt.labelDefault) : form.contextManager
              })()}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {CONTEXT_MANAGER_OPTIONS.map((opt) => (
              <SelectItem key={opt.value} value={opt.value}>
                <div className="flex flex-col gap-0.5">
                  <span className="font-medium">{t(opt.labelKey, opt.labelDefault)}</span>
                  <span className="text-muted-foreground text-xs">
                    {t(opt.descKey, opt.descDefault)}
                  </span>
                </div>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>
    </ConfigSectionCard>
  )
}

interface RuntimeSectionProps {
  form: CoreConfigForm
  onFieldChange: UpdateCoreField
}

export function RuntimeSection({ form, onFieldChange }: RuntimeSectionProps) {
  const { t } = useTranslation()
  const selectedDmScopeOption = DM_SCOPE_OPTIONS.find(
    (scope) => scope.value === form.dmScope,
  )

  return (
    <ConfigSectionCard title={t("pages.config.sections.runtime")}>
      <Field
        label={t("pages.config.session_scope")}
        hint={t("pages.config.session_scope_hint")}
        layout="setting-row"
      >
        <Select
          value={form.dmScope}
          onValueChange={(value) => onFieldChange("dmScope", value)}
        >
          <SelectTrigger className="w-full">
            <SelectValue>
              {selectedDmScopeOption
                ? t(
                    selectedDmScopeOption.labelKey,
                    selectedDmScopeOption.labelDefault,
                  )
                : form.dmScope}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {DM_SCOPE_OPTIONS.map((scope) => (
              <SelectItem key={scope.value} value={scope.value}>
                <div className="flex flex-col gap-0.5">
                  <span className="font-medium">{t(scope.labelKey)}</span>
                  <span className="text-muted-foreground text-xs">
                    {t(scope.descKey)}
                  </span>
                </div>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>

      <SwitchCardField
        label={t("pages.config.heartbeat_enabled")}
        hint={t("pages.config.heartbeat_enabled_hint")}
        layout="setting-row"
        checked={form.heartbeatEnabled}
        onCheckedChange={(checked) =>
          onFieldChange("heartbeatEnabled", checked)
        }
      />

      {form.heartbeatEnabled && (
        <Field
          label={t("pages.config.heartbeat_interval")}
          hint={t("pages.config.heartbeat_interval_hint")}
          layout="setting-row"
        >
          <Input
            type="number"
            min={1}
            value={form.heartbeatInterval}
            onChange={(e) => onFieldChange("heartbeatInterval", e.target.value)}
          />
        </Field>
      )}
    </ConfigSectionCard>
  )
}

interface LauncherSectionProps {
  launcherForm: LauncherForm
  onFieldChange: UpdateLauncherField
  disabled: boolean
}

export function LauncherSection({
  launcherForm,
  onFieldChange,
  disabled,
}: LauncherSectionProps) {
  const { t } = useTranslation()

  return (
    <ConfigSectionCard title={t("pages.config.sections.launcher")}>
      <SwitchCardField
        label={t("pages.config.lan_access")}
        hint={t("pages.config.lan_access_hint")}
        layout="setting-row"
        checked={launcherForm.publicAccess}
        disabled={disabled}
        onCheckedChange={(checked) => onFieldChange("publicAccess", checked)}
      />

      <Field
        label={t("pages.config.server_port")}
        hint={t("pages.config.server_port_hint")}
        layout="setting-row"
      >
        <Input
          type="number"
          min={1}
          max={65535}
          value={launcherForm.port}
          disabled={disabled}
          onChange={(e) => onFieldChange("port", e.target.value)}
        />
      </Field>

      <Field
        label={t("pages.config.allowed_cidrs")}
        hint={t("pages.config.allowed_cidrs_hint")}
        layout="setting-row"
        controlClassName="md:max-w-md"
      >
        <Textarea
          value={launcherForm.allowedCIDRsText}
          disabled={disabled}
          placeholder={t("pages.config.allowed_cidrs_placeholder")}
          className="min-h-[88px]"
          onChange={(e) => onFieldChange("allowedCIDRsText", e.target.value)}
        />
      </Field>
    </ConfigSectionCard>
  )
}

interface TradingRiskSectionProps {
  form: CoreConfigForm
  onFieldChange: UpdateCoreField
}

export function TradingRiskSection({ form, onFieldChange }: TradingRiskSectionProps) {
  const { t } = useTranslation()

  return (
    <ConfigSectionCard
      title={t("pages.config.sections.trading_risk", "Trading Risk")}
      description={t(
        "pages.config.sections.trading_risk_desc",
        "Controls for live order execution and futures trading.",
      )}
    >
      <SwitchCardField
        label={t("pages.config.allow_leverage", "Allow Leverage Trading")}
        hint={t(
          "pages.config.allow_leverage_hint",
          "Enable futures and leveraged order types. When disabled, the agent cannot open or close futures positions.",
        )}
        layout="setting-row"
        checked={form.allowLeverage}
        onCheckedChange={(checked) => onFieldChange("allowLeverage", checked)}
      />

      <SwitchCardField
        label={t("pages.config.paper_trading_mode", "Paper Trading Mode")}
        hint={t(
          "pages.config.paper_trading_mode_hint",
          "Simulate all orders without real execution. Safe for testing strategies.",
        )}
        layout="setting-row"
        checked={form.paperTradingMode}
        onCheckedChange={(checked) => onFieldChange("paperTradingMode", checked)}
      />
    </ConfigSectionCard>
  )
}

interface FinancialContextSectionProps {
  form: CoreConfigForm
  onFieldChange: UpdateCoreField
}

export function FinancialContextSection({
  form,
  onFieldChange,
}: FinancialContextSectionProps) {
  const { t } = useTranslation()

  return (
    <ConfigSectionCard title={t("pages.config.sections.financial_context", "Financial Context")}>
      <SwitchCardField
        label={t("pages.config.inject_financial_context")}
        hint={t("pages.config.inject_financial_context_hint")}
        layout="setting-row"
        checked={form.injectFinancialContext}
        onCheckedChange={(checked) => onFieldChange("injectFinancialContext", checked)}
      />

      {form.injectFinancialContext && (
        <>
          <Field
            label={t("pages.config.financial_context_ttl")}
            hint={t("pages.config.financial_context_ttl_hint")}
            layout="setting-row"
          >
            <Input
              type="number"
              min={0}
              value={form.financialContextTtlMinutes}
              onChange={(e) => onFieldChange("financialContextTtlMinutes", e.target.value)}
            />
          </Field>

          <Field
            label={t("pages.config.max_context_assets")}
            hint={t("pages.config.max_context_assets_hint")}
            layout="setting-row"
          >
            <Input
              type="number"
              min={1}
              value={form.maxContextAssets}
              onChange={(e) => onFieldChange("maxContextAssets", e.target.value)}
            />
          </Field>

          <Field
            label={t("pages.config.max_context_dca_plans")}
            hint={t("pages.config.max_context_dca_plans_hint")}
            layout="setting-row"
          >
            <Input
              type="number"
              min={0}
              value={form.maxContextDcaPlans}
              onChange={(e) => onFieldChange("maxContextDcaPlans", e.target.value)}
            />
          </Field>

          <Field
            label={t("pages.config.max_context_dn_plans")}
            hint={t("pages.config.max_context_dn_plans_hint")}
            layout="setting-row"
          >
            <Input
              type="number"
              min={0}
              value={form.maxContextDnPlans}
              onChange={(e) => onFieldChange("maxContextDnPlans", e.target.value)}
            />
          </Field>
        </>
      )}
    </ConfigSectionCard>
  )
}

interface DevicesSectionProps {
  form: CoreConfigForm
  onFieldChange: UpdateCoreField
  autoStartEnabled: boolean
  autoStartHint: string
  autoStartDisabled: boolean
  onAutoStartChange: (checked: boolean) => void
}

export function DevicesSection({
  form,
  onFieldChange,
  autoStartEnabled,
  autoStartHint,
  autoStartDisabled,
  onAutoStartChange,
}: DevicesSectionProps) {
  const { t } = useTranslation()

  return (
    <ConfigSectionCard title={t("pages.config.sections.devices")}>
      <SwitchCardField
        label={t("pages.config.devices_enabled")}
        hint={t("pages.config.devices_enabled_hint")}
        layout="setting-row"
        checked={form.devicesEnabled}
        onCheckedChange={(checked) => onFieldChange("devicesEnabled", checked)}
      />

      <SwitchCardField
        label={t("pages.config.monitor_usb")}
        hint={t("pages.config.monitor_usb_hint")}
        layout="setting-row"
        checked={form.monitorUSB}
        onCheckedChange={(checked) => onFieldChange("monitorUSB", checked)}
      />

      <SwitchCardField
        label={t("pages.config.autostart_label")}
        hint={autoStartHint}
        layout="setting-row"
        checked={autoStartEnabled}
        disabled={autoStartDisabled}
        onCheckedChange={onAutoStartChange}
      />
    </ConfigSectionCard>
  )
}

interface DebugSectionProps {
  form: CoreConfigForm
  onFieldChange: UpdateCoreField
}

export function DebugSection({ form, onFieldChange }: DebugSectionProps) {
  const { t } = useTranslation()

  return (
    <ConfigSectionCard
      title={t("pages.config.sections.debug", "Debug")}
      description={t(
        "pages.config.sections.debug_desc",
        "Advanced debugging and development tools.",
      )}
    >
      <SwitchCardField
        label={t("pages.config.debug_dev_mcp_enabled", "Developer MCP Server (Debug)")}
        hint={t(
          "pages.config.debug_dev_mcp_hint",
          "Exposes redacted runtime data over localhost. Keep disabled in production.",
        )}
        layout="setting-row"
        checked={form.debugDevMcpEnabled}
        onCheckedChange={(checked) => onFieldChange("debugDevMcpEnabled", checked)}
      />

      {form.debugDevMcpEnabled && (
        <div className="bg-blue-50 px-3 py-2 text-sm text-blue-700">
          {t(
            "pages.config.debug_dev_mcp_info",
            "Connect MCP clients to http://127.0.0.1:18790/dev-mcp — token available in gateway logs on startup",
          )}
        </div>
      )}
    </ConfigSectionCard>
  )
}
