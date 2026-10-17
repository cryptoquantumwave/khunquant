import { IconLoader2 } from "@tabler/icons-react"
import { useMutation } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import { stopAgent } from "@/api/proactive"
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
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

export function StopSection() {
  const { t } = useTranslation()
  const [confirm, setConfirm] = useState(false)
  const [summary, setSummary] = useState("")

  const stop = useMutation({
    mutationFn: stopAgent,
    onSuccess: (res) => {
      setConfirm(false)
      setSummary(res.text)
      toast.success(t("pages.agent.proactive.stop.done"))
    },
    onError: (err: Error) => {
      setConfirm(false)
      toast.error(t("pages.agent.proactive.stop.error"), {
        description: err.message,
      })
    },
  })

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">
          {t("pages.agent.proactive.stop.title")}
        </CardTitle>
        <CardDescription>
          {t("pages.agent.proactive.stop.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <Button
          variant="destructive"
          disabled={stop.isPending}
          onClick={() => setConfirm(true)}
        >
          {stop.isPending && <IconLoader2 className="size-4 animate-spin" />}
          {t("pages.agent.proactive.stop.button")}
        </Button>
        {summary !== "" && (
          <pre className="rounded-lg border p-3 text-xs whitespace-pre-wrap">
            {summary}
          </pre>
        )}
      </CardContent>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("pages.agent.proactive.stop.confirm_title")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("pages.agent.proactive.stop.confirm_description")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>
              {t("pages.agent.proactive.stop.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction onClick={() => stop.mutate()}>
              {t("pages.agent.proactive.stop.confirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  )
}
