import {
  Navigate,
  Outlet,
  createFileRoute,
  useRouterState,
} from "@tanstack/react-router"

export const Route = createFileRoute("/portfolios")({
  component: PortfoliosLayout,
})

function PortfoliosLayout() {
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  })

  if (pathname === "/portfolios") {
    return <Navigate to="/portfolios/$name" params={{ name: "binance" }} />
  }

  return <Outlet />
}
