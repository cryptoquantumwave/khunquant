import { createFileRoute } from "@tanstack/react-router"

import { PortfolioConfigPage } from "@/components/portfolios/portfolio-config-page"

export const Route = createFileRoute("/portfolios/$name")({
  component: PortfoliosByNameRoute,
})

function PortfoliosByNameRoute() {
  const { name } = Route.useParams()

  return <PortfolioConfigPage exchangeName={name} />
}
