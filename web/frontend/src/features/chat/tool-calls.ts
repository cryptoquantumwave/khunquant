import type { ChatToolCall } from "@/store/chat"

export function parseToolCallsValue(raw: unknown): ChatToolCall[] | undefined {
  if (!Array.isArray(raw)) {
    return undefined
  }

  const toolCalls: ChatToolCall[] = []
  for (const item of raw) {
    if (!item || typeof item !== "object") {
      continue
    }

    const toolCall = item as Record<string, unknown>
    const rawFunction =
      toolCall.function && typeof toolCall.function === "object"
        ? (toolCall.function as Record<string, unknown>)
        : null
    const rawExtraContent =
      toolCall.extra_content && typeof toolCall.extra_content === "object"
        ? (toolCall.extra_content as Record<string, unknown>)
        : null

    const nextToolCall: ChatToolCall = {
      ...(typeof toolCall.id === "string" ? { id: toolCall.id } : {}),
      ...(typeof toolCall.type === "string" ? { type: toolCall.type } : {}),
    }

    if (rawFunction) {
      const name =
        typeof rawFunction.name === "string" ? rawFunction.name : undefined
      const argumentsText =
        typeof rawFunction.arguments === "string"
          ? rawFunction.arguments
          : undefined

      if (name || argumentsText) {
        nextToolCall.function = {
          ...(name ? { name } : {}),
          ...(argumentsText ? { arguments: argumentsText } : {}),
        }
      }
    }

    if (rawExtraContent) {
      const toolFeedbackExplanation =
        typeof rawExtraContent.tool_feedback_explanation === "string"
          ? rawExtraContent.tool_feedback_explanation
          : undefined

      if (toolFeedbackExplanation) {
        nextToolCall.extraContent = {
          toolFeedbackExplanation,
        }
      }
    }

    if (
      nextToolCall.id ||
      nextToolCall.type ||
      nextToolCall.function ||
      nextToolCall.extraContent
    ) {
      toolCalls.push(nextToolCall)
    }
  }

  return toolCalls.length > 0 ? toolCalls : undefined
}
