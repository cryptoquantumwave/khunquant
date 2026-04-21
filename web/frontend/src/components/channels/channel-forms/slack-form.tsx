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
import { Field, KeyInput } from "@/components/shared-form"
import { Card, CardContent } from "@/components/ui/card"

interface SlackFormProps {
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

export function SlackForm({
  config,
  onChange,
  isEdit,
  fieldErrors = {},
  registerArrayFieldFlusher,
  arrayFieldResetVersion,
}: SlackFormProps) {
  const { t } = useTranslation()
  const botTokenExtraHint =
    isEdit && asString(config.bot_token)
      ? ` ${t("channels.field.secretHintSet")}`
      : ""
  const appTokenExtraHint =
    isEdit && asString(config.app_token)
      ? ` ${t("channels.field.secretHintSet")}`
      : ""

  return (
    <div className="space-y-5">
      <Field
        label={t("channels.field.botToken")}
        required
        hint={`${t("channels.form.desc.botToken")}${botTokenExtraHint}`}
        error={fieldErrors.bot_token}
      >
        <KeyInput
          value={asString(config._bot_token)}
          onChange={(v) => onChange("_bot_token", v)}
          placeholder={maskedSecretPlaceholder(config.bot_token, "xoxb-xxxx")}
        />
      </Field>

      <Field
        label={t("channels.field.appToken")}
        hint={`${t("channels.form.desc.appToken")}${appTokenExtraHint}`}
      >
        <KeyInput
          value={asString(config._app_token)}
          onChange={(v) => onChange("_app_token", v)}
          placeholder={maskedSecretPlaceholder(config.app_token, "xapp-xxxx")}
        />
      </Field>

      <Card className="shadow-sm">
        <CardContent className="divide-border/60 divide-y px-6 py-0 [&>div]:py-5">
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
        </CardContent>
      </Card>
    </div>
  )
}
