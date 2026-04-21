import { IconCheck, IconLoader2, IconShieldLock, IconX } from "@tabler/icons-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import type { ChannelConfig } from "@/api/channels"
import {
  type PairingRequest,
  approvePairing,
  getPairingRequests,
  rejectPairing,
} from "@/api/pairing"
import {
  type ArrayFieldFlusher,
  ChannelArrayListField,
} from "@/components/channels/channel-array-list-field"
import {
  asStringArray,
  parseAllowFromInput,
} from "@/components/channels/channel-array-utils"
import { maskedSecretPlaceholder } from "@/components/secret-placeholder"
import { Field, KeyInput, SwitchCardField } from "@/components/shared-form"
import { updateGatewayStore } from "@/store"
import { AlertDot } from "@/components/ui/alert-dot"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"

interface TelegramFormProps {
  config: ChannelConfig
  onChange: (key: string, value: unknown) => void
  isEdit: boolean
  fieldErrors?: Record<string, string>
  registerArrayFieldFlusher?: (
    fieldPath: string,
    flusher: ArrayFieldFlusher | null,
  ) => void
  arrayFieldResetVersion?: number
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : ""
}

function asRecord(value: unknown): Record<string, unknown> {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return value as Record<string, unknown>
  }
  return {}
}

function asBool(value: unknown): boolean {
  return value === true
}

function formatExpiry(expiresAtMs: number): string {
  const remaining = expiresAtMs - Date.now()
  if (remaining <= 0) return "Expired"
  const minutes = Math.ceil(remaining / 60_000)
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  const mins = minutes % 60
  return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`
}

function PairingSection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()

  const { data: requests, isLoading, error } = useQuery({
    queryKey: ["pairing-requests"],
    queryFn: getPairingRequests,
    refetchInterval: 5_000,
  })

  const approveMutation = useMutation({
    mutationFn: approvePairing,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["pairing-requests"] })
      updateGatewayStore({ restartRequired: true })
      toast.success(t("pages.agent.pairing.approve_success"))
    },
    onError: (err: Error) => {
      toast.error(t("pages.agent.pairing.approve_error"), { description: err.message })
    },
  })

  const rejectMutation = useMutation({
    mutationFn: rejectPairing,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["pairing-requests"] })
      toast.success(t("pages.agent.pairing.reject_success"))
    },
    onError: (err: Error) => {
      toast.error(t("pages.agent.pairing.reject_error"), { description: err.message })
    },
  })

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <IconShieldLock className="text-muted-foreground size-4" />
        <p className="text-sm font-medium">{t("pages.agent.pairing.section_title")}</p>
        {isLoading
          ? <IconLoader2 className="text-muted-foreground size-3.5 animate-spin" />
          : requests && requests.length > 0 && <AlertDot />
        }
      </div>

      {error && (
        <p className="text-destructive text-xs">{t("pages.agent.pairing.load_error")}</p>
      )}

      {!isLoading && !error && (!requests || requests.length === 0) && (
        <p className="text-muted-foreground text-xs">{t("pages.agent.pairing.empty")}</p>
      )}

      {requests && requests.length > 0 && (
        <div className="space-y-2">
          {requests.map((req: PairingRequest) => {
            const displayName = req.display_name || req.username || req.platform_id
            const username = req.username ? `@${req.username}` : null
            const approving = approveMutation.isPending && approveMutation.variables === req.code
            const rejecting = rejectMutation.isPending && rejectMutation.variables === req.code

            return (
              <Card key={req.code} className="border-border/60">
                <CardHeader className="pb-2 pt-3 px-4">
                  <div className="flex items-start justify-between gap-2">
                    <div className="min-w-0">
                      <CardTitle className="text-sm font-medium">{displayName}</CardTitle>
                      {username && (
                        <CardDescription className="text-xs">{username}</CardDescription>
                      )}
                    </div>
                    <span className="bg-muted text-muted-foreground shrink-0 rounded px-2 py-0.5 font-mono text-xs">
                      {req.code}
                    </span>
                  </div>
                </CardHeader>
                <CardContent className="px-4 pb-3 pt-0">
                  <p className="text-muted-foreground mb-2 text-xs">
                    ID: {req.platform_id} · {t("pages.agent.pairing.expires")}: {formatExpiry(req.expires_at_ms)}
                  </p>
                  <div className="flex gap-2">
                    <Button
                      size="sm"
                      onClick={() => approveMutation.mutate(req.code)}
                      disabled={approving || rejecting}
                      className="h-7 gap-1 px-2 text-xs"
                    >
                      {approving ? <IconLoader2 className="size-3 animate-spin" /> : <IconCheck className="size-3" />}
                      {t("pages.agent.pairing.approve")}
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => rejectMutation.mutate(req.code)}
                      disabled={approving || rejecting}
                      className="h-7 gap-1 px-2 text-xs"
                    >
                      {rejecting ? <IconLoader2 className="size-3 animate-spin" /> : <IconX className="size-3" />}
                      {t("pages.agent.pairing.reject")}
                    </Button>
                  </div>
                </CardContent>
              </Card>
            )
          })}
        </div>
      )}
    </div>
  )
}

export function TelegramForm({
  config,
  onChange,
  isEdit,
  fieldErrors = {},
  registerArrayFieldFlusher,
  arrayFieldResetVersion,
}: TelegramFormProps) {
  const { t } = useTranslation()
  const typingConfig = asRecord(config.typing)
  const placeholderConfig = asRecord(config.placeholder)
  const placeholderEnabled = asBool(placeholderConfig.enabled)
  const tokenExtraHint =
    isEdit && asString(config.token)
      ? ` ${t("channels.field.secretHintSet")}`
      : ""

  return (
    <div className="space-y-5">
      <Field
        label={t("channels.field.token")}
        required
        hint={`${t("channels.form.desc.token")}${tokenExtraHint}`}
        error={fieldErrors.token}
      >
        <KeyInput
          value={asString(config._token)}
          onChange={(v) => onChange("_token", v)}
          placeholder={maskedSecretPlaceholder(
            config.token,
            t("channels.field.tokenPlaceholder"),
          )}
        />
      </Field>

      <Field
        label={t("channels.field.baseUrl")}
        hint={t("channels.form.desc.baseUrl")}
      >
        <Input
          value={asString(config.base_url)}
          onChange={(e) => onChange("base_url", e.target.value)}
          placeholder="https://api.telegram.org"
        />
      </Field>
      <Field
        label={t("channels.field.proxy")}
        hint={t("channels.form.desc.proxy")}
      >
        <Input
          value={asString(config.proxy)}
          onChange={(e) => onChange("proxy", e.target.value)}
          placeholder="http://127.0.0.1:7890"
        />
      </Field>

      <ChannelArrayListField
        label={t("channels.field.allowFrom")}
        hint={t("channels.form.desc.allowFrom")}
        value={asStringArray(config.allow_from)}
        onChange={(value) => onChange("allow_from", value)}
        placeholder={t("channels.field.allowFromPlaceholder")}
        parser={parseAllowFromInput}
        fieldPath="allow_from"
        registerFlusher={registerArrayFieldFlusher}
        resetVersion={arrayFieldResetVersion}
      />

      <SwitchCardField
        label={t("channels.field.typingEnabled")}
        hint={t("channels.form.desc.typingEnabled")}
        checked={asBool(typingConfig.enabled)}
        onCheckedChange={(checked) =>
          onChange("typing", { ...typingConfig, enabled: checked })
        }
        ariaLabel={t("channels.field.typingEnabled")}
      />

      <SwitchCardField
        label={t("channels.field.placeholderEnabled")}
        hint={t("channels.form.desc.placeholderEnabled")}
        checked={placeholderEnabled}
        onCheckedChange={(checked) =>
          onChange("placeholder", {
            ...placeholderConfig,
            enabled: checked,
          })
        }
        ariaLabel={t("channels.field.placeholderEnabled")}
      >
        {placeholderEnabled && (
          <div className="space-y-1">
            <Input
              value={asString(placeholderConfig.text)}
              onChange={(e) =>
                onChange("placeholder", {
                  ...placeholderConfig,
                  text: e.target.value,
                })
              }
              placeholder={t("channels.field.placeholderText")}
              aria-label={t("channels.field.placeholderText")}
            />
          </div>
        )}
      </SwitchCardField>

      <SwitchCardField
        label={t("channels.field.pairingEnabled")}
        hint={t("channels.form.desc.pairingEnabled")}
        checked={asBool(config.pairing_enabled) || config.pairing_enabled === undefined}
        onCheckedChange={(checked) => onChange("pairing_enabled", checked)}
        ariaLabel={t("channels.field.pairingEnabled")}
      >
        {(asBool(config.pairing_enabled) || config.pairing_enabled === undefined) && (
          <PairingSection />
        )}
      </SwitchCardField>
    </div>
  )
}
