import type { ChannelConfig } from "@/api/channels"

export const SECRET_FIELD_MAP: Record<string, string> = {
  token: "_token",
  app_secret: "_app_secret",
  client_secret: "_client_secret",
  corp_secret: "_corp_secret",
  channel_secret: "_channel_secret",
  channel_access_token: "_channel_access_token",
  access_token: "_access_token",
  bot_token: "_bot_token",
  app_token: "_app_token",
  encoding_aes_key: "_encoding_aes_key",
  encrypt_key: "_encrypt_key",
  verification_token: "_verification_token",
  password: "_password",
  nickserv_password: "_nickserv_password",
  sasl_password: "_sasl_password",
}

// Per-channel secret fields. Used to inject empty placeholders for secrets the
// server omits when unset (omitzero JSON tag on SecureString fields).
const CHANNEL_SECRET_FIELDS: Record<string, string[]> = {
  telegram: ["token"],
  discord: ["token"],
  slack: ["bot_token", "app_token"],
  feishu: ["app_secret", "encrypt_key", "verification_token"],
  dingtalk: ["client_secret"],
  line: ["channel_secret", "channel_access_token"],
  qq: ["app_secret"],
  onebot: ["access_token"],
  wecom: ["token"],
  wecom_app: ["corp_secret"],
  wecom_aibot: ["token"],
  pico: ["token"],
  matrix: ["access_token"],
  irc: ["password", "nickserv_password", "sasl_password"],
}

const SECRET_FIELD_SET = new Set(Object.keys(SECRET_FIELD_MAP))

export function isSecretField(key: string): boolean {
  return SECRET_FIELD_SET.has(key)
}

export function buildEditConfig(
  channelName: string,
  config: ChannelConfig,
): ChannelConfig {
  const edit: ChannelConfig = { ...config }

  for (const key of CHANNEL_SECRET_FIELDS[channelName] ?? []) {
    // Inject empty slot so the form renders the field even when server omits it.
    if (!(key in edit)) {
      edit[key] = ""
    }
    const editKey = SECRET_FIELD_MAP[key]
    if (editKey) {
      edit[editKey] = ""
    }
  }

  return edit
}
