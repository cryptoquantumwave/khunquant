import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import { getMutes, unmute } from "@/api/proactive-dashboard"
import {
  ConfirmButton,
  ManageQuery,
} from "@/components/proactive/manage-shared"
import {
  MANAGE_REFRESH_MS,
  formatTime,
  useManageAction,
} from "@/components/proactive/manage-utils"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

const MUTES_KEY = ["proactive", "manage", "mutes"] as const

export function MutesSection() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: MUTES_KEY,
    queryFn: getMutes,
    refetchInterval: MANAGE_REFRESH_MS,
  })
  const remove = useManageAction((id: string) => unmute(id))

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">
          {t("pages.agent.proactive.mutes.title")}
        </CardTitle>
        <CardDescription>
          {t("pages.agent.proactive.mutes.description")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <ManageQuery query={query}>
          {(data) =>
            (data.mutes ?? []).length === 0 ? (
              <p className="text-muted-foreground py-4 text-center text-sm">
                {t("pages.agent.proactive.mutes.empty")}
              </p>
            ) : (
              <ul className="space-y-2">
                {(data.mutes ?? []).map((m) => (
                  <li
                    key={m.id}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border p-3"
                  >
                    <div className="min-w-0 space-y-0.5">
                      {/* The key and the reason are plain text, never interpreted. */}
                      <p className="font-mono text-sm break-all">{m.key}</p>
                      <p className="text-muted-foreground text-xs">
                        {t(`pages.agent.proactive.mutes.kind.${m.kind}`)} ·{" "}
                        {t("pages.agent.proactive.mutes.until", {
                          time: formatTime(m.until),
                        })}
                        {m.reason ? ` · ${m.reason}` : ""}
                      </p>
                    </div>
                    <ConfirmButton
                      label={t("pages.agent.proactive.mutes.unmute")}
                      title={t("pages.agent.proactive.mutes.confirm_title")}
                      description={t(
                        "pages.agent.proactive.mutes.confirm_description",
                      )}
                      pending={remove.isPending && remove.variables === m.id}
                      onConfirm={() => remove.mutate(m.id)}
                    />
                  </li>
                ))}
              </ul>
            )
          }
        </ManageQuery>
      </CardContent>
    </Card>
  )
}
