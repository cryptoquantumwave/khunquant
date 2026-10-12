import { IconLoader2 } from "@tabler/icons-react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import {
  type ModesInput,
  type ProactiveStatus,
  putProactiveModes,
} from "@/api/proactive"
import { SwitchCardField } from "@/components/shared-form"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

const HEARTBEAT_MODES = ["legacy", "governed", "sweep"] as const
const ATTENTION_MODES = ["off", "shadow", "live"] as const

const SELECT_CLASS =
  "border-input bg-background h-9 w-full rounded-md border px-3 py-2 text-sm"

interface Draft {
  enabled: boolean
  heartbeatMode: string
  attentionMode: string
  dailyBudget: string
  digest: boolean
}

function fromStatus(s: ProactiveStatus): Draft {
  return {
    enabled: s.enabled,
    heartbeatMode: s.configured.heartbeat_mode,
    attentionMode: s.configured.attention_mode,
    dailyBudget: String(s.configured.daily_budget),
    digest: s.configured.digest,
  }
}

function parseBudget(raw: string): number | null {
  if (!/^\d+$/.test(raw.trim())) return null
  return Number(raw.trim())
}

// diff returns only the settings that differ from the saved ones, which is what
// the server's partial update expects.
function diff(saved: ProactiveStatus, d: Draft): ModesInput {
  const base = fromStatus(saved)
  const out: ModesInput = {}
  if (d.enabled !== base.enabled) out.enabled = d.enabled
  if (d.heartbeatMode !== base.heartbeatMode) out.heartbeat_mode = d.heartbeatMode
  if (d.attentionMode !== base.attentionMode) out.attention_mode = d.attentionMode
  const budget = parseBudget(d.dailyBudget)
  if (budget !== null && budget !== saved.configured.daily_budget) {
    out.daily_budget = budget
  }
  if (d.digest !== base.digest) out.digest = d.digest
  return out
}

export function ModesSection({ status }: { status: ProactiveStatus }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()

  // The draft only exists while the user is editing; otherwise the form shows
  // what the server has.
  const [draft, setDraft] = useState<Draft | null>(null)
  const current = draft ?? fromStatus(status)
  const edit = (patch: Partial<Draft>) => setDraft({ ...current, ...patch })

  const save = useMutation({
    mutationFn: putProactiveModes,
    onSuccess: (next) => {
      queryClient.setQueryData(["proactive", "status"], next)
      setDraft(null)
      toast.success(t("pages.agent.proactive.modes.save_success"))
    },
  })

  const budgetValid = parseBudget(current.dailyBudget) !== null
  const changes = diff(status, current)
  const dirty = Object.keys(changes).length > 0

  const modeLabel = (kind: "heartbeat" | "attention", mode: string) =>
    t(`pages.agent.proactive.modes.${kind}_${mode}`, mode)

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">
          {t("pages.agent.proactive.modes.title")}
        </CardTitle>
        <CardDescription>
          {t("pages.agent.proactive.modes.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <SwitchCardField
          label={t("pages.agent.proactive.modes.enabled")}
          hint={t("pages.agent.proactive.modes.enabled_hint")}
          checked={current.enabled}
          onCheckedChange={(v) => edit({ enabled: v })}
        />

        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="proactive-heartbeat-mode">
              {t("pages.agent.proactive.modes.heartbeat")}
            </Label>
            <select
              id="proactive-heartbeat-mode"
              className={SELECT_CLASS}
              value={current.heartbeatMode}
              onChange={(e) => edit({ heartbeatMode: e.target.value })}
            >
              {!HEARTBEAT_MODES.some((m) => m === current.heartbeatMode) && (
                <option value={current.heartbeatMode}>{current.heartbeatMode}</option>
              )}
              {HEARTBEAT_MODES.map((m) => (
                <option key={m} value={m}>
                  {modeLabel("heartbeat", m)}
                </option>
              ))}
            </select>
            <p className="text-muted-foreground text-xs">
              {t("pages.agent.proactive.modes.heartbeat_hint")}
            </p>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="proactive-attention-mode">
              {t("pages.agent.proactive.modes.attention")}
            </Label>
            <select
              id="proactive-attention-mode"
              className={SELECT_CLASS}
              value={current.attentionMode}
              onChange={(e) => edit({ attentionMode: e.target.value })}
            >
              {!ATTENTION_MODES.some((m) => m === current.attentionMode) && (
                <option value={current.attentionMode}>{current.attentionMode}</option>
              )}
              {ATTENTION_MODES.map((m) => (
                <option key={m} value={m}>
                  {modeLabel("attention", m)}
                </option>
              ))}
            </select>
            <p className="text-muted-foreground text-xs">
              {t("pages.agent.proactive.modes.attention_hint")}
            </p>
          </div>
        </div>

        <div className="space-y-1.5">
          <Label htmlFor="proactive-daily-budget">
            {t("pages.agent.proactive.modes.daily_budget")}
          </Label>
          <Input
            id="proactive-daily-budget"
            type="number"
            min={0}
            step={1}
            className="max-w-40"
            value={current.dailyBudget}
            onChange={(e) => edit({ dailyBudget: e.target.value })}
            aria-invalid={!budgetValid}
          />
          <p
            className={
              budgetValid
                ? "text-muted-foreground text-xs"
                : "text-destructive text-xs"
            }
          >
            {budgetValid
              ? t("pages.agent.proactive.modes.daily_budget_hint")
              : t("pages.agent.proactive.modes.daily_budget_invalid")}
          </p>
        </div>

        <SwitchCardField
          label={t("pages.agent.proactive.modes.digest")}
          hint={t("pages.agent.proactive.modes.digest_hint")}
          checked={current.digest}
          onCheckedChange={(v) => edit({ digest: v })}
        />

        {save.error && (
          <p role="alert" className="text-destructive text-sm whitespace-pre-wrap">
            {save.error.message}
          </p>
        )}

        <div className="flex flex-wrap gap-2">
          <Button
            size="sm"
            disabled={!dirty || !budgetValid || save.isPending}
            onClick={() => save.mutate(changes)}
            className="gap-1.5"
          >
            {save.isPending && <IconLoader2 className="size-3.5 animate-spin" />}
            {t("common.save")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={draft === null || save.isPending}
            onClick={() => {
              setDraft(null)
              save.reset()
            }}
          >
            {t("common.cancel")}
          </Button>
        </div>

        <div className="rounded-lg border p-3 text-sm">
          <div className="text-muted-foreground mb-2 text-xs">
            {t("pages.agent.proactive.modes.in_effect")}
          </div>
          <table className="w-full text-xs">
            <thead>
              <tr className="text-muted-foreground text-left">
                <th className="py-1 font-normal" />
                <th className="py-1 font-normal">
                  {t("pages.agent.proactive.modes.configured")}
                </th>
                <th className="py-1 font-normal">
                  {t("pages.agent.proactive.modes.effective")}
                </th>
              </tr>
            </thead>
            <tbody className="font-mono">
              <tr>
                <td className="py-1 font-sans">
                  {t("pages.agent.proactive.modes.heartbeat")}
                </td>
                <td>{status.configured.heartbeat_mode}</td>
                <td>{status.effective.heartbeat_mode}</td>
              </tr>
              <tr>
                <td className="py-1 font-sans">
                  {t("pages.agent.proactive.modes.attention")}
                </td>
                <td>{status.configured.attention_mode}</td>
                <td>{status.effective.attention_mode}</td>
              </tr>
            </tbody>
          </table>
          <p className="text-muted-foreground mt-2 text-xs">
            {status.effective.can_deliver
              ? t("pages.agent.proactive.modes.can_deliver")
              : t("pages.agent.proactive.modes.cannot_deliver")}
          </p>
          {status.downgrades.length > 0 && (
            <div className="mt-2">
              <p className="text-xs font-medium text-amber-600 dark:text-amber-400">
                {t("pages.agent.proactive.modes.downgrades")}
              </p>
              <ul className="text-muted-foreground mt-1 list-disc space-y-0.5 pl-4 text-xs">
                {status.downgrades.map((d) => (
                  <li key={d}>{d}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
