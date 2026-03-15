import { IconLoader2 } from "@tabler/icons-react"
import { useCallback, useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import { getAppConfig, patchAppConfig } from "@/api/channels"
import { PageHeader } from "@/components/page-header"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import { useAtomValue } from "jotai"
import { gatewayAtom } from "@/store/gateway"

interface PortfolioConfigPageProps {
  exchangeName: string
}

function asRecord(value: unknown): Record<string, unknown> {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return value as Record<string, unknown>
  }
  return {}
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : ""
}

function asBool(value: unknown): boolean {
  return value === true
}

interface BinanceForm {
  enabled: boolean
  apiKey: string
  apiKeyEdit: string
  secret: string
  secretEdit: string
  testnet: boolean
}

const EMPTY_BINANCE_FORM: BinanceForm = {
  enabled: false,
  apiKey: "",
  apiKeyEdit: "",
  secret: "",
  secretEdit: "",
  testnet: false,
}

function getExchangeDisplayName(name: string): string {
  switch (name) {
    case "binance":
      return "Binance"
    default:
      return name.charAt(0).toUpperCase() + name.slice(1)
  }
}

function BinanceConfigForm({
  form,
  onChange,
}: {
  form: BinanceForm
  onChange: (patch: Partial<BinanceForm>) => void
}) {
  const { t } = useTranslation()

  return (
    <div className="divide-border/70 divide-y">
      <div className="flex items-center justify-between px-4 py-3">
        <div>
          <p className="text-sm font-medium">
            {t("portfolios.binance.api_key")}
          </p>
          <p className="text-muted-foreground mt-0.5 text-xs">
            {t("portfolios.binance.api_key_hint")}
          </p>
        </div>
        <div className="w-64">
          <Input
            type="password"
            value={form.apiKeyEdit}
            placeholder={
              form.apiKey
                ? t("portfolios.binance.credential_set")
                : t("portfolios.binance.api_key_placeholder")
            }
            onChange={(e) => onChange({ apiKeyEdit: e.target.value })}
          />
        </div>
      </div>

      <div className="flex items-center justify-between px-4 py-3">
        <div>
          <p className="text-sm font-medium">
            {t("portfolios.binance.secret")}
          </p>
          <p className="text-muted-foreground mt-0.5 text-xs">
            {t("portfolios.binance.secret_hint")}
          </p>
        </div>
        <div className="w-64">
          <Input
            type="password"
            value={form.secretEdit}
            placeholder={
              form.secret
                ? t("portfolios.binance.credential_set")
                : t("portfolios.binance.secret_placeholder")
            }
            onChange={(e) => onChange({ secretEdit: e.target.value })}
          />
        </div>
      </div>

      <div className="flex items-center justify-between px-4 py-3">
        <div>
          <p className="text-sm font-medium">
            {t("portfolios.binance.testnet")}
          </p>
          <p className="text-muted-foreground mt-0.5 text-xs">
            {t("portfolios.binance.testnet_hint")}
          </p>
        </div>
        <Switch
          checked={form.testnet}
          onCheckedChange={(checked) => onChange({ testnet: checked })}
        />
      </div>
    </div>
  )
}

export function PortfolioConfigPage({ exchangeName }: PortfolioConfigPageProps) {
  const { t } = useTranslation()
  const gateway = useAtomValue(gatewayAtom)

  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [fetchError, setFetchError] = useState("")
  const [serverError, setServerError] = useState("")

  const [baseForm, setBaseForm] = useState<BinanceForm>(EMPTY_BINANCE_FORM)
  const [form, setForm] = useState<BinanceForm>(EMPTY_BINANCE_FORM)

  const loadData = useCallback(async () => {
    if (exchangeName !== "binance") {
      setFetchError(t("portfolios.notFound", { name: exchangeName }))
      setLoading(false)
      return
    }

    setLoading(true)
    try {
      const appConfig = await getAppConfig()
      const exchanges = asRecord(asRecord(appConfig).exchanges)
      const binance = asRecord(exchanges.binance)

      const loaded: BinanceForm = {
        enabled: asBool(binance.enabled),
        apiKey: asString(binance.api_key),
        apiKeyEdit: "",
        secret: asString(binance.secret),
        secretEdit: "",
        testnet: asBool(binance.testnet),
      }

      setBaseForm(loaded)
      setForm(loaded)
      setFetchError("")
      setServerError("")
    } catch (e) {
      setFetchError(e instanceof Error ? e.message : t("portfolios.loadError"))
    } finally {
      setLoading(false)
    }
  }, [exchangeName, t])

  useEffect(() => {
    loadData()
  }, [loadData])

  const previousGatewayStatusRef = useRef(gateway.status)
  useEffect(() => {
    const previousStatus = previousGatewayStatusRef.current
    if (previousStatus !== "running" && gateway.status === "running") {
      void loadData()
    }
    previousGatewayStatusRef.current = gateway.status
  }, [gateway.status, loadData])

  const handleChange = (patch: Partial<BinanceForm>) => {
    setForm((prev) => ({ ...prev, ...patch }))
  }

  const handleReset = () => {
    setForm(baseForm)
    setServerError("")
  }

  const handleSave = async () => {
    setSaving(true)
    setServerError("")
    try {
      const apiKey =
        form.apiKeyEdit.trim() !== "" ? form.apiKeyEdit : form.apiKey
      const secret =
        form.secretEdit.trim() !== "" ? form.secretEdit : form.secret

      await patchAppConfig({
        exchanges: {
          binance: {
            enabled: form.enabled,
            api_key: apiKey,
            secret: secret,
            testnet: form.testnet,
          },
        },
      })
      toast.success(t("portfolios.saveSuccess"))
      await loadData()
    } catch (e) {
      const message =
        e instanceof Error ? e.message : t("portfolios.saveError")
      setServerError(message)
      toast.error(message)
    } finally {
      setSaving(false)
    }
  }

  const displayName = getExchangeDisplayName(exchangeName)
  const isConfigured = form.apiKey !== "" && form.secret !== ""

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title={displayName}
        titleExtra={
          <div className="flex items-center gap-1.5">
            {form.enabled ? (
              <span className="rounded-full bg-emerald-500/10 px-2 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400">
                {t("portfolios.status.enabled")}
              </span>
            ) : isConfigured ? (
              <span className="rounded-full bg-amber-500/10 px-2 py-0.5 text-[10px] font-medium text-amber-600 dark:text-amber-400">
                {t("portfolios.status.configured")}
              </span>
            ) : null}
          </div>
        }
      />

      <div className="flex min-h-0 flex-1 justify-center overflow-y-auto px-4 pb-8 sm:px-6">
        {loading ? (
          <div className="flex items-center justify-center py-20">
            <IconLoader2 className="text-muted-foreground size-6 animate-spin" />
          </div>
        ) : fetchError ? (
          <div className="text-destructive bg-destructive/10 rounded-lg px-4 py-3 text-sm">
            {fetchError}
          </div>
        ) : (
          <div className="w-full max-w-250 space-y-5 pt-2">
            <p className="text-sm font-medium">
              {t("portfolios.edit", { name: displayName })}
            </p>

            <div className="border-border/60 bg-background rounded-lg border">
              <div className="flex items-center justify-between px-4 py-3">
                <p className="text-sm font-medium">
                  {t("portfolios.enableLabel")}
                </p>
                <Switch
                  checked={form.enabled}
                  onCheckedChange={(checked) =>
                    handleChange({ enabled: checked })
                  }
                />
              </div>

              {exchangeName === "binance" && (
                <BinanceConfigForm form={form} onChange={handleChange} />
              )}
            </div>

            {serverError && (
              <p className="text-destructive text-sm">{serverError}</p>
            )}

            <div className="border-border/60 flex justify-end gap-2 border-t py-4">
              <Button variant="outline" onClick={handleReset} disabled={saving}>
                {t("common.reset")}
              </Button>
              <Button onClick={handleSave} disabled={saving}>
                {saving ? t("common.saving") : t("common.save")}
              </Button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
