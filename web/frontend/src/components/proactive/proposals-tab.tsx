import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import {
  type ProposalRow,
  decideProposal,
  getProposals,
} from "@/api/proactive-manage"
import {
  ConfirmButton,
  ManageQuery,
  WebOwnerNotice,
} from "@/components/proactive/manage-shared"
import {
  MANAGE_QUERY_KEY,
  MANAGE_REFRESH_MS,
  formatCountdown,
  useManageAction,
  useNow,
} from "@/components/proactive/manage-utils"

const KINDS = [
  "order",
  "mandate",
  "memory",
  "trigger_param",
  "intent_cleanup",
  "mute",
]

export function ProposalsTab() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: [...MANAGE_QUERY_KEY, "proposals"],
    queryFn: getProposals,
    refetchInterval: MANAGE_REFRESH_MS,
  })
  const decide = useManageAction(
    ({ code, action }: { code: string; action: "approve" | "reject" }) =>
      decideProposal(code, action),
  )

  return (
    <ManageQuery query={query}>
      {(data) => {
        const ownerOk = data.web_owner_active === true
        const reason = data.web_owner_missing ?? ""
        const rows = data.proposals ?? []
        return (
          <div className="space-y-3">
            {!ownerOk && rows.length > 0 && <WebOwnerNotice missing={reason} />}
            {rows.length === 0 ? (
              <p className="text-muted-foreground py-6 text-center text-sm">
                {t("pages.agent.proactive.manage.proposals.empty")}
              </p>
            ) : (
              rows.map((row) => (
                <ProposalCard
                  key={row.code}
                  row={row}
                  fetchedAt={query.dataUpdatedAt}
                  ownerOk={ownerOk}
                  reason={reason}
                  pending={
                    decide.isPending && decide.variables?.code === row.code
                  }
                  onDecide={(action) =>
                    decide.mutate({ code: row.code, action })
                  }
                />
              ))
            )}
          </div>
        )
      }}
    </ManageQuery>
  )
}

function ProposalCard({
  row,
  fetchedAt,
  ownerOk,
  reason,
  pending,
  onDecide,
}: {
  row: ProposalRow
  fetchedAt: number
  ownerOk: boolean
  reason: string
  pending: boolean
  onDecide: (action: "approve" | "reject") => void
}) {
  const { t } = useTranslation()
  const m = (key: string, opts?: Record<string, unknown>) =>
    t(`pages.agent.proactive.manage.proposals.${key}`, opts)
  const now = useNow()
  // The countdown only shows the server's figure running down since it was fetched;
  // the gateway decides whether a proposal is still open.
  const left = row.expires_in_s - (now - fetchedAt) / 1000
  const kind = KINDS.includes(row.kind) ? m(`kind.${row.kind}`) : row.kind
  const blocked = t("pages.agent.proactive.manage.web_owner_blocked", {
    reason,
  })
  const isPending = row.status === "pending"

  // The code-rendered summary, shown again wherever a decision is confirmed.
  const summary = (
    <p className="text-sm font-medium break-words">{row.summary}</p>
  )

  return (
    <div className="rounded-lg border p-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1 space-y-1.5">
          <div className="text-muted-foreground flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            <span className="bg-muted rounded-full px-2 py-0.5 font-medium">
              {kind}
            </span>
            <span className="font-mono">
              {m("code")} {row.code}
            </span>
            <span>{isPending ? m("pending") : row.status}</span>
            <span>
              {left > 0
                ? m("expires_in", { time: formatCountdown(left) })
                : m("expired")}
            </span>
            {row.reducing && <span>{m("reducing")}</span>}
          </div>
          {summary}
          {row.quote && (
            <Quote
              label={t(
                row.quote_kind === "fact"
                  ? "pages.agent.proactive.manage.quote_fact"
                  : "pages.agent.proactive.manage.quote_rationale",
              )}
              text={row.quote}
            />
          )}
        </div>
        {isPending && (
          <div className="flex flex-wrap gap-2">
            <ConfirmButton
              label={m("approve")}
              title={m("approve_title")}
              description={m("approve_description")}
              detail={summary}
              variant="default"
              disabled={!ownerOk}
              disabledReason={blocked}
              pending={pending}
              onConfirm={() => onDecide("approve")}
            />
            <ConfirmButton
              label={m("reject")}
              title={m("reject_title")}
              description={m("reject_description")}
              detail={summary}
              variant="destructive"
              disabled={!ownerOk}
              disabledReason={blocked}
              pending={pending}
              onConfirm={() => onDecide("reject")}
            />
          </div>
        )}
      </div>
    </div>
  )
}

// Quote shows text a model wrote, as a clearly labelled quotation. It is a plain
// text node: never markup, never markdown, never a link.
function Quote({ label, text }: { label: string; text: string }) {
  const { t } = useTranslation()
  return (
    <figure className="border-muted-foreground/30 bg-muted/40 rounded-md border-l-2 px-3 py-2">
      <figcaption className="text-muted-foreground text-[11px] tracking-wide uppercase">
        {t("pages.agent.proactive.manage.suggested_by_assistant")} · {label}
      </figcaption>
      <blockquote className="mt-1 text-xs break-words whitespace-pre-wrap">
        {text}
      </blockquote>
    </figure>
  )
}
