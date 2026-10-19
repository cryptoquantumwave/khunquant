import { IconLoader2 } from "@tabler/icons-react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import { getProactiveStatus } from "@/api/proactive"
import { PageHeader } from "@/components/page-header"
import { DecisionsSection } from "@/components/proactive/decisions-section"
import { ManageSection } from "@/components/proactive/manage-section"
import { ModesSection } from "@/components/proactive/modes-section"
import { OwnerSection } from "@/components/proactive/owner-section"
import { ProfileSection } from "@/components/proactive/profile-section"
import { StopSection } from "@/components/proactive/stop-section"

export function ProactivePage() {
  const { t } = useTranslation()
  const {
    data: status,
    isLoading,
    error,
  } = useQuery({
    queryKey: ["proactive", "status"],
    queryFn: getProactiveStatus,
  })

  return (
    <div className="flex h-full flex-col">
      <PageHeader title={t("navigation.proactive")} />

      <div className="flex-1 overflow-auto px-6 py-3">
        <div className="w-full max-w-3xl space-y-4 pb-8">
          {isLoading ? (
            <div className="text-muted-foreground flex items-center gap-2 py-6 text-sm">
              <IconLoader2 className="size-4 animate-spin" />
              {t("pages.agent.proactive.loading")}
            </div>
          ) : error || !status ? (
            <div className="text-destructive py-6 text-sm">
              {t("pages.agent.proactive.load_error")}
            </div>
          ) : (
            <>
              <OwnerSection status={status} />
              <ModesSection status={status} />
            </>
          )}
          <StopSection />
          <ManageSection />
          <ProfileSection />
          <DecisionsSection />
        </div>
      </div>
    </div>
  )
}
