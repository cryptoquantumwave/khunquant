import { IconLoader2, IconRefresh } from "@tabler/icons-react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import { getProactiveDecisions } from "@/api/proactive"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

const SEVERITY_CLASS: Record<string, string> = {
  info: "bg-muted text-muted-foreground",
  notice: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400",
  warning: "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400",
  critical: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400",
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

export function DecisionsSection() {
  const { t } = useTranslation()
  const { data, isLoading, isFetching, error, refetch } = useQuery({
    queryKey: ["proactive", "decisions"],
    queryFn: () => getProactiveDecisions(50),
    refetchInterval: 30_000,
  })

  return (
    <Card className="border-border/60">
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle className="text-base">
              {t("pages.agent.proactive.decisions.title")}
            </CardTitle>
            <CardDescription>
              {t("pages.agent.proactive.decisions.description")}
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            className="gap-1.5"
            disabled={isFetching}
            onClick={() => void refetch()}
          >
            {isFetching ? (
              <IconLoader2 className="size-3.5 animate-spin" />
            ) : (
              <IconRefresh className="size-3.5" />
            )}
            {t("pages.agent.proactive.decisions.refresh")}
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="text-muted-foreground flex items-center gap-2 py-4 text-sm">
            <IconLoader2 className="size-4 animate-spin" />
            {t("pages.agent.proactive.loading")}
          </div>
        ) : error ? (
          <p className="text-destructive text-sm">
            {t("pages.agent.proactive.decisions.load_error")}
          </p>
        ) : !data?.length ? (
          <div className="text-muted-foreground py-8 text-center text-sm">
            <p>{t("pages.agent.proactive.decisions.empty")}</p>
            <p className="mt-1 text-xs opacity-70">
              {t("pages.agent.proactive.decisions.empty_hint")}
            </p>
          </div>
        ) : (
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full text-sm">
              <thead>
                <tr className="bg-muted/40 text-muted-foreground border-b text-left text-xs tracking-wide uppercase">
                  <th className="px-3 py-2">{t("pages.agent.proactive.decisions.time")}</th>
                  <th className="px-3 py-2">{t("pages.agent.proactive.decisions.severity")}</th>
                  <th className="px-3 py-2">{t("pages.agent.proactive.decisions.disposition")}</th>
                  <th className="px-3 py-2">{t("pages.agent.proactive.decisions.reason")}</th>
                  <th className="px-3 py-2">{t("pages.agent.proactive.decisions.item")}</th>
                  <th className="px-3 py-2 text-center">
                    {t("pages.agent.proactive.decisions.delivered")}
                  </th>
                </tr>
              </thead>
              <tbody>
                {data.map((d) => (
                  <tr key={d.id} className="border-border/30 border-b last:border-0">
                    <td className="text-muted-foreground px-3 py-2 font-mono text-xs whitespace-nowrap">
                      {formatTime(d.at)}
                    </td>
                    <td className="px-3 py-2">
                      <span
                        className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                          SEVERITY_CLASS[d.severity_name] ?? SEVERITY_CLASS.info
                        }`}
                      >
                        {d.severity_name}
                      </span>
                    </td>
                    <td className="px-3 py-2 whitespace-nowrap">
                      {d.disposition}
                      {d.shadow && (
                        <span className="text-muted-foreground ml-1 text-xs">
                          ({t("pages.agent.proactive.decisions.shadow")})
                        </span>
                      )}
                    </td>
                    <td className="text-muted-foreground max-w-60 px-3 py-2 text-xs">
                      {d.reason || "-"}
                    </td>
                    <td className="max-w-72 px-3 py-2 text-xs">
                      {d.title || (
                        <span className="text-muted-foreground font-mono">
                          {d.trigger_id || d.dedup_key || "-"}
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-2 text-center">
                      {d.delivered ? (
                        <span aria-label={t("pages.agent.proactive.decisions.yes")}>✓</span>
                      ) : (
                        <span
                          className="text-muted-foreground"
                          aria-label={t("pages.agent.proactive.decisions.no")}
                        >
                          -
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  )
}
