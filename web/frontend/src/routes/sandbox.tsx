import { createFileRoute } from "@tanstack/react-router"

import { SandboxPage } from "@/components/sandbox/sandbox-page"

export const Route = createFileRoute("/sandbox")({
  component: SandboxPage,
})
