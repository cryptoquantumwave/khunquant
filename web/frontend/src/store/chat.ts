import { atom, getDefaultStore } from "jotai"
import { atomWithStorage } from "jotai/utils"

import {
  getInitialActiveSessionId,
  writeStoredSessionId,
} from "@/features/chat/state"

export type AssistantMessageKind = "normal" | "thought" | "tool_calls"

export interface ChatToolCallFunction {
  name?: string
  arguments?: string
}

export interface ChatToolCallExtraContent {
  toolFeedbackExplanation?: string
}

export interface ChatToolCall {
  id?: string
  type?: string
  function?: ChatToolCallFunction
  extraContent?: ChatToolCallExtraContent
}

export interface ChatMessage {
  id: string
  role: "user" | "assistant"
  content: string
  timestamp: number | string
  imageDataUri?: string  // set when the message is a media.create delivery
  imageCaption?: string
  imageFilename?: string
  // reasoning/tool-call fields — populated only for live pico streaming
  kind?: AssistantMessageKind
  modelName?: string
  toolCalls?: ChatToolCall[]
}

export interface ContextUsage {
  used_tokens: number
  total_tokens: number
  compress_at_tokens: number
  used_percent: number
}

export type ConnectionState =
  | "disconnected"
  | "connecting"
  | "connected"
  | "error"

export interface ChatStoreState {
  messages: ChatMessage[]
  connectionState: ConnectionState
  isTyping: boolean
  activeSessionId: string
  hasHydratedActiveSession: boolean
  contextUsage?: ContextUsage
}

type ChatStorePatch = Partial<ChatStoreState>

const DEFAULT_CHAT_STATE: ChatStoreState = {
  messages: [],
  connectionState: "disconnected",
  isTyping: false,
  activeSessionId: getInitialActiveSessionId(),
  hasHydratedActiveSession: false,
}

export const chatAtom = atom<ChatStoreState>(DEFAULT_CHAT_STATE)

export const showThoughtsAtom = atomWithStorage<boolean>(
  "khunquant:chat-show-reasoning",
  true,
  undefined,
  { getOnInit: true },
)

const store = getDefaultStore()

export function getChatState() {
  return store.get(chatAtom)
}

export function updateChatStore(
  patch:
    | ChatStorePatch
    | ((prev: ChatStoreState) => ChatStorePatch | ChatStoreState),
) {
  store.set(chatAtom, (prev) => {
    const nextPatch = typeof patch === "function" ? patch(prev) : patch
    const next = { ...prev, ...nextPatch }

    if (next.activeSessionId !== prev.activeSessionId) {
      writeStoredSessionId(next.activeSessionId)
    }

    return next
  })
}
