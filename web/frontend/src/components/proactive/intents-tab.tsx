import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import {
  type IntentAction,
  type IntentRow,
  actOnIntent,
  getIntents,
} from "@/api/proactive-manage"
import {
  ConfirmButton,
  ManageQuery,
  WebOwnerNotice,
} from "@/components/proactive/manage-shared"
import {
  MANAGE_QUERY_KEY,
  MANAGE_REFRESH_MS,
  formatTime,
  useManageAction,
} from "@/components/proactive/manage-utils"

const SOURCES = ["explicit", "inferred", "reflection"]
const STATES = ["proposed", "active", "paused"]

export function IntentsTab() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: [...MANAGE_QUERY_KEY, "intents"],
    queryFn: getIntents,
    refetchInterval: MANAGE_REFRESH_MS,
  })
  const act = useManageAction(
    ({ id, action }: { id: string; action: IntentAction }) =>
      actOnIntent(id, action),
  )

  return (
    <ManageQuery query={query}>
      {(data) => {
        const ownerOk = data.web_owner_active === true
        const reason = data.web_owner_missing ?? ""
        const rows = data.intents ?? []
        return (
          <div className="space-y-3">
            {!ownerOk && rows.some((r) => r.status !== "active") && (
              <WebOwnerNotice missing={reason} />
            )}
            {rows.length === 0 ? (
              <p className="text-muted-foreground py-6 text-center text-sm">
                {t("pages.agent.proactive.manage.intents.empty")}
              </p>
            ) : (
              rows.map((row) => (
                <IntentCard
                  key={row.id}
                  row={row}
                  ownerOk={ownerOk}
                  reason={reason}
                  pending={act.isPending && act.variables?.id === row.id}
                  onAct={(action) => act.mutate({ id: row.id, action })}
                />
              ))
            )}
          </div>
        )
      }}
    </ManageQuery>
  )
}

function IntentCard({
  row,
  ownerOk,
  reason,
  pending,
  onAct,
}: {
  row: IntentRow
  ownerOk: boolean
  reason: string
  pending: boolean
  onAct: (action: IntentAction) => void
}) {
  const { t } = useTranslation()
  const m = (key: string, opts?: Record<string, unknown>) =>
    t(`pages.agent.proactive.manage.intents.${key}`, opts)
  const blocked = t("pages.agent.proactive.manage.web_owner_blocked", {
    reason,
  })
  const source = SOURCES.includes(row.source)
    ? m(`source.${row.source}`)
    : row.source
  const state = STATES.includes(row.status)
    ? m(`state.${row.status}`)
    : row.status

  return (
    <div className="rounded-lg border p-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          {/* The title is owner- or model-written: a plain text node, nothing else. */}
          <p className="text-sm font-medium break-words">{row.title}</p>
          <dl className="text-muted-foreground mt-1.5 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs">
            <dt>{m("kind")}</dt>
            <dd>{row.type}</dd>
            <dt>{m("status")}</dt>
            <dd>{state}</dd>
            <dt>{m("created_by")}</dt>
            <dd>
              {source}
              {row.via_web ? ` (${m("via_web")})` : ""}
            </dd>
            <dt>{m("expires")}</dt>
            <dd>{row.expires_at ? formatTime(row.expires_at) : m("never")}</dd>
          </dl>
          <p className="text-muted-foreground mt-1 text-xs">
            {m("triggers", { count: row.triggers })}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          {row.status === "proposed" && (
            <ConfirmButton
              label={m("confirm_intent")}
              title={m("confirm_intent")}
              description={m("confirm_description")}
              detail={<p className="text-sm break-words">{row.title}</p>}
              variant="default"
              disabled={!ownerOk}
              disabledReason={blocked}
              pending={pending}
              onConfirm={() => onAct("confirm")}
            />
          )}
          {row.status === "paused" && (
            <ConfirmButton
              label={m("resume")}
              title={m("resume")}
              description={m("resume_description")}
              detail={<p className="text-sm break-words">{row.title}</p>}
              disabled={!ownerOk}
              disabledReason={blocked}
              pending={pending}
              onConfirm={() => onAct("resume")}
            />
          )}
          {row.status === "active" && (
            <ConfirmButton
              label={m("pause")}
              title={m("pause")}
              description={m("pause_description")}
              detail={<p className="text-sm break-words">{row.title}</p>}
              pending={pending}
              onConfirm={() => onAct("pause")}
            />
          )}
          <ConfirmButton
            label={m("cancel_intent")}
            title={m("cancel_intent")}
            description={m("cancel_description")}
            detail={<p className="text-sm break-words">{row.title}</p>}
            variant="destructive"
            pending={pending}
            onConfirm={() => onAct("cancel")}
          />
        </div>
      </div>
    </div>
  )
}
