import { IconLoader2, IconRefresh } from "@tabler/icons-react"
import { useIsFetching, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"

import { IntentsTab } from "@/components/proactive/intents-tab"
import { MANAGE_QUERY_KEY } from "@/components/proactive/manage-utils"
import { MandatesTab } from "@/components/proactive/mandates-tab"
import { ProposalsTab } from "@/components/proactive/proposals-tab"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

type Tab = "intents" | "proposals" | "mandates"
const TABS: Tab[] = ["intents", "proposals", "mandates"]

// ManageSection is the page's window on what the assistant keeps track of (intents),
// what it asks permission for (proposals) and what it may do by itself (mandates).
// Only the open tab is mounted, so only it refreshes.
export function ManageSection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const fetching = useIsFetching({ queryKey: MANAGE_QUERY_KEY }) > 0
  const [tab, setTab] = useState<Tab>("proposals")

  return (
    <Card className="border-border/60">
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle className="text-base">
              {t("pages.agent.proactive.manage.title")}
            </CardTitle>
            <CardDescription>
              {t("pages.agent.proactive.manage.description")}
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            className="gap-1.5"
            disabled={fetching}
            onClick={() =>
              void queryClient.invalidateQueries({ queryKey: MANAGE_QUERY_KEY })
            }
          >
            {fetching ? (
              <IconLoader2 className="size-3.5 animate-spin" />
            ) : (
              <IconRefresh className="size-3.5" />
            )}
            {t("pages.agent.proactive.manage.refresh")}
          </Button>
        </div>
        <div role="tablist" className="mt-2 flex gap-1">
          {TABS.map((id) => (
            <Button
              key={id}
              role="tab"
              aria-selected={tab === id}
              size="sm"
              variant={tab === id ? "secondary" : "ghost"}
              onClick={() => setTab(id)}
            >
              {t(`pages.agent.proactive.manage.tabs.${id}`)}
            </Button>
          ))}
        </div>
      </CardHeader>
      <CardContent role="tabpanel">
        {tab === "intents" && <IntentsTab />}
        {tab === "proposals" && <ProposalsTab />}
        {tab === "mandates" && <MandatesTab />}
      </CardContent>
    </Card>
  )
}
