import { IconLoader2 } from "@tabler/icons-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { toast } from "sonner"

import { getChannelsCatalog } from "@/api/channels"
import {
  type ProactiveStatus,
  clearProactiveOwner,
  putProactiveOwner,
} from "@/api/proactive"
import { ownerHints } from "@/components/proactive/owner-hints"
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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

export function OwnerSection({ status }: { status: ProactiveStatus }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()

  const [channel, setChannel] = useState("telegram")
  const [chatId, setChatId] = useState("")
  const [senderId, setSenderId] = useState("")
  const [confirmClear, setConfirmClear] = useState(false)

  const { data: catalog } = useQuery({
    queryKey: ["channels-catalog"],
    queryFn: getChannelsCatalog,
  })

  const onSaved = (next: ProactiveStatus) => {
    queryClient.setQueryData(["proactive", "status"], next)
  }

  const save = useMutation({
    mutationFn: putProactiveOwner,
    onSuccess: (next) => {
      onSaved(next)
      // The server never returns the full IDs; clear the form so they do not
      // linger on screen.
      setChatId("")
      setSenderId("")
      toast.success(t("pages.agent.proactive.owner.save_success"))
    },
  })

  const clear = useMutation({
    mutationFn: clearProactiveOwner,
    onSuccess: (next) => {
      onSaved(next)
      setConfirmClear(false)
      toast.success(t("pages.agent.proactive.owner.clear_success"))
    },
    onError: (err: Error) => {
      setConfirmClear(false)
      toast.error(t("pages.agent.proactive.owner.clear_error"), {
        description: err.message,
      })
    },
  })

  const hints = ownerHints(channel, chatId, senderId)
  const hasOwnerValues =
    status.owner.channel !== "" ||
    status.owner.chat_id_masked !== "" ||
    status.owner.sender_id_masked !== ""
  const canSave =
    channel.trim() !== "" &&
    chatId.trim() !== "" &&
    senderId.trim() !== "" &&
    !save.isPending

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">
          {t("pages.agent.proactive.owner.title")}
        </CardTitle>
        <CardDescription>
          {t("pages.agent.proactive.owner.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="rounded-lg border p-3 text-sm">
          <div className="text-muted-foreground mb-1 text-xs">
            {t("pages.agent.proactive.owner.current")}
          </div>
          {hasOwnerValues ? (
            <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 font-mono text-xs">
              <dt className="text-muted-foreground">
                {t("pages.agent.proactive.owner.channel")}
              </dt>
              <dd>{status.owner.channel || "-"}</dd>
              <dt className="text-muted-foreground">
                {t("pages.agent.proactive.owner.chat_id")}
              </dt>
              <dd>{status.owner.chat_id_masked || "-"}</dd>
              <dt className="text-muted-foreground">
                {t("pages.agent.proactive.owner.sender_id")}
              </dt>
              <dd>{status.owner.sender_id_masked || "-"}</dd>
            </dl>
          ) : (
            <p className="text-muted-foreground">
              {t("pages.agent.proactive.owner.none")}
            </p>
          )}
          <p
            className={
              status.owner_configured
                ? "mt-2 text-xs text-green-600 dark:text-green-400"
                : "text-muted-foreground mt-2 text-xs"
            }
          >
            {status.owner_configured
              ? t("pages.agent.proactive.owner.configured")
              : t("pages.agent.proactive.owner.not_configured")}
          </p>
          {status.validation_error !== "" && (
            <p className="text-destructive mt-2 text-xs whitespace-pre-wrap">
              {status.validation_error}
            </p>
          )}
        </div>

        <p className="text-muted-foreground text-sm">
          {t("pages.agent.proactive.owner.howto")}
        </p>

        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            if (!canSave) return
            save.mutate({
              channel: channel.trim(),
              chat_id: chatId.trim(),
              sender_id: senderId.trim(),
            })
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="proactive-owner-channel">
              {t("pages.agent.proactive.owner.channel")}
            </Label>
            <Input
              id="proactive-owner-channel"
              list="proactive-owner-channels"
              value={channel}
              onChange={(e) => setChannel(e.target.value)}
              placeholder="telegram"
              autoComplete="off"
            />
            <datalist id="proactive-owner-channels">
              {catalog?.channels.map((ch) => (
                <option key={ch.name} value={ch.name} />
              ))}
            </datalist>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="proactive-owner-chat">
              {t("pages.agent.proactive.owner.chat_id")}
            </Label>
            <Input
              id="proactive-owner-chat"
              value={chatId}
              onChange={(e) => setChatId(e.target.value)}
              placeholder={status.owner.chat_id_masked || "123456789"}
              autoComplete="off"
              inputMode="numeric"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="proactive-owner-sender">
              {t("pages.agent.proactive.owner.sender_id")}
            </Label>
            <Input
              id="proactive-owner-sender"
              value={senderId}
              onChange={(e) => setSenderId(e.target.value)}
              placeholder={status.owner.sender_id_masked || "telegram:123456789"}
              autoComplete="off"
            />
          </div>

          {hints.length > 0 && (
            <ul className="space-y-1 text-xs text-amber-600 dark:text-amber-400">
              {hints.map((h) => (
                <li key={h}>{t(`pages.agent.proactive.owner.hint_${h}`)}</li>
              ))}
            </ul>
          )}
          {save.error && (
            <p role="alert" className="text-destructive text-sm whitespace-pre-wrap">
              {save.error.message}
            </p>
          )}

          <div className="flex flex-wrap gap-2">
            <Button type="submit" size="sm" disabled={!canSave} className="gap-1.5">
              {save.isPending && <IconLoader2 className="size-3.5 animate-spin" />}
              {t("pages.agent.proactive.owner.save")}
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={!hasOwnerValues || clear.isPending}
              onClick={() => setConfirmClear(true)}
            >
              {t("pages.agent.proactive.owner.clear")}
            </Button>
          </div>
        </form>
      </CardContent>

      <AlertDialog open={confirmClear} onOpenChange={setConfirmClear}>
        <AlertDialogContent size="sm">
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("pages.agent.proactive.owner.clear_title")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("pages.agent.proactive.owner.clear_description")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={() => clear.mutate()}
            >
              {t("pages.agent.proactive.owner.clear_confirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  )
}
