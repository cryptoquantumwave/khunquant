import { IconDownload, IconTrash } from "@tabler/icons-react"
import { useMemo } from "react"
import { useTranslation } from "react-i18next"

import { LogsPanel } from "@/components/logs/logs-panel"
import { ToolLogsPanel } from "@/components/logs/tool-logs-panel"
import { PageHeader } from "@/components/page-header"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useGatewayLogs } from "@/hooks/use-gateway-logs"
import { useLogWrapColumns } from "@/hooks/use-log-wrap-columns"
import { parseToolLogs } from "@/lib/tool-log-parser"

export function LogsPage() {
  const { t } = useTranslation()
  const { clearLogs, clearing, logs } = useGatewayLogs()
  const { contentRef, measureRef, wrapColumns } = useLogWrapColumns()

  const toolEntries = useMemo(() => parseToolLogs(logs), [logs])

  function exportAsFile() {
    const content = logs.join("\n")
    const blob = new Blob([content], { type: "text/plain" })
    const url = URL.createObjectURL(blob)
    const a = document.createElement("a")
    a.href = url
    a.download = `khunquant-${new Date().toISOString().replace(/[:.]/g, "-")}.log`
    a.click()
    URL.revokeObjectURL(url)
  }

  function exportToClipboard() {
    const content = logs.join("\n")
    navigator.clipboard.writeText(content).then(() => {
      // Brief visual confirmation via browser default — no toast needed
    })
  }

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title={t("navigation.logs")}
        children={
          <div className="flex items-center gap-2">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="sm" disabled={logs.length === 0}>
                  <IconDownload className="size-4" />
                  {t("pages.logs.export")}
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={exportAsFile}>
                  {t("pages.logs.export_file")}
                </DropdownMenuItem>
                <DropdownMenuItem onClick={exportToClipboard}>
                  {t("pages.logs.export_clipboard")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <Button
              variant="outline"
              size="sm"
              onClick={clearLogs}
              disabled={logs.length === 0 || clearing}
            >
              <IconTrash className="size-4" />
              {t("pages.logs.clear")}
            </Button>
          </div>
        }
      />

      <Tabs defaultValue="all" className="flex flex-1 flex-col overflow-hidden">
        <TabsList>
          <TabsTrigger value="all">{t("pages.logs.tabs.all")}</TabsTrigger>
          <TabsTrigger value="tools">
            {t("pages.logs.tabs.tools")}
            {toolEntries.length > 0 && (
              <span className="ml-1 text-xs opacity-60">({toolEntries.length})</span>
            )}
          </TabsTrigger>
        </TabsList>
        <TabsContent value="all" className="flex flex-1 flex-col overflow-hidden p-4 sm:p-8">
          <LogsPanel
            logs={logs}
            wrapColumns={wrapColumns}
            contentRef={contentRef}
            measureRef={measureRef}
          />
        </TabsContent>
        <TabsContent value="tools" className="flex flex-1 flex-col overflow-hidden p-4 sm:p-8">
          <ToolLogsPanel entries={toolEntries} />
        </TabsContent>
      </Tabs>
    </div>
  )
}
