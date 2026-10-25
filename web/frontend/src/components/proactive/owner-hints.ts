// Client-side hints for the owner form. They only catch the common mistakes
// early; the server runs the real validation and its message is always shown.

export type OwnerHint =
  | "group_chat"
  | "telegram_chat_numeric"
  | "sender_not_canonical"
  | "platform_mismatch"
  | "telegram_sender_numeric"

const DIGITS = /^\d+$/

function splitCanonical(sender: string): { platform: string; id: string } | null {
  const idx = sender.indexOf(":")
  if (idx <= 0 || idx === sender.length - 1) return null
  return { platform: sender.slice(0, idx).toLowerCase(), id: sender.slice(idx + 1) }
}

export function ownerHints(
  channelInput: string,
  chatIdInput: string,
  senderIdInput: string,
): OwnerHint[] {
  const channel = channelInput.trim().toLowerCase()
  const chatId = chatIdInput.trim()
  const sender = senderIdInput.trim()
  const canonical = sender ? splitCanonical(sender) : null
  const telegram =
    channel === "telegram" || (channel === "" && canonical?.platform === "telegram")

  const hints: OwnerHint[] = []
  if (telegram && chatId.startsWith("-")) {
    // Telegram group, supergroup and channel ids are negative.
    hints.push("group_chat")
  } else if (telegram && chatId !== "" && !DIGITS.test(chatId)) {
    hints.push("telegram_chat_numeric")
  }
  if (sender !== "" && canonical === null) {
    hints.push("sender_not_canonical")
  }
  if (canonical !== null && channel !== "" && canonical.platform !== channel) {
    hints.push("platform_mismatch")
  }
  if (canonical?.platform === "telegram" && !DIGITS.test(canonical.id)) {
    hints.push("telegram_sender_numeric")
  }
  return hints
}
