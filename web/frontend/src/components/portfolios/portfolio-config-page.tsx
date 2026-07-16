import {
  IconInfoCircle,
  IconLoader2,
  IconPlus,
  IconTrash,
} from "@tabler/icons-react"
import { useAtomValue } from "jotai"
import { useCallback, useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import { getAppConfig, patchAppConfig } from "@/api/channels"
import { PageHeader } from "@/components/page-header"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { useWebullConnect } from "@/hooks/use-webull-connect"
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

function asBool(value: unknown): boolean {
  return value === true
}

// ── Account types ──────────────────────────────────────────────────────────

interface AccountDraft {
  /** "" means unnamed; will be assigned positional name on the backend */
  name: string
  apiKey: string
  apiKeyEdit: string
  secret: string
  secretEdit: string
  proxy: string
}

interface OKXAccountDraft extends AccountDraft {
  passphrase: string
  passphraseEdit: string
}

interface SettradeAccountDraft extends AccountDraft {
  brokerId: string
  appCode: string
  accountNo: string
  pin: string
  pinEdit: string
}

interface WebullAccountDraft extends AccountDraft {
  accountId: string
  region: string
  environment: string
}

// ── Exchange-level form ────────────────────────────────────────────────────

interface ExchangeForm {
  enabled: boolean
  testnet?: boolean
}

interface BinanceForm extends ExchangeForm {
  accounts: AccountDraft[]
}

interface OKXForm extends ExchangeForm {
  accounts: OKXAccountDraft[]
}

interface BitkubForm extends ExchangeForm {
  accounts: AccountDraft[]
}

interface BinanceTHForm extends ExchangeForm {
  accounts: AccountDraft[]
}

interface SettradeForm extends ExchangeForm {
  accounts: SettradeAccountDraft[]
}

interface WebullForm extends ExchangeForm {
  accounts: WebullAccountDraft[]
}

// ── Helpers ────────────────────────────────────────────────────────────────

function emptyAccount(): AccountDraft {
  return {
    name: "",
    apiKey: "",
    apiKeyEdit: "",
    secret: "",
    secretEdit: "",
    proxy: "",
  }
}

function emptyOKXAccount(): OKXAccountDraft {
  return { ...emptyAccount(), passphrase: "", passphraseEdit: "" }
}

function emptySettradeAccount(): SettradeAccountDraft {
  return {
    ...emptyAccount(),
    brokerId: "",
    appCode: "ALGO_EQ",
    accountNo: "",
    pin: "",
    pinEdit: "",
  }
}

function emptyWebullAccount(): WebullAccountDraft {
  return {
    ...emptyAccount(),
    accountId: "",
    region: "us",
    environment: "prod",
  }
}

function parseAccounts(raw: unknown): AccountDraft[] {
  if (!Array.isArray(raw)) return []
  return raw.map((item) => {
    const r = asRecord(item)
    return {
      name: typeof r.name === "string" ? r.name : "",
      apiKey: typeof r.api_key === "string" ? r.api_key : "",
      apiKeyEdit: "",
      secret: typeof r.secret === "string" ? r.secret : "",
      secretEdit: "",
      proxy: typeof r.proxy === "string" ? r.proxy : "",
    }
  })
}

function parseOKXAccounts(raw: unknown): OKXAccountDraft[] {
  if (!Array.isArray(raw)) return []
  return raw.map((item) => {
    const r = asRecord(item)
    return {
      name: typeof r.name === "string" ? r.name : "",
      apiKey: typeof r.api_key === "string" ? r.api_key : "",
      apiKeyEdit: "",
      secret: typeof r.secret === "string" ? r.secret : "",
      secretEdit: "",
      passphrase: typeof r.passphrase === "string" ? r.passphrase : "",
      passphraseEdit: "",
      proxy: typeof r.proxy === "string" ? r.proxy : "",
    }
  })
}

/** Serialize an AccountDraft to the shape the backend expects */
function serializeAccount(acc: AccountDraft) {
  const apiKey = acc.apiKeyEdit.trim() !== "" ? acc.apiKeyEdit : acc.apiKey
  const secret = acc.secretEdit.trim() !== "" ? acc.secretEdit : acc.secret
  return {
    ...(acc.name.trim() !== "" ? { name: acc.name.trim() } : {}),
    api_key: apiKey,
    secret: secret,
    proxy: acc.proxy.trim(),
  }
}

function serializeOKXAccount(acc: OKXAccountDraft) {
  const passphrase =
    acc.passphraseEdit.trim() !== "" ? acc.passphraseEdit : acc.passphrase
  return { ...serializeAccount(acc), passphrase }
}

function serializeSettradeAccount(acc: SettradeAccountDraft) {
  const pin = acc.pinEdit.trim() !== "" ? acc.pinEdit : acc.pin
  return {
    ...serializeAccount(acc),
    broker_id: acc.brokerId,
    app_code: acc.appCode,
    account_no: acc.accountNo,
    ...(pin !== "" ? { pin } : {}),
  }
}

function serializeWebullAccount(acc: WebullAccountDraft) {
  return {
    ...serializeAccount(acc),
    account_id: acc.accountId,
    region: acc.region,
    environment: acc.environment,
  }
}

function parseSettradeAccounts(raw: unknown): SettradeAccountDraft[] {
  if (!Array.isArray(raw)) return []
  return raw.map((item) => {
    const r = asRecord(item)
    return {
      name: typeof r.name === "string" ? r.name : "",
      apiKey: typeof r.api_key === "string" ? r.api_key : "",
      apiKeyEdit: "",
      secret: typeof r.secret === "string" ? r.secret : "",
      secretEdit: "",
      brokerId: typeof r.broker_id === "string" ? r.broker_id : "",
      appCode: typeof r.app_code === "string" ? r.app_code : "",
      accountNo: typeof r.account_no === "string" ? r.account_no : "",
      pin: typeof r.pin === "string" ? r.pin : "",
      pinEdit: "",
      proxy: typeof r.proxy === "string" ? r.proxy : "",
    }
  })
}

function parseWebullAccounts(raw: unknown): WebullAccountDraft[] {
  if (!Array.isArray(raw)) return []
  return raw.map((item) => {
    const r = asRecord(item)
    return {
      name: typeof r.name === "string" ? r.name : "",
      apiKey: typeof r.api_key === "string" ? r.api_key : "",
      apiKeyEdit: "",
      secret: typeof r.secret === "string" ? r.secret : "",
      secretEdit: "",
      accountId: typeof r.account_id === "string" ? r.account_id : "",
      region: typeof r.region === "string" && r.region !== "" ? r.region : "us",
      environment:
        typeof r.environment === "string" && r.environment !== ""
          ? r.environment
          : "prod",
      proxy: typeof r.proxy === "string" ? r.proxy : "",
    }
  })
}

function getExchangeDisplayName(name: string): string {
  switch (name) {
    case "binance":
      return "Binance"
    case "okx":
      return "OKX"
    case "bitkub":
      return "Bitkub"
    case "binanceth":
      return "Binance TH"
    case "settrade":
      return "Settrade"
    case "webull":
      return "Webull"
    default:
      return name.charAt(0).toUpperCase() + name.slice(1)
  }
}

function getExchangeDocsUrl(exchangeName: string): string {
  return `https://khunquant.com/docs/exchanges/${exchangeName}`
}

// ── Settrade static data ───────────────────────────────────────────────────

const SETTRADE_BROKERS = [
  { id: "008", name: "ASP" },
  { id: "038", name: "BYD" },
  { id: "007", name: "CGSI" },
  { id: "063", name: "Classic Ausiris" },
  { id: "032", name: "DAOL SEC" },
  { id: "025", name: "Globlex" },
  { id: "023", name: "INVX" },
  { id: "013", name: "KGI" },
  { id: "015", name: "Kingsford" },
  { id: "029", name: "Krungsri" },
  { id: "018", name: "KTX" },
  { id: "060", name: "MTSGF" },
  { id: "003", name: "Pi" },
  { id: "034", name: "PST" },
  { id: "022", name: "Trinity" },
  { id: "026", name: "UOB" },
  { id: "062", name: "YLG" },
  { id: "019", name: "Yuanta" },
]

const SETTRADE_APP_CODES = [
  { value: "ALGO_EQ", label: "ALGO_EQ (Equity)" },
  { value: "ALGO", label: "ALGO (Derivatives)" },
]

const SETTRADE_BROKER_LIST_URL =
  "https://developer.settrade.com/open-api/document/broker-list"
const SETTRADE_OPEN_API_DOC_URL =
  "https://developer.settrade.com/open-api/document"
const WEBULL_DOC_URL = "https://developer.webull.com/apis/docs"

// ── Account card ───────────────────────────────────────────────────────────

function AccountCard({
  index,
  account,
  hasPassphrase,
  isSettrade,
  isWebull,
  onChange,
  onRemove,
}: {
  index: number
  account:
    | AccountDraft
    | OKXAccountDraft
    | SettradeAccountDraft
    | WebullAccountDraft
  hasPassphrase?: boolean
  isSettrade?: boolean
  isWebull?: boolean
  onChange: (
    patch: Partial<OKXAccountDraft & SettradeAccountDraft & WebullAccountDraft>,
  ) => void
  onRemove: () => void
}) {
  const { t } = useTranslation()
  const placeholder = `Account ${index + 1}`
  const okxAcc = account as OKXAccountDraft
  const stAcc = account as SettradeAccountDraft
  const wbAcc = account as WebullAccountDraft

  // Mirrors the backend's account-name resolution (config.ResolveAccount):
  // an unnamed account is addressed by its 1-based position.
  const effectiveAccountName = account.name.trim() || String(index + 1)
  const webullConnect = useWebullConnect(effectiveAccountName, !!isWebull)

  const apiKeyLabel = isSettrade
    ? t("portfolios.settrade.api_key")
    : isWebull
      ? t("portfolios.webull.api_key")
      : t("portfolios.binance.api_key")
  const apiKeyPlaceholder = isSettrade
    ? account.apiKey
      ? t("portfolios.settrade.credential_set")
      : t("portfolios.settrade.api_key_placeholder")
    : isWebull
      ? account.apiKey
        ? t("portfolios.webull.credential_set")
        : t("portfolios.webull.api_key_placeholder")
      : account.apiKey
        ? t("portfolios.binance.credential_set")
        : t("portfolios.binance.api_key_placeholder")
  const secretLabel = isSettrade
    ? t("portfolios.settrade.secret")
    : isWebull
      ? t("portfolios.webull.secret")
      : t("portfolios.binance.secret")
  const secretPlaceholder = isSettrade
    ? account.secret
      ? t("portfolios.settrade.credential_set")
      : t("portfolios.settrade.secret_placeholder")
    : isWebull
      ? account.secret
        ? t("portfolios.webull.credential_set")
        : t("portfolios.webull.secret_placeholder")
      : account.secret
        ? t("portfolios.binance.credential_set")
        : t("portfolios.binance.secret_placeholder")

  return (
    <div className="border-border/60 rounded-lg border">
      <div className="flex items-center justify-between px-4 py-3">
        <Input
          className="w-48 text-sm"
          value={account.name}
          placeholder={placeholder}
          onChange={(e) => onChange({ name: e.target.value })}
        />
        <Button
          variant="ghost"
          size="icon"
          className="text-muted-foreground hover:text-destructive"
          onClick={onRemove}
          title={t("common.remove")}
        >
          <IconTrash className="size-4" />
        </Button>
      </div>

      <div className="divide-border/70 divide-y border-t">
        <div className="flex items-center justify-between px-4 py-3">
          <p className="flex items-center gap-1.5 text-sm">
            {apiKeyLabel}
            {isSettrade && (
              <a
                href={SETTRADE_OPEN_API_DOC_URL}
                target="_blank"
                rel="noreferrer"
                className="text-muted-foreground hover:text-foreground"
                title={t("portfolios.settrade.open_api_doc_link")}
              >
                <IconInfoCircle className="size-4" />
              </a>
            )}
            {isWebull && (
              <a
                href={WEBULL_DOC_URL}
                target="_blank"
                rel="noreferrer"
                className="text-muted-foreground hover:text-foreground"
                title={t("portfolios.webull.doc_link")}
              >
                <IconInfoCircle className="size-4" />
              </a>
            )}
          </p>
          <div className="w-64">
            <Input
              type="password"
              value={account.apiKeyEdit}
              placeholder={apiKeyPlaceholder}
              onChange={(e) => onChange({ apiKeyEdit: e.target.value })}
            />
          </div>
        </div>

        <div className="flex items-center justify-between px-4 py-3">
          <p className="flex items-center gap-1.5 text-sm">
            {secretLabel}
            {isSettrade && (
              <a
                href={SETTRADE_OPEN_API_DOC_URL}
                target="_blank"
                rel="noreferrer"
                className="text-muted-foreground hover:text-foreground"
                title={t("portfolios.settrade.open_api_doc_link")}
              >
                <IconInfoCircle className="size-4" />
              </a>
            )}
            {isWebull && (
              <a
                href={WEBULL_DOC_URL}
                target="_blank"
                rel="noreferrer"
                className="text-muted-foreground hover:text-foreground"
                title={t("portfolios.webull.doc_link")}
              >
                <IconInfoCircle className="size-4" />
              </a>
            )}
          </p>
          <div className="w-64">
            <Input
              type="password"
              value={account.secretEdit}
              placeholder={secretPlaceholder}
              onChange={(e) => onChange({ secretEdit: e.target.value })}
            />
          </div>
        </div>

        {hasPassphrase && (
          <div className="flex items-center justify-between px-4 py-3">
            <p className="text-sm">{t("portfolios.okx.passphrase")}</p>
            <div className="w-64">
              <Input
                type="password"
                value={okxAcc.passphraseEdit ?? ""}
                placeholder={
                  okxAcc.passphrase
                    ? t("portfolios.okx.credential_set")
                    : t("portfolios.okx.passphrase_placeholder")
                }
                onChange={(e) => onChange({ passphraseEdit: e.target.value })}
              />
            </div>
          </div>
        )}

        {isSettrade && (
          <>
            <div className="bg-muted/30 px-4 py-3">
              <p className="text-sm font-medium">
                {t("portfolios.settrade.broker_api_support_title")}
              </p>
              <p className="text-muted-foreground mt-1 text-xs">
                {t("portfolios.settrade.broker_api_support_desc")}{" "}
                <a
                  href={SETTRADE_BROKER_LIST_URL}
                  target="_blank"
                  rel="noreferrer"
                  className="text-foreground underline underline-offset-2"
                >
                  {t("portfolios.settrade.broker_api_support_link")}
                </a>
              </p>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.settrade.broker_id")}</p>
              <div className="w-64">
                <Select
                  value={stAcc.brokerId ?? ""}
                  onValueChange={(v) => onChange({ brokerId: v })}
                >
                  <SelectTrigger>
                    <SelectValue
                      placeholder={t(
                        "portfolios.settrade.broker_id_placeholder",
                      )}
                    />
                  </SelectTrigger>
                  <SelectContent>
                    {SETTRADE_BROKERS.map((b) => (
                      <SelectItem key={b.id} value={b.id}>
                        {b.name} ({b.id})
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.settrade.app_code")}</p>
              <div className="w-64">
                <Select
                  value={stAcc.appCode ?? ""}
                  onValueChange={(v) => onChange({ appCode: v })}
                >
                  <SelectTrigger>
                    <SelectValue
                      placeholder={t(
                        "portfolios.settrade.app_code_placeholder",
                      )}
                    />
                  </SelectTrigger>
                  <SelectContent>
                    {SETTRADE_APP_CODES.map((c) => (
                      <SelectItem key={c.value} value={c.value}>
                        {c.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.settrade.account_no")}</p>
              <div className="w-64">
                <Input
                  value={stAcc.accountNo ?? ""}
                  placeholder={t("portfolios.settrade.account_no_placeholder")}
                  onChange={(e) => onChange({ accountNo: e.target.value })}
                />
              </div>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.settrade.pin")}</p>
              <div className="w-64">
                <Input
                  type="password"
                  value={stAcc.pinEdit ?? ""}
                  placeholder={
                    stAcc.pin
                      ? t("portfolios.settrade.pin_set")
                      : t("portfolios.settrade.pin_placeholder")
                  }
                  onChange={(e) => onChange({ pinEdit: e.target.value })}
                />
              </div>
            </div>
          </>
        )}

        {isWebull && (
          <>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.webull.account_id")}</p>
              <div className="w-64">
                <Input
                  value={wbAcc.accountId ?? ""}
                  placeholder={t("portfolios.webull.account_id_placeholder")}
                  onChange={(e) => onChange({ accountId: e.target.value })}
                />
              </div>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.webull.region")}</p>
              <div className="w-64">
                <Input
                  value={wbAcc.region ?? "us"}
                  placeholder={t("portfolios.webull.region_placeholder")}
                  onChange={(e) => onChange({ region: e.target.value })}
                />
              </div>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <p className="text-sm">{t("portfolios.webull.environment")}</p>
              <div className="w-64">
                <Select
                  value={wbAcc.environment ?? "prod"}
                  onValueChange={(v) => onChange({ environment: v })}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="prod">
                      {t("portfolios.webull.environment_prod")}
                    </SelectItem>
                    <SelectItem value="uat">
                      {t("portfolios.webull.environment_uat")}
                    </SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="flex items-center justify-between px-4 py-3">
              <div>
                <p className="text-sm">
                  {t("portfolios.webull.connect_title")}
                </p>
                <p className="text-muted-foreground mt-0.5 text-xs">
                  {webullConnect.status === "NORMAL" &&
                    (webullConnect.daysRemaining !== undefined
                      ? t("portfolios.webull.status_normal", {
                          days: Math.max(
                            0,
                            Math.floor(webullConnect.daysRemaining),
                          ),
                        })
                      : t("portfolios.webull.status_normal_today"))}
                  {webullConnect.status === "PENDING" &&
                    t("portfolios.webull.status_pending")}
                  {webullConnect.status === "INVALID" &&
                    t("portfolios.webull.status_invalid")}
                  {webullConnect.status === "EXPIRED" &&
                    t("portfolios.webull.status_expired")}
                  {webullConnect.error && (
                    <span className="text-destructive">
                      {webullConnect.error}
                    </span>
                  )}
                </p>
              </div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={
                  webullConnect.connecting || webullConnect.status === "PENDING"
                }
                onClick={() => void webullConnect.connect()}
              >
                {(webullConnect.connecting ||
                  webullConnect.status === "PENDING") && (
                  <IconLoader2 className="mr-1.5 size-4 animate-spin" />
                )}
                {webullConnect.status === "NORMAL"
                  ? t("portfolios.webull.reconnect_button")
                  : t("portfolios.webull.connect_button")}
              </Button>
            </div>
          </>
        )}

        <div className="flex items-center justify-between px-4 py-3">
          <div>
            <p className="text-sm">{t("portfolios.proxy")}</p>
            <p className="text-muted-foreground mt-0.5 text-xs">
              {t("portfolios.proxy_description")}
            </p>
          </div>
          <div className="w-64">
            <Input
              value={account.proxy ?? ""}
              placeholder={t("portfolios.proxy_placeholder")}
              onChange={(e) => onChange({ proxy: e.target.value })}
            />
          </div>
        </div>
      </div>
    </div>
  )
}

// ── Page ───────────────────────────────────────────────────────────────────

type AnyForm =
  | BinanceForm
  | OKXForm
  | BitkubForm
  | BinanceTHForm
  | SettradeForm
  | WebullForm

const EMPTY_FORM: BinanceForm = { enabled: false, testnet: false, accounts: [] }

export function PortfolioConfigPage({
  exchangeName,
}: PortfolioConfigPageProps) {
  const { t } = useTranslation()
  const gateway = useAtomValue(gatewayAtom)

  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [fetchError, setFetchError] = useState("")
  const [serverError, setServerError] = useState("")

  const [baseForm, setBaseForm] = useState<AnyForm>(EMPTY_FORM)
  const [form, setForm] = useState<AnyForm>(EMPTY_FORM)

  const loadData = useCallback(async () => {
    if (
      !["binance", "okx", "bitkub", "binanceth", "settrade", "webull"].includes(
        exchangeName,
      )
    ) {
      setFetchError(t("portfolios.notFound", { name: exchangeName }))
      setLoading(false)
      return
    }

    setLoading(true)
    try {
      const appConfig = await getAppConfig()
      const exchangesData = asRecord(asRecord(appConfig).exchanges)

      let loaded: AnyForm
      if (exchangeName === "binance") {
        const d = asRecord(exchangesData.binance)
        loaded = {
          enabled: asBool(d.enabled),
          testnet: asBool(d.testnet),
          accounts: parseAccounts(d.accounts),
        } satisfies BinanceForm
      } else if (exchangeName === "okx") {
        const d = asRecord(exchangesData.okx)
        loaded = {
          enabled: asBool(d.enabled),
          testnet: asBool(d.testnet),
          accounts: parseOKXAccounts(d.accounts),
        } satisfies OKXForm
      } else if (exchangeName === "bitkub") {
        const d = asRecord(exchangesData.bitkub)
        loaded = {
          enabled: asBool(d.enabled),
          accounts: parseAccounts(d.accounts),
        } satisfies BitkubForm
      } else if (exchangeName === "settrade") {
        const d = asRecord(exchangesData.settrade)
        loaded = {
          enabled: asBool(d.enabled),
          accounts: parseSettradeAccounts(d.accounts),
        } satisfies SettradeForm
      } else if (exchangeName === "webull") {
        const d = asRecord(exchangesData.webull)
        loaded = {
          enabled: asBool(d.enabled),
          accounts: parseWebullAccounts(d.accounts),
        } satisfies WebullForm
      } else {
        const d = asRecord(exchangesData.binanceth)
        loaded = {
          enabled: asBool(d.enabled),
          accounts: parseAccounts(d.accounts),
        } satisfies BinanceTHForm
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
    void loadData()
  }, [loadData])

  const previousGatewayStatusRef = useRef(gateway.status)
  useEffect(() => {
    const previousStatus = previousGatewayStatusRef.current
    if (previousStatus !== "running" && gateway.status === "running") {
      void loadData()
    }
    previousGatewayStatusRef.current = gateway.status
  }, [gateway.status, loadData])

  const handleEnabledChange = (checked: boolean) =>
    setForm((prev) => ({ ...prev, enabled: checked }))

  const handleTestnetChange = (checked: boolean) =>
    setForm((prev) => ({ ...prev, testnet: checked }))

  const handleAccountChange = (
    index: number,
    patch: Partial<OKXAccountDraft & SettradeAccountDraft>,
  ) => {
    setForm((prev) => {
      const accounts = [...(prev as BinanceForm).accounts]
      accounts[index] = { ...accounts[index], ...patch } as OKXAccountDraft
      return { ...prev, accounts }
    })
  }

  const handleAddAccount = () => {
    setForm((prev) => {
      const isOKX = exchangeName === "okx"
      const isSettrade = exchangeName === "settrade"
      const isWebull = exchangeName === "webull"
      const accounts = [
        ...(prev as BinanceForm).accounts,
        isOKX
          ? emptyOKXAccount()
          : isSettrade
            ? emptySettradeAccount()
            : isWebull
              ? emptyWebullAccount()
              : emptyAccount(),
      ]
      return { ...prev, accounts }
    })
  }

  const handleRemoveAccount = (index: number) => {
    setForm((prev) => {
      const accounts = (prev as BinanceForm).accounts.filter(
        (_, i) => i !== index,
      )
      return { ...prev, accounts }
    })
  }

  const handleReset = () => {
    setForm(baseForm)
    setServerError("")
  }

  const handleSave = async () => {
    setSaving(true)
    setServerError("")
    try {
      if (exchangeName === "binance") {
        const f = form as BinanceForm
        await patchAppConfig({
          exchanges: {
            binance: {
              enabled: f.enabled,
              testnet: f.testnet,
              accounts: f.accounts.map(serializeAccount),
            },
          },
        })
      } else if (exchangeName === "okx") {
        const f = form as OKXForm
        await patchAppConfig({
          exchanges: {
            okx: {
              enabled: f.enabled,
              testnet: f.testnet,
              accounts: f.accounts.map(serializeOKXAccount),
            },
          },
        })
      } else if (exchangeName === "bitkub") {
        const f = form as BitkubForm
        await patchAppConfig({
          exchanges: {
            bitkub: {
              enabled: f.enabled,
              accounts: f.accounts.map(serializeAccount),
            },
          },
        })
      } else if (exchangeName === "binanceth") {
        const f = form as BinanceTHForm
        await patchAppConfig({
          exchanges: {
            binanceth: {
              enabled: f.enabled,
              accounts: f.accounts.map(serializeAccount),
            },
          },
        })
      } else if (exchangeName === "settrade") {
        const f = form as SettradeForm
        await patchAppConfig({
          exchanges: {
            settrade: {
              enabled: f.enabled,
              accounts: f.accounts.map(serializeSettradeAccount),
            },
          },
        })
      } else if (exchangeName === "webull") {
        const f = form as WebullForm
        await patchAppConfig({
          exchanges: {
            webull: {
              enabled: f.enabled,
              accounts: f.accounts.map(serializeWebullAccount),
            },
          },
        })
      }
      toast.success(t("portfolios.saveSuccess"))
      await loadData()
    } catch (e) {
      const message = e instanceof Error ? e.message : t("portfolios.saveError")
      setServerError(message)
      toast.error(message)
    } finally {
      setSaving(false)
    }
  }

  const displayName = getExchangeDisplayName(exchangeName)
  const docsUrl = getExchangeDocsUrl(exchangeName)
  const accounts = (form as BinanceForm).accounts
  const isConfigured = accounts.length > 0

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
            <div className="flex items-center gap-2 text-sm">
              <p className="font-medium">
                {t("portfolios.edit", { name: displayName })}
              </p>
              <a
                href={docsUrl}
                target="_blank"
                rel="noreferrer"
                className="text-muted-foreground hover:text-foreground text-xs underline underline-offset-2"
              >
                {t("portfolios.docLink")}
              </a>
            </div>

            {/* Exchange-level settings */}
            <div className="border-border/60 bg-background rounded-lg border">
              <div className="flex items-center justify-between px-4 py-3">
                <p className="text-sm font-medium">
                  {t("portfolios.enableLabel")}
                </p>
                <Switch
                  checked={form.enabled}
                  onCheckedChange={handleEnabledChange}
                />
              </div>

              {(exchangeName === "binance" || exchangeName === "okx") && (
                <div className="border-border/70 flex items-center justify-between border-t px-4 py-3">
                  <div>
                    <p className="text-sm font-medium">
                      {t("portfolios.binance.testnet")}
                    </p>
                    <p className="text-muted-foreground mt-0.5 text-xs">
                      {t("portfolios.binance.testnet_hint")}
                    </p>
                  </div>
                  <Switch
                    checked={form.testnet ?? false}
                    onCheckedChange={handleTestnetChange}
                  />
                </div>
              )}
            </div>

            {/* Accounts list */}
            <div className="space-y-3">
              <p className="text-sm font-medium">
                {t("portfolios.accounts", "Accounts")}
              </p>

              {accounts.map((acc, i) => (
                <AccountCard
                  key={i}
                  index={i}
                  account={acc}
                  hasPassphrase={exchangeName === "okx"}
                  isSettrade={exchangeName === "settrade"}
                  onChange={(patch) => handleAccountChange(i, patch)}
                  onRemove={() => handleRemoveAccount(i)}
                />
              ))}

              <Button
                variant="outline"
                size="sm"
                className="gap-1.5"
                onClick={handleAddAccount}
              >
                <IconPlus className="size-4" />
                {t("portfolios.addAccount", "Add Account")}
              </Button>
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
