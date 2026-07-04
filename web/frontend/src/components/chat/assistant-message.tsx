import {
  IconBrain,
  IconCheck,
  IconChevronDown,
  IconCopy,
  IconTool,
} from "@tabler/icons-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import ReactMarkdown from "react-markdown"
import rehypeHighlight from "rehype-highlight"
import rehypeRaw from "rehype-raw"
import rehypeSanitize from "rehype-sanitize"
import remarkGfm from "remark-gfm"

import {
  MessageCodeBlock,
  MarkdownCodeBlock,
} from "@/components/chat/message-code-block"
import { Button } from "@/components/ui/button"
import { formatMessageTime } from "@/hooks/use-pico-chat"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"
import { cn } from "@/lib/utils"
import {
  type AssistantMessageKind,
  type ChatToolCall,
} from "@/store/chat"

interface AssistantMessageProps {
  content: string
  kind?: AssistantMessageKind
  modelName?: string
  toolCalls?: ChatToolCall[]
  timestamp?: string | number
}

export function AssistantMessage({
  content,
  kind = "normal",
  modelName,
  toolCalls = [],
  timestamp = "",
}: AssistantMessageProps) {
  const { t } = useTranslation()
  const { copy, isCopied } = useCopyToClipboard()
  const isThought = kind === "thought"
  const isToolCalls = kind === "tool_calls"
  const isCollapsedBlock = isThought || isToolCalls
  const hasText = content.trim().length > 0
  const hasToolCalls = toolCalls.length > 0
  const [isExpanded, setIsExpanded] = useState(true)
  const formattedTimestamp =
    timestamp !== "" ? formatMessageTime(timestamp) : ""
  const collapsedLabel = isThought
    ? t("chat.reasoningLabel")
    : t("chat.toolCallsLabel")
  const copyMessageLabel = isCopied
    ? t("chat.copiedLabel")
    : t("chat.copyMessage")
  const trimmedModelName = modelName?.trim() ?? ""

  return (
    <div className="group flex w-full flex-col gap-1.5">
      {!isCollapsedBlock && (
        <div className="text-muted-foreground/60 flex items-center justify-between gap-2 px-1 text-xs opacity-70">
          <div className="flex items-center gap-2">
            <span>KhunQuant</span>
            {trimmedModelName && (
              <>
                <span className="opacity-50">•</span>
                <span>{trimmedModelName}</span>
              </>
            )}
            {formattedTimestamp && (
              <>
                <span className="opacity-50">•</span>
                <span>{formattedTimestamp}</span>
              </>
            )}
          </div>
        </div>
      )}

      {(hasText || isCollapsedBlock || hasToolCalls) && (
        <div
          className={cn(
            "relative overflow-hidden rounded-xl border",
            isCollapsedBlock
              ? "border-border/30 bg-muted/20 text-muted-foreground dark:border-border/20 dark:bg-muted/10"
              : "bg-card text-card-foreground border-border/60",
          )}
        >
          {isCollapsedBlock && (
            <div
              className="text-muted-foreground/60 hover:text-muted-foreground/80 flex cursor-pointer items-center justify-between px-3 py-2 text-[12px] font-medium transition-colors select-none"
              onClick={() => setIsExpanded(!isExpanded)}
            >
              <div className="flex items-center gap-1.5">
                {isThought ? (
                  <IconBrain className="size-3.5" />
                ) : (
                  <IconTool className="size-3.5" />
                )}
                <span>{collapsedLabel}</span>
                {trimmedModelName && (
                  <span className="text-muted-foreground/45">{trimmedModelName}</span>
                )}
              </div>
              <div className="flex items-center gap-2">
                {formattedTimestamp && (
                  <span className="opacity-50">{formattedTimestamp}</span>
                )}
                <IconChevronDown
                  className={cn(
                    "size-3.5 opacity-0 transition-all duration-200 group-hover:opacity-100",
                    isExpanded ? "rotate-180" : "",
                  )}
                />
              </div>
            </div>
          )}
          {(!isCollapsedBlock || isExpanded) && isToolCalls && hasToolCalls && (
            <div className="space-y-3 px-3 pt-0 pb-3">
              {toolCalls.map((toolCall, index) => {
                const explanation =
                  toolCall.extraContent?.toolFeedbackExplanation?.trim() ?? ""
                const toolName = toolCall.function?.name?.trim() ?? ""
                const toolArguments = toolCall.function?.arguments?.trim() ?? ""
                const hasFunctionSummary = toolName || toolArguments

                if (!explanation && !hasFunctionSummary) {
                  return null
                }

                return (
                  <div
                    key={toolCall.id ?? `${toolName}-${index}`}
                    className={cn(
                      "space-y-3",
                      index > 0 && "border-border/20 border-t pt-3",
                    )}
                  >
                    {explanation && (
                      <div className="space-y-1.5">
                        <div className="text-muted-foreground/55 text-[11px] font-medium tracking-wide uppercase">
                          {t("chat.toolCallExplanationLabel")}
                        </div>
                        <div className="prose dark:prose-invert prose-p:my-1.5 prose-p:whitespace-pre-wrap max-w-none text-[13px] leading-relaxed [overflow-wrap:anywhere] break-words opacity-75">
                          <ReactMarkdown
                            remarkPlugins={[remarkGfm]}
                            rehypePlugins={[
                              rehypeRaw,
                              rehypeSanitize,
                              rehypeHighlight,
                            ]}
                            components={{
                              pre: MarkdownCodeBlock,
                            }}
                          >
                            {explanation}
                          </ReactMarkdown>
                        </div>
                      </div>
                    )}

                    {hasFunctionSummary && (
                      <div
                        className={cn(
                          "space-y-1.5",
                          explanation && "border-border/20 border-t pt-3",
                        )}
                      >
                        <div className="text-muted-foreground/55 text-[11px] font-medium tracking-wide uppercase">
                          {t("chat.toolCallFunctionLabel")}
                        </div>
                        <div className="bg-background/55 border-border/25 space-y-2 rounded-lg border px-3 py-2.5">
                          {toolName && !toolArguments && (
                            <div className="text-foreground/75 font-mono text-[12px] font-semibold">
                              {toolName}
                            </div>
                          )}
                          {toolArguments && (
                            <MessageCodeBlock
                              code={toolArguments}
                              language="json"
                              label={toolName || t("chat.toolCallArgumentsLabel")}
                              className="my-0 shadow-none"
                              bodyClassName="px-3 py-2 text-[12px] leading-relaxed"
                            />
                          )}
                        </div>
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          )}
          {(!isCollapsedBlock || isExpanded) && !isToolCalls && hasText && (
            <div
              className={cn(
                "prose dark:prose-invert prose-pre:my-2 prose-pre:overflow-x-auto prose-pre:rounded-lg prose-pre:border prose-pre:bg-zinc-100 prose-pre:p-0 prose-pre:text-zinc-900 dark:prose-pre:bg-zinc-950 dark:prose-pre:text-zinc-100 max-w-none [overflow-wrap:anywhere] break-words",
                isThought
                  ? "prose-p:my-1.5 prose-p:whitespace-pre-wrap px-3 pt-0 pb-3 text-[13px] leading-relaxed opacity-70"
                  : "prose-p:my-2 prose-p:whitespace-pre-wrap p-4 text-[15px] leading-relaxed",
              )}
            >
              <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                rehypePlugins={[rehypeRaw, rehypeSanitize, rehypeHighlight]}
                components={{
                  pre: MarkdownCodeBlock,
                }}
              >
                {content}
              </ReactMarkdown>
            </div>
          )}

          {!isCollapsedBlock && hasText && (
            <Button
              variant="ghost"
              size="icon"
              className={cn(
                "bg-background/50 hover:bg-background/80 absolute top-2 right-2 h-7 w-7 opacity-0 transition-opacity group-hover:opacity-100",
              )}
              onClick={() => void copy(content)}
              aria-label={copyMessageLabel}
              title={copyMessageLabel}
            >
              {isCopied ? (
                <IconCheck className="h-4 w-4 text-green-500" />
              ) : (
                <IconCopy className="text-muted-foreground h-4 w-4" />
              )}
            </Button>
          )}
        </div>
      )}
    </div>
  )
}
