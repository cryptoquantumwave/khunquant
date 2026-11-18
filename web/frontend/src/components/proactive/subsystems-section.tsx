import { IconChevronDown, IconLoader2 } from "@tabler/icons-react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import {
  type ProactiveStatus,
  type SubsystemsInput,
  putProactiveSubsystems,
} from "@/api/proactive"
import { SwitchCardField } from "@/components/shared-form"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

const TRIGGER_MODES = ["off", "shadow", "live"] as const

const SELECT_CLASS =
  "border-input bg-background h-9 w-full rounded-md border px-3 py-2 text-sm"

const T = "pages.agent.proactive.subsystems"

type Changes = Record<string, boolean | number | string>

interface Field {
  key: string
  kind: "number" | "text"
  label: string
  hint?: string
  placeholder?: string
}

interface Draft {
  enabled?: boolean
  mode?: string
  fields: Record<string, string>
}

interface GroupProps {
  id: string
  title: string
  description: string
  // The group's switch, when it has one.
  enabled?: { value: boolean; label: string; hint: string }
  // The group's mode select (triggers only).
  mode?: {
    value: string
    label: string
    hint: string
    options: readonly string[]
    optionLabel: (m: string) => string
  }
  // The numbers and text in the collapsed "Advanced" area.
  fields: Field[]
  saved: Record<string, string | number | boolean>
  // What applies right now, and whether the group is in effect at all.
  inEffect: boolean
  effectiveText: string
  // Sentences for the unmet requirements of this group.
  notes: string[]
  build: (changes: Changes) => SubsystemsInput
  // When set, turning the switch off asks first.
  confirmOff?: { title: string; description: string; confirm: string }
}

function parseWhole(raw: string): number | null {
  if (!/^\d+$/.test(raw.trim())) return null
  return Number(raw.trim())
}

function Group(props: GroupProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState<Draft | null>(null)
  const [open, setOpen] = useState(false)
  const [confirming, setConfirming] = useState<Changes | null>(null)

  const fieldValue = (f: Field) =>
    draft?.fields[f.key] ?? String(props.saved[f.key] ?? "")
  const enabled = draft?.enabled ?? props.enabled?.value ?? false
  const mode = draft?.mode ?? props.mode?.value ?? ""

  const edit = (patch: Partial<Draft>) =>
    setDraft({ fields: {}, ...draft, ...patch })
  const editField = (key: string, value: string) =>
    setDraft({
      ...draft,
      fields: { ...draft?.fields, [key]: value },
    })

  // changes is only what differs from what is saved: the server takes a partial update.
  const changes: Changes = {}
  const invalid: string[] = []
  if (props.enabled && enabled !== props.enabled.value)
    changes.enabled = enabled
  if (props.mode && mode !== props.mode.value) changes.mode = mode
  for (const f of props.fields) {
    const raw = fieldValue(f)
    if (f.kind === "text") {
      if (raw.trim() !== String(props.saved[f.key] ?? "")) changes[f.key] = raw
      continue
    }
    const n = parseWhole(raw)
    if (n === null) invalid.push(f.key)
    else if (n !== props.saved[f.key]) changes[f.key] = n
  }
  const dirty = Object.keys(changes).length > 0

  const save = useMutation({
    mutationFn: (c: Changes) => putProactiveSubsystems(props.build(c)),
    onSuccess: (next: ProactiveStatus) => {
      queryClient.setQueryData(["proactive", "status"], next)
      setDraft(null)
      toast.success(t(`${T}.save_success`, { name: props.title }))
    },
  })

  const submit = () => {
    if (props.confirmOff && props.enabled?.value && changes.enabled === false) {
      setConfirming(changes)
      return
    }
    save.mutate(changes)
  }

  return (
    <div className="space-y-3 rounded-lg border p-4" data-group={props.id}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <p className="text-sm font-semibold">{props.title}</p>
          <p className="text-muted-foreground text-xs">{props.description}</p>
        </div>
        <span
          className={
            props.inEffect
              ? "rounded-full bg-emerald-100 px-2 py-0.5 text-xs text-emerald-800 dark:bg-emerald-950 dark:text-emerald-200"
              : "bg-muted text-muted-foreground rounded-full px-2 py-0.5 text-xs"
          }
        >
          {props.inEffect
            ? t(`${T}.in_effect_on`, { value: props.effectiveText })
            : t(`${T}.in_effect_off`, { value: props.effectiveText })}
        </span>
      </div>

      {props.enabled && (
        <SwitchCardField
          label={props.enabled.label}
          hint={props.enabled.hint}
          checked={enabled}
          onCheckedChange={(v) => edit({ enabled: v })}
        />
      )}

      {props.mode && (
        <div className="space-y-1.5">
          <Label htmlFor={`proactive-${props.id}-mode`}>
            {props.mode.label}
          </Label>
          <select
            id={`proactive-${props.id}-mode`}
            className={SELECT_CLASS}
            value={mode}
            onChange={(e) => edit({ mode: e.target.value })}
          >
            {!props.mode.options.includes(mode) && (
              <option value={mode}>{mode}</option>
            )}
            {props.mode.options.map((m) => (
              <option key={m} value={m}>
                {props.mode?.optionLabel(m)}
              </option>
            ))}
          </select>
          <p className="text-muted-foreground text-xs">{props.mode.hint}</p>
        </div>
      )}

      {props.notes.length > 0 && (
        <ul className="list-disc space-y-0.5 pl-4 text-xs text-amber-700 dark:text-amber-400">
          {props.notes.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}

      {props.fields.length > 0 && (
        <Collapsible open={open} onOpenChange={setOpen}>
          <CollapsibleTrigger asChild>
            <Button variant="ghost" size="sm" className="gap-1 px-1">
              <IconChevronDown
                className={`size-4 transition-transform ${open ? "rotate-180" : ""}`}
              />
              {t(`${T}.advanced`)}
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent className="mt-2 grid gap-4 sm:grid-cols-2">
            {props.fields.map((f) => {
              const bad = invalid.includes(f.key)
              return (
                <div key={f.key} className="space-y-1.5">
                  <Label htmlFor={`proactive-${props.id}-${f.key}`}>
                    {f.label}
                  </Label>
                  <Input
                    id={`proactive-${props.id}-${f.key}`}
                    type={f.kind === "number" ? "number" : "text"}
                    min={f.kind === "number" ? 0 : undefined}
                    step={f.kind === "number" ? 1 : undefined}
                    placeholder={f.placeholder}
                    value={fieldValue(f)}
                    onChange={(e) => editField(f.key, e.target.value)}
                    aria-invalid={bad}
                  />
                  <p
                    className={
                      bad
                        ? "text-destructive text-xs"
                        : "text-muted-foreground text-xs"
                    }
                  >
                    {bad ? t(`${T}.whole_number`) : f.hint}
                  </p>
                </div>
              )
            })}
          </CollapsibleContent>
        </Collapsible>
      )}

      {save.error && (
        <p
          role="alert"
          className="text-destructive text-sm whitespace-pre-wrap"
        >
          {save.error.message}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          disabled={!dirty || invalid.length > 0 || save.isPending}
          onClick={submit}
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

      {props.confirmOff && (
        <AlertDialog
          open={confirming !== null}
          onOpenChange={(o) => {
            if (!o) setConfirming(null)
          }}
        >
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{props.confirmOff.title}</AlertDialogTitle>
              <AlertDialogDescription>
                {props.confirmOff.description}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
              <AlertDialogAction
                onClick={() => {
                  if (confirming) save.mutate(confirming)
                  setConfirming(null)
                }}
              >
                {props.confirmOff.confirm}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  )
}

// SubsystemsSection is the controls for the proactive sub-systems the Modes card does
// not cover: perception, triggers, approvals, cognition and news. Each group saves on
// its own through PUT /api/agent/proactive/subsystems; the server judges every number
// with the config package's own checks and its message is shown as it is.
export function SubsystemsSection({ status }: { status: ProactiveStatus }) {
  const { t } = useTranslation()
  const sub = status.subsystems
  const c = sub.configured
  const e = sub.effective

  const note = (code: string) => t(`${T}.note_${code}`)
  const has = (code: string) => sub.notes.includes(code)
  const only = (...codes: string[]) =>
    codes.filter((code) => has(code)).map(note)

  const onOff = (v: boolean) => t(v ? `${T}.on` : `${T}.off`)

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">{t(`${T}.title`)}</CardTitle>
        <CardDescription>{t(`${T}.description`)}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-muted-foreground text-xs">
          {t(`${T}.reload_notice`)}
        </p>

        <p className="text-sm">
          {status.paper_trading_mode ? t(`${T}.paper_on`) : t(`${T}.paper_off`)}{" "}
          <Link to="/config" className="text-primary underline">
            {t(`${T}.paper_link`)}
          </Link>
        </p>

        {has("proactive_off") && (
          <p className="text-xs text-amber-700 dark:text-amber-400">
            {note("proactive_off")}
          </p>
        )}

        <Group
          id="perception"
          title={t(`${T}.perception.title`)}
          description={t(`${T}.perception.description`)}
          enabled={{
            value: c.perception.enabled,
            label: t(`${T}.perception.enabled`),
            hint: t(`${T}.perception.enabled_hint`),
          }}
          fields={[
            {
              key: "market_poll_seconds",
              kind: "number",
              label: t(`${T}.perception.market_poll_seconds`),
              hint: t(`${T}.perception.market_poll_seconds_hint`),
            },
            {
              key: "reads_per_minute_limit",
              kind: "number",
              label: t(`${T}.perception.reads_per_minute_limit`),
              hint: t(`${T}.perception.reads_per_minute_limit_hint`),
            },
            {
              key: "snapshot_minutes",
              kind: "number",
              label: t(`${T}.perception.snapshot_minutes`),
              hint: t(`${T}.perception.snapshot_minutes_hint`),
            },
          ]}
          saved={{ ...c.perception }}
          inEffect={e.perception}
          effectiveText={onOff(e.perception)}
          notes={[]}
          build={(c) => ({ perception: c as SubsystemsInput["perception"] })}
        />

        <Group
          id="triggers"
          title={t(`${T}.triggers.title`)}
          description={t(`${T}.triggers.description`)}
          mode={{
            value: c.triggers.mode,
            label: t(`${T}.triggers.mode`),
            hint: t(`${T}.triggers.mode_hint`),
            options: TRIGGER_MODES,
            optionLabel: (m) => t(`${T}.triggers.mode_${m}`, m),
          }}
          fields={[]}
          saved={{}}
          inEffect={e.triggers_mode !== "off"}
          effectiveText={e.triggers_mode}
          notes={only("triggers_need_perception")}
          build={(c) => ({ triggers: c as SubsystemsInput["triggers"] })}
        />

        <Group
          id="approvals"
          title={t(`${T}.approvals.title`)}
          description={t(`${T}.approvals.description`)}
          enabled={{
            value: c.approvals.enabled,
            label: t(`${T}.approvals.enabled`),
            hint: t(`${T}.approvals.enabled_hint`),
          }}
          fields={[
            {
              key: "expiry_minutes",
              kind: "number",
              label: t(`${T}.approvals.expiry_minutes`),
              hint: t(`${T}.approvals.expiry_minutes_hint`),
            },
          ]}
          saved={{ expiry_minutes: c.approvals.expiry_minutes }}
          inEffect={e.approvals}
          effectiveText={onOff(e.approvals)}
          notes={[]}
          confirmOff={{
            title: t(`${T}.approvals.confirm_off_title`),
            description: t(`${T}.approvals.confirm_off_description`),
            confirm: t(`${T}.approvals.confirm_off`),
          }}
          build={(c) => ({ approvals: c as SubsystemsInput["approvals"] })}
        />

        <Group
          id="cognition"
          title={t(`${T}.cognition.title`)}
          description={t(`${T}.cognition.description`)}
          enabled={{
            value: c.cognition.enabled,
            label: t(`${T}.cognition.enabled`),
            hint: t(`${T}.cognition.enabled_hint`),
          }}
          fields={[
            {
              key: "wakes_per_hour",
              kind: "number",
              label: t(`${T}.cognition.wakes_per_hour`),
              hint: t(`${T}.cognition.wakes_per_hour_hint`),
            },
            {
              key: "tokens_per_day",
              kind: "number",
              label: t(`${T}.cognition.tokens_per_day`),
              hint: t(`${T}.cognition.tokens_per_day_hint`),
            },
            {
              key: "max_concurrent_llm",
              kind: "number",
              label: t(`${T}.cognition.max_concurrent_llm`),
              hint: t(`${T}.cognition.max_concurrent_llm_hint`),
            },
            {
              key: "proactive_model",
              kind: "text",
              label: t(`${T}.cognition.proactive_model`),
              hint: t(`${T}.cognition.proactive_model_hint`),
              placeholder: t(`${T}.cognition.proactive_model_placeholder`),
            },
          ]}
          saved={{ ...c.cognition }}
          inEffect={e.cognition}
          effectiveText={onOff(e.cognition)}
          notes={[]}
          build={(c) => ({ cognition: c as SubsystemsInput["cognition"] })}
        />

        <Group
          id="news"
          title={t(`${T}.news.title`)}
          description={t(`${T}.news.description`)}
          enabled={{
            value: c.news.enabled,
            label: t(`${T}.news.enabled`),
            hint: t(`${T}.news.enabled_hint`),
          }}
          fields={[
            {
              key: "interval_minutes",
              kind: "number",
              label: t(`${T}.news.interval_minutes`),
              hint: t(`${T}.news.interval_minutes_hint`, {
                min: sub.limits.news_min_interval_minutes,
              }),
            },
            {
              key: "max_assets",
              kind: "number",
              label: t(`${T}.news.max_assets`),
              hint: t(`${T}.news.max_assets_hint`, {
                max: sub.limits.news_max_assets_cap,
              }),
            },
            {
              key: "max_parses_per_run",
              kind: "number",
              label: t(`${T}.news.max_parses_per_run`),
              hint: t(`${T}.news.max_parses_per_run_hint`),
            },
            {
              key: "max_parses_per_day",
              kind: "number",
              label: t(`${T}.news.max_parses_per_day`),
              hint: t(`${T}.news.max_parses_per_day_hint`),
            },
            {
              key: "run_seconds",
              kind: "number",
              label: t(`${T}.news.run_seconds`),
              hint: t(`${T}.news.run_seconds_hint`),
            },
          ]}
          saved={{ ...c.news }}
          inEffect={e.news}
          effectiveText={onOff(e.news)}
          notes={only(
            "news_needs_cognition",
            "news_needs_triggers",
            "news_needs_web_search",
          )}
          build={(c) => ({ news: c as SubsystemsInput["news"] })}
        />
      </CardContent>
    </Card>
  )
}
