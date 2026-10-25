import { IconLoader2 } from "@tabler/icons-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import {
  type ProfileField,
  type ProfileState,
  getProactiveProfile,
  putProactiveProfile,
} from "@/api/proactive"
import {
  LANGUAGES,
  RISK_LEVELS,
  STANDARD_TARGETS,
  type ProfileDraft,
  applyFieldReset,
  buildProfilePatch,
  draftFromProfile,
  isValidTimezone,
  parseBudget,
  parseTargets,
  profileProblems,
  timezoneOptions,
} from "@/components/proactive/profile-model"
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

const SELECT_CLASS =
  "border-input bg-background h-9 w-full rounded-md border px-3 py-2 text-sm"

function FieldHeader({
  htmlFor,
  label,
  onReset,
  disabled,
}: {
  htmlFor?: string
  label: string
  onReset: () => void
  disabled: boolean
}) {
  const { t } = useTranslation()
  return (
    <div className="flex items-center justify-between gap-2">
      <Label htmlFor={htmlFor}>{label}</Label>
      <Button
        type="button"
        variant="ghost"
        size="xs"
        className="text-muted-foreground h-6 px-1.5 text-xs"
        disabled={disabled}
        onClick={onReset}
      >
        {t("pages.agent.proactive.profile.reset_field")}
      </Button>
    </div>
  )
}

export function ProfileSection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()

  const {
    data: state,
    isLoading,
    error,
  } = useQuery({
    queryKey: ["proactive", "profile"],
    queryFn: getProactiveProfile,
  })

  // Edits live in a draft that exists only while the user is editing.
  const [draft, setDraft] = useState<ProfileDraft | null>(null)

  const store = (next: ProfileState) => {
    queryClient.setQueryData(["proactive", "profile"], next)
  }

  const save = useMutation({
    mutationFn: putProactiveProfile,
    onSuccess: (next) => {
      store(next)
      setDraft(null)
      toast.success(t("pages.agent.proactive.profile.save_success"))
    },
  })

  const reset = useMutation({
    mutationFn: (field: ProfileField) => putProactiveProfile({ reset: [field] }),
    onSuccess: (next, field) => {
      store(next)
      setDraft((prev) =>
        prev ? applyFieldReset(prev, draftFromProfile(next.profile), field) : prev,
      )
      toast.success(t("pages.agent.proactive.profile.reset_success"))
    },
    onError: (err: Error) => {
      toast.error(t("pages.agent.proactive.profile.reset_error"), {
        description: err.message,
      })
    },
  })

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">
          {t("pages.agent.proactive.profile.title")}
        </CardTitle>
        <CardDescription>
          {t("pages.agent.proactive.profile.description")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="text-muted-foreground flex items-center gap-2 py-4 text-sm">
            <IconLoader2 className="size-4 animate-spin" />
            {t("pages.agent.proactive.loading")}
          </div>
        ) : error || !state ? (
          <p className="text-destructive text-sm">
            {t("pages.agent.proactive.profile.load_error")}
          </p>
        ) : (
          <ProfileForm
            state={state}
            draft={draft ?? draftFromProfile(state.profile)}
            dirtyDraft={draft !== null}
            onChange={setDraft}
            onSave={(patch) => save.mutate(patch)}
            onDiscard={() => {
              setDraft(null)
              save.reset()
            }}
            onReset={(field) => reset.mutate(field)}
            saving={save.isPending}
            resetting={reset.isPending}
            serverError={save.error?.message ?? ""}
          />
        )}
      </CardContent>
    </Card>
  )
}

interface ProfileFormProps {
  state: ProfileState
  draft: ProfileDraft
  dirtyDraft: boolean
  onChange: (d: ProfileDraft) => void
  onSave: (patch: ReturnType<typeof buildProfilePatch>) => void
  onDiscard: () => void
  onReset: (field: ProfileField) => void
  saving: boolean
  resetting: boolean
  serverError: string
}

function ProfileForm({
  state,
  draft,
  dirtyDraft,
  onChange,
  onSave,
  onDiscard,
  onReset,
  saving,
  resetting,
  serverError,
}: ProfileFormProps) {
  const { t } = useTranslation()
  const set = (patch: Partial<ProfileDraft>) => onChange({ ...draft, ...patch })

  const problems = profileProblems(draft)
  const patch = buildProfilePatch(state.profile, draft)
  const hasChanges = Object.keys(patch).length > 0
  // The very first save is allowed even with no edits so the owner can keep the
  // defaults on purpose.
  const canSave = problems.length === 0 && (hasChanges || !state.has_profile) && !saving
  const targets = parseTargets(draft.targets)
  const extraTargetKeys = Object.keys(draft.targets).filter(
    (k) => !(STANDARD_TARGETS as readonly string[]).includes(k),
  )
  const busy = saving || resetting
  const problemText = (p: (typeof problems)[number]) =>
    t(`pages.agent.proactive.profile.problem_${p}`)

  return (
    <form
      className="space-y-5"
      onSubmit={(e) => {
        e.preventDefault()
        if (canSave) onSave(patch)
      }}
    >
      <p className="text-muted-foreground text-xs">
        {state.has_profile
          ? t("pages.agent.proactive.profile.meta", {
              version: state.version,
              by: state.updated_by,
              at: new Date(state.updated_at).toLocaleString(),
            })
          : t("pages.agent.proactive.profile.no_profile")}
      </p>

      {/* Time zone */}
      <div className="space-y-1.5">
        <FieldHeader
          htmlFor="proactive-tz"
          label={t("pages.agent.proactive.profile.timezone")}
          onReset={() => onReset("timezone")}
          disabled={busy || !state.has_profile}
        />
        <Input
          id="proactive-tz"
          list="proactive-timezones"
          value={draft.timezone}
          onChange={(e) => set({ timezone: e.target.value })}
          placeholder="Asia/Bangkok"
          autoComplete="off"
          aria-invalid={!isValidTimezone(draft.timezone)}
        />
        <datalist id="proactive-timezones">
          {timezoneOptions().map((z) => (
            <option key={z} value={z} />
          ))}
        </datalist>
        <p className="text-muted-foreground text-xs">
          {t("pages.agent.proactive.profile.timezone_hint")}
        </p>
      </div>

      {/* Active hours */}
      <div className="space-y-1.5">
        <FieldHeader
          label={t("pages.agent.proactive.profile.active_hours")}
          onReset={() => onReset("active_hours")}
          disabled={busy || !state.has_profile}
        />
        <div className="flex flex-wrap items-center gap-2">
          <Input
            type="time"
            className="w-32"
            aria-label={t("pages.agent.proactive.profile.hours_start")}
            value={draft.hoursStart}
            onChange={(e) => set({ hoursStart: e.target.value })}
          />
          <span className="text-muted-foreground text-sm">
            {t("pages.agent.proactive.profile.hours_to")}
          </span>
          <Input
            type="time"
            className="w-32"
            aria-label={t("pages.agent.proactive.profile.hours_end")}
            value={draft.hoursEnd}
            onChange={(e) => set({ hoursEnd: e.target.value })}
          />
        </div>
        <p className="text-muted-foreground text-xs">
          {t("pages.agent.proactive.profile.active_hours_hint")}
        </p>
      </div>

      <div className="grid gap-5 sm:grid-cols-2">
        {/* Language */}
        <div className="space-y-1.5">
          <FieldHeader
            htmlFor="proactive-language"
            label={t("pages.agent.proactive.profile.language")}
            onReset={() => onReset("language")}
            disabled={busy || !state.has_profile}
          />
          <select
            id="proactive-language"
            className={SELECT_CLASS}
            value={draft.language}
            onChange={(e) => set({ language: e.target.value })}
          >
            <option value="">{t("pages.agent.proactive.profile.not_set")}</option>
            {LANGUAGES.map((l) => (
              <option key={l} value={l}>
                {t(`pages.agent.proactive.profile.language_${l}`)}
              </option>
            ))}
          </select>
        </div>

        {/* Risk tolerance */}
        <div className="space-y-1.5">
          <FieldHeader
            htmlFor="proactive-risk"
            label={t("pages.agent.proactive.profile.risk")}
            onReset={() => onReset("risk_tolerance")}
            disabled={busy || !state.has_profile}
          />
          <select
            id="proactive-risk"
            className={SELECT_CLASS}
            value={draft.risk}
            onChange={(e) => set({ risk: e.target.value })}
          >
            <option value="">{t("pages.agent.proactive.profile.not_set")}</option>
            {RISK_LEVELS.map((r) => (
              <option key={r} value={r}>
                {t(`pages.agent.proactive.profile.risk_${r}`)}
              </option>
            ))}
          </select>
        </div>
      </div>

      {/* Allocation targets */}
      <div className="space-y-1.5">
        <FieldHeader
          label={t("pages.agent.proactive.profile.targets")}
          onReset={() => onReset("allocation_targets")}
          disabled={busy || !state.has_profile}
        />
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          {[...STANDARD_TARGETS, ...extraTargetKeys].map((key) => (
            <div key={key} className="space-y-1">
              <Label htmlFor={`proactive-target-${key}`} className="text-xs font-normal">
                {t(`pages.agent.proactive.profile.target_${key}`, key)}
              </Label>
              <Input
                id={`proactive-target-${key}`}
                type="number"
                min={0}
                max={100}
                step="any"
                placeholder="%"
                value={draft.targets[key] ?? ""}
                onChange={(e) =>
                  set({ targets: { ...draft.targets, [key]: e.target.value } })
                }
                aria-invalid={targets.invalid.includes(key) || targets.sum > 100}
              />
            </div>
          ))}
        </div>
        <p
          className={
            targets.sum > 100
              ? "text-destructive text-xs"
              : "text-muted-foreground text-xs"
          }
        >
          {t("pages.agent.proactive.profile.targets_total", { sum: targets.sum })}
        </p>
      </div>

      {/* Assets to avoid */}
      <div className="space-y-1.5">
        <FieldHeader
          htmlFor="proactive-avoid"
          label={t("pages.agent.proactive.profile.avoid")}
          onReset={() => onReset("assets_to_avoid")}
          disabled={busy || !state.has_profile}
        />
        <Input
          id="proactive-avoid"
          value={draft.avoid}
          onChange={(e) => set({ avoid: e.target.value })}
          placeholder="DOGE, SHIB"
          autoComplete="off"
        />
        <p className="text-muted-foreground text-xs">
          {t("pages.agent.proactive.profile.avoid_hint")}
        </p>
      </div>

      <div className="grid gap-5 sm:grid-cols-2">
        {/* Notification budget */}
        <div className="space-y-1.5">
          <FieldHeader
            htmlFor="proactive-budget"
            label={t("pages.agent.proactive.profile.budget")}
            onReset={() => onReset("notification_budget_per_day")}
            disabled={busy || !state.has_profile}
          />
          <Input
            id="proactive-budget"
            type="number"
            min={0}
            step={1}
            value={draft.budget}
            onChange={(e) => set({ budget: e.target.value })}
            aria-invalid={parseBudget(draft.budget) === null}
          />
          <p className="text-muted-foreground text-xs">
            {t("pages.agent.proactive.profile.budget_hint")}
          </p>
        </div>

        {/* Digest time */}
        <div className="space-y-1.5">
          <FieldHeader
            htmlFor="proactive-digest-time"
            label={t("pages.agent.proactive.profile.digest_time")}
            onReset={() => onReset("digest_time")}
            disabled={busy || !state.has_profile}
          />
          <Input
            id="proactive-digest-time"
            type="time"
            className="w-32"
            value={draft.digestTime}
            onChange={(e) => set({ digestTime: e.target.value })}
          />
          <p className="text-muted-foreground text-xs">
            {t("pages.agent.proactive.profile.digest_time_hint")}
          </p>
        </div>
      </div>

      {problems.length > 0 && (
        <ul className="space-y-0.5 text-xs text-amber-600 dark:text-amber-400">
          {problems.map((p) => (
            <li key={p}>{problemText(p)}</li>
          ))}
        </ul>
      )}
      {serverError !== "" && (
        <p role="alert" className="text-destructive text-sm whitespace-pre-wrap">
          {serverError}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="sm" disabled={!canSave} className="gap-1.5">
          {saving && <IconLoader2 className="size-3.5 animate-spin" />}
          {t("common.save")}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={!dirtyDraft || saving}
          onClick={onDiscard}
        >
          {t("pages.agent.proactive.profile.discard")}
        </Button>
      </div>
    </form>
  )
}
