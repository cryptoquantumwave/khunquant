import { IconLoader2 } from "@tabler/icons-react"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"

import { getProactiveStatus } from "@/api/proactive"
import { PageHeader } from "@/components/page-header"
import { AutonomySection } from "@/components/proactive/autonomy-section"
import { ManageSection } from "@/components/proactive/manage-section"
import { MetricsSection } from "@/components/proactive/metrics-section"
import { ModesSection } from "@/components/proactive/modes-section"
import { MutesSection } from "@/components/proactive/mutes-section"
import { OwnerSection } from "@/components/proactive/owner-section"
import { ProfileSection } from "@/components/proactive/profile-section"
import { StopSection } from "@/components/proactive/stop-section"
import { TimelineSection } from "@/components/proactive/timeline-section"
import { Button } from "@/components/ui/button"

type View = "overview" | "controls" | "settings"
const VIEWS: View[] = ["overview", "controls", "settings"]

export function ProactivePage() {
  const { t } = useTranslation()
  const [view, setView] = useState<View>("overview")

  return (
    <div className="flex h-full flex-col">
      <PageHeader title={t("navigation.proactive")} />

      <div className="flex-1 overflow-auto px-6 py-3">
        <div className="w-full max-w-5xl space-y-4 pb-8">
          {/* Red, and in view on every view of the page. */}
          <StopSection />
          <div role="tablist" className="flex gap-1">
            {VIEWS.map((id) => (
              <Button
                key={id}
                role="tab"
                aria-selected={view === id}
                size="sm"
                variant={view === id ? "secondary" : "ghost"}
                onClick={() => setView(id)}
              >
                {t(`pages.agent.proactive.views.${id}`)}
              </Button>
            ))}
          </div>
          <div role="tabpanel" className="space-y-4">
            {view === "overview" && (
              <>
                <MetricsSection />
                <TimelineSection />
              </>
            )}
            {view === "controls" && (
              <>
                <AutonomySection />
                <MutesSection />
                <ManageSection />
              </>
            )}
            {view === "settings" && <SettingsView />}
          </div>
        </div>
      </div>
    </div>
  )
}

function SettingsView() {
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
    <>
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
      <ProfileSection />
    </>
  )
}
