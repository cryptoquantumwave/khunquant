import { IconLoader2 } from "@tabler/icons-react"
import type { UseQueryResult } from "@tanstack/react-query"
import { type ReactNode, useState } from "react"
import { useTranslation } from "react-i18next"

import type { ManageEnvelope } from "@/api/proactive-manage"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"

// ManageQuery shows one clear state for a gateway that is down or a service that is
// off, instead of a stack of errors, and renders its children otherwise.
export function ManageQuery<T extends ManageEnvelope>({
  query,
  children,
}: {
  query: UseQueryResult<T>
  children: (data: T) => ReactNode
}) {
  const { t } = useTranslation()
  if (query.isLoading) {
    return (
      <div className="text-muted-foreground flex items-center gap-2 py-6 text-sm">
        <IconLoader2 className="size-4 animate-spin" />
        {t("pages.agent.proactive.manage.loading")}
      </div>
    )
  }
  if (query.error || !query.data) {
    return (
      <p className="text-muted-foreground py-6 text-center text-sm">
        {t("pages.agent.proactive.manage.unreachable")}
      </p>
    )
  }
  if (!query.data.available) {
    return (
      <div className="text-muted-foreground py-6 text-center text-sm">
        <p>{t("pages.agent.proactive.manage.unavailable")}</p>
        {query.data.reason && (
          <p className="mt-1 text-xs opacity-70">{query.data.reason}</p>
        )}
      </div>
    )
  }
  return <>{children(query.data)}</>
}

// WebOwnerNotice explains once why the buttons that raise risk are disabled.
export function WebOwnerNotice({ missing }: { missing: string }) {
  const { t } = useTranslation()
  return (
    <p className="rounded-md border border-amber-300/50 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-500/30 dark:bg-amber-900/20 dark:text-amber-300">
      {t("pages.agent.proactive.manage.web_owner_blocked", { reason: missing })}
    </p>
  )
}

// ConfirmButton is a button that asks first. Everything on these tabs uses it: no
// click changes anything without a confirmation.
export function ConfirmButton({
  label,
  title,
  description,
  detail,
  variant = "outline",
  disabled = false,
  disabledReason,
  pending = false,
  onConfirm,
}: {
  label: string
  title: string
  description: string
  // Shown again in the dialog (an approval shows its summary).
  detail?: ReactNode
  variant?: "default" | "outline" | "destructive"
  disabled?: boolean
  disabledReason?: string
  pending?: boolean
  onConfirm: () => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button
        size="sm"
        variant={variant}
        disabled={disabled || pending}
        title={disabled ? disabledReason : undefined}
        onClick={() => setOpen(true)}
      >
        {pending && <IconLoader2 className="size-3.5 animate-spin" />}
        {label}
      </Button>
      <AlertDialog open={open} onOpenChange={setOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{title}</AlertDialogTitle>
            <AlertDialogDescription>{description}</AlertDialogDescription>
          </AlertDialogHeader>
          {detail}
          <AlertDialogFooter>
            <AlertDialogCancel>
              {t("pages.agent.proactive.manage.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                setOpen(false)
                onConfirm()
              }}
            >
              {label}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
