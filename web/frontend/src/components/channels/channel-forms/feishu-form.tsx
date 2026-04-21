import { useTranslation } from "react-i18next"

import type { ChannelConfig } from "@/api/channels"
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
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"

interface FeishuFormProps {
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

function asBool(value: unknown): boolean {
  return typeof value === "boolean" ? value : false
}

export function FeishuForm({
  config,
  onChange,
  isEdit,
  fieldErrors = {},
  registerArrayFieldFlusher,
  arrayFieldResetVersion,
}: FeishuFormProps) {
  const { t } = useTranslation()
  const appSecretExtraHint =
    isEdit && asString(config.app_secret)
      ? ` ${t("channels.field.secretHintSet")}`
      : ""
  const verificationExtraHint =
    isEdit && asString(config.verification_token)
      ? ` ${t("channels.field.secretHintSet")}`
      : ""
  const encryptExtraHint =
    isEdit && asString(config.encrypt_key)
      ? ` ${t("channels.field.secretHintSet")}`
      : ""

  return (
    <div className="space-y-5">
      <Field
        label={t("channels.field.appId")}
        required
        hint={t("channels.form.desc.appId")}
        error={fieldErrors.app_id}
      >
        <Input
          value={asString(config.app_id)}
          onChange={(e) => onChange("app_id", e.target.value)}
          placeholder="cli_xxxx"
        />
      </Field>

      <Field
        label={t("channels.field.appSecret")}
        required
        hint={`${t("channels.form.desc.appSecret")}${appSecretExtraHint}`}
        error={fieldErrors.app_secret}
      >
        <KeyInput
          value={asString(config._app_secret)}
          onChange={(v) => onChange("_app_secret", v)}
          placeholder={maskedSecretPlaceholder(
            config.app_secret,
            t("channels.field.secretPlaceholder"),
          )}
        />
      </Field>

      <Card className="py-3 shadow-sm">
        <CardContent className="divide-border/60 divide-y px-6 py-0 [&>div]:py-5">
          <Field
            label={t("channels.field.verificationToken")}
            hint={`${t("channels.form.desc.verificationToken")}${verificationExtraHint}`}
          >
            <KeyInput
              value={asString(config._verification_token)}
              onChange={(v) => onChange("_verification_token", v)}
              placeholder={maskedSecretPlaceholder(
                config.verification_token,
                t("channels.field.secretPlaceholder"),
              )}
            />
          </Field>
          <Field
            label={t("channels.field.encryptKey")}
            hint={`${t("channels.form.desc.encryptKey")}${encryptExtraHint}`}
          >
            <KeyInput
              value={asString(config._encrypt_key)}
              onChange={(v) => onChange("_encrypt_key", v)}
              placeholder={maskedSecretPlaceholder(
                config.encrypt_key,
                t("channels.field.secretPlaceholder"),
              )}
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

          <div>
            <SwitchCardField
              label={t("channels.field.isLark")}
              hint={t("channels.form.desc.isLark")}
              checked={asBool(config.is_lark)}
              onCheckedChange={(checked) => onChange("is_lark", checked)}
              ariaLabel={t("channels.field.isLark")}
            />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
