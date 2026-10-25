import { createFileRoute } from "@tanstack/react-router"

import { ProactivePage } from "@/components/proactive/proactive-page"

export const Route = createFileRoute("/agent/proactive")({
  component: ProactiveRoute,
})

function ProactiveRoute() {
  return <ProactivePage />
}
