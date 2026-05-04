package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/caarlos0/env/v11"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
)

// rrCounter is a global counter for round-robin load balancing across models.
var rrCounter atomic.Uint64

// FlexibleStringSlice is a []string that also accepts JSON numbers,
// so allow_from can contain both "123" and 123.
// It also supports parsing comma-separated strings from environment variables,
// including both English (,) and Chinese (，) commas.
type FlexibleStringSlice []string

func (f *FlexibleStringSlice) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = nil
		return nil
	}

	// Accept a single JSON string for convenience, e.g.:
	// "text": "Thinking..."
	var singleString string
	if err := json.Unmarshal(data, &singleString); err == nil {
		*f = FlexibleStringSlice{singleString}
		return nil
	}

	// Accept a single JSON number too, to keep symmetry with mixed allow_from
	// payloads that may contain numeric identifiers.
	var singleNumber float64
	if err := json.Unmarshal(data, &singleNumber); err == nil {
		*f = FlexibleStringSlice{fmt.Sprintf("%.0f", singleNumber)}
		return nil
	}

	// Try []string first
	var ss []string
	if err := json.Unmarshal(data, &ss); err == nil {
		*f = ss
		return nil
	}

	// Try []interface{} to handle mixed types
	var raw []any
	if err := json.Unmarshal(data, &raw); err != nil {
		var s string
		// fail over to compatible to old format string
		if err = json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = []string{s}
		return nil
	}

	result := make([]string, 0, len(raw))
	for _, v := range raw {
		switch val := v.(type) {
		case string:
			result = append(result, val)
		case float64:
			result = append(result, fmt.Sprintf("%.0f", val))
		default:
			result = append(result, fmt.Sprintf("%v", val))
		}
	}
	*f = result
	return nil
}

// UnmarshalText implements encoding.TextUnmarshaler to support env variable parsing.
// It handles comma-separated values with both English (,) and Chinese (，) commas.
func (f *FlexibleStringSlice) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*f = nil
		return nil
	}

	s := string(text)
	// Replace Chinese comma with English comma, then split
	s = strings.ReplaceAll(s, "，", ",")
	parts := strings.Split(s, ",")

	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	*f = result
	return nil
}

type Config struct {
	Agents      AgentsConfig      `json:"agents"                yaml:"-"`
	Bindings    []AgentBinding    `json:"bindings,omitempty"    yaml:"-"`
	Session     SessionConfig     `json:"session,omitempty"     yaml:"-"`
	Channels    ChannelsConfig    `json:"channels"              yaml:"channels,omitempty"`
	Providers   ProvidersConfig   `json:"providers,omitempty"   yaml:"-"`
	ModelList   SecureModelList   `json:"model_list"            yaml:"model_list"` // New model-centric provider configuration
	Gateway     GatewayConfig     `json:"gateway"               yaml:"-"`
	Tools       ToolsConfig       `json:"tools"                 yaml:"-"`
	Exchanges   ExchangesConfig   `json:"exchanges"             yaml:"exchanges,omitempty"`
	TradingRisk TradingRiskConfig `json:"trading_risk,omitempty" yaml:"-"`
	Heartbeat   HeartbeatConfig   `json:"heartbeat"             yaml:"-"`
	Devices     DevicesConfig     `json:"devices"               yaml:"-"`
	Voice       VoiceConfig       `json:"voice"                 yaml:"-"`
	BuildInfo   BuildInfo         `json:"build_info,omitempty"  yaml:"-"`
	Update      UpdateConfig      `json:"update,omitempty"      yaml:"-"`

	// sensitiveCache caches the strings.Replacer for filtering sensitive data from logs/LLM output.
	sensitiveCache *SensitiveDataCache
}

// UpdateConfig controls the automatic update-check behaviour.
type UpdateConfig struct {
	CheckOnStart bool `json:"check_on_start"`
}

// PermissionScope restricts what operations an exchange account may perform.
// Multiple scopes may be combined. An empty slice means full access.
type PermissionScope string

const (
	// ScopeMarketData allows read-only market data fetching only.
	ScopeMarketData PermissionScope = "market_data"
	// ScopeTrade allows order placement and cancellation.
	ScopeTrade PermissionScope = "trade"
	// ScopeTransfer allows internal fund transfers between wallets.
	ScopeTransfer PermissionScope = "transfer"
)

// ExchangeAccount holds credentials for a single named exchange sub-account.
type ExchangeAccount struct {
	Name        string            `json:"name,omitempty"        yaml:"-"`
	APIKey      SecureString      `json:"api_key,omitzero"      yaml:"api_key,omitempty"`
	Secret      SecureString      `json:"secret,omitzero"       yaml:"secret,omitempty"`
	Permissions []PermissionScope `json:"permissions,omitempty" yaml:"-"`
	Proxy       string            `json:"proxy,omitempty"       yaml:"-"` // e.g. "http://vps:3128" or "socks5://127.0.0.1:1080"
}

// HasPermission returns true if the account has the requested scope.
// An empty Permissions list grants all scopes (backward-compatible default).
func (a ExchangeAccount) HasPermission(scope PermissionScope) bool {
	if len(a.Permissions) == 0 {
		return true
	}
	for _, p := range a.Permissions {
		if p == scope {
			return true
		}
	}
	return false
}

// RedactedAPIKey returns the API key with all but the last 4 characters masked,
// safe for logging. Returns "***" for keys shorter than 4 characters.
func (a ExchangeAccount) RedactedAPIKey() string {
	key := a.APIKey.String()
	if len(key) <= 4 {
		return "***"
	}
	return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
}

// TradingRiskConfig holds per-exchange risk controls for order execution.
type TradingRiskConfig struct {
	// PaperTradingMode forces all create_order calls to simulate without real execution.
	PaperTradingMode bool `json:"paper_trading_mode" env:"KHUNQUANT_TRADING_PAPER_MODE"`

	// MaxOrderValueUSD rejects any single order whose notional exceeds this value (USD).
	// 0 means no limit. Recommended: 10000.
	MaxOrderValueUSD float64 `json:"max_order_value_usd" env:"KHUNQUANT_TRADING_MAX_ORDER_USD"`

	// DailyLossLimitUSD pauses order execution when the day's realised losses
	// exceed this value (USD). 0 means no limit.
	DailyLossLimitUSD float64 `json:"daily_loss_limit_usd" env:"KHUNQUANT_TRADING_DAILY_LOSS_USD"`

	// AllowMargin enables margin / cross-collateral order types.
	// Default false — must explicitly opt-in.
	AllowMargin bool `json:"allow_margin" env:"KHUNQUANT_TRADING_ALLOW_MARGIN"`

	// AllowLeverage enables leveraged / futures order types.
	// Default false — must explicitly opt-in.
	AllowLeverage bool `json:"allow_leverage" env:"KHUNQUANT_TRADING_ALLOW_LEVERAGE"`
}

// OKXExchangeAccount extends ExchangeAccount with the OKX-specific passphrase.
type OKXExchangeAccount struct {
	ExchangeAccount
	Passphrase SecureString `json:"passphrase,omitzero" yaml:"passphrase,omitempty"`
}

// SettradeExchangeAccount extends ExchangeAccount with SETTRADE-specific fields.
// APIKey = Settrade app login ID; Secret = base64-encoded PKCS#8 ECDSA P-256 private key.
type SettradeExchangeAccount struct {
	ExchangeAccount
	BrokerID  string       `json:"broker_id"         yaml:"-"` // e.g. "FSSVP"
	AppCode   string       `json:"app_code"          yaml:"-"` // e.g. "ALGO" — used in all OAM URL paths
	AccountNo string       `json:"account_no"        yaml:"-"` // trading account number
	PIN       SecureString `json:"pin,omitzero"      yaml:"pin,omitempty"` // trading PIN; stored in config for auto-verify
}

// SettradeExchangeConfig holds the SETTRADE exchange credentials and settings.
type SettradeExchangeConfig struct {
	Enabled  bool                      `json:"enabled" env:"KHUNQUANT_EXCHANGES_SETTRADE_ENABLED"`
	Accounts []SettradeExchangeAccount `json:"accounts,omitempty"`
}

// ResolveAccount returns the SETTRADE account matching name, or the first account when name is "".
func (c *SettradeExchangeConfig) ResolveAccount(name string) (SettradeExchangeAccount, bool) {
	for i, acc := range c.Accounts {
		effectiveName := acc.Name
		if effectiveName == "" {
			effectiveName = fmt.Sprintf("%d", i+1)
		}
		if name == "" || strings.EqualFold(effectiveName, name) {
			if acc.Name == "" {
				acc.Name = effectiveName
			}
			return acc, true
		}
	}
	return SettradeExchangeAccount{}, false
}

// ExchangesConfig holds configuration for all supported exchanges.
type ExchangesConfig struct {
	Binance   BinanceExchangeConfig   `json:"binance"`
	OKX       OKXExchangeConfig       `json:"okx"`
	Bitkub    BitkubExchangeConfig    `json:"bitkub"`
	BinanceTH BinanceTHExchangeConfig `json:"binanceth"`
	Settrade  SettradeExchangeConfig  `json:"settrade"`
}

// BinanceExchangeConfig holds the Binance exchange credentials and settings.
type BinanceExchangeConfig struct {
	Enabled  bool              `json:"enabled"  env:"KHUNQUANT_EXCHANGES_BINANCE_ENABLED"`
	Testnet  bool              `json:"testnet"  env:"KHUNQUANT_EXCHANGES_BINANCE_TESTNET"`
	Accounts []ExchangeAccount `json:"accounts,omitempty"`
}

// ResolveAccount returns the account matching name, or the first account when name is "".
// Accounts with no name are assigned positional names "1", "2", etc.
func (c *BinanceExchangeConfig) ResolveAccount(name string) (ExchangeAccount, bool) {
	return resolveAccount(c.Accounts, name)
}

// BinanceTHExchangeConfig holds the Binance Thailand exchange credentials and settings.
type BinanceTHExchangeConfig struct {
	Enabled  bool              `json:"enabled"  env:"KHUNQUANT_EXCHANGES_BINANCETH_ENABLED"`
	Accounts []ExchangeAccount `json:"accounts,omitempty"`
}

// ResolveAccount returns the account matching name, or the first account when name is "".
func (c *BinanceTHExchangeConfig) ResolveAccount(name string) (ExchangeAccount, bool) {
	return resolveAccount(c.Accounts, name)
}

// BitkubExchangeConfig holds the Bitkub exchange credentials and settings.
type BitkubExchangeConfig struct {
	Enabled  bool              `json:"enabled"  env:"KHUNQUANT_EXCHANGES_BITKUB_ENABLED"`
	Accounts []ExchangeAccount `json:"accounts,omitempty"`
}

// ResolveAccount returns the account matching name, or the first account when name is "".
func (c *BitkubExchangeConfig) ResolveAccount(name string) (ExchangeAccount, bool) {
	return resolveAccount(c.Accounts, name)
}

// OKXExchangeConfig holds the OKX exchange credentials and settings.
type OKXExchangeConfig struct {
	Enabled  bool                 `json:"enabled"  env:"KHUNQUANT_EXCHANGES_OKX_ENABLED"`
	Testnet  bool                 `json:"testnet"  env:"KHUNQUANT_EXCHANGES_OKX_TESTNET"`
	Accounts []OKXExchangeAccount `json:"accounts,omitempty"`
}

// ResolveAccount returns the OKX account matching name, or the first account when name is "".
func (c *OKXExchangeConfig) ResolveAccount(name string) (OKXExchangeAccount, bool) {
	for i, acc := range c.Accounts {
		effectiveName := acc.Name
		if effectiveName == "" {
			effectiveName = fmt.Sprintf("%d", i+1)
		}
		if name == "" || strings.EqualFold(effectiveName, name) {
			if acc.Name == "" {
				acc.Name = effectiveName
			}
			return acc, true
		}
	}
	return OKXExchangeAccount{}, false
}

// resolveAccount is a generic helper for []ExchangeAccount resolution.
func resolveAccount(accounts []ExchangeAccount, name string) (ExchangeAccount, bool) {
	for i, acc := range accounts {
		effectiveName := acc.Name
		if effectiveName == "" {
			effectiveName = fmt.Sprintf("%d", i+1)
		}
		if name == "" || strings.EqualFold(effectiveName, name) {
			if acc.Name == "" {
				acc.Name = effectiveName
			}
			return acc, true
		}
	}
	return ExchangeAccount{}, false
}

// BuildInfo contains build-time version information
type BuildInfo struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

// MarshalJSON implements custom JSON marshaling for Config
// to omit providers section when empty and session when empty
func (c Config) MarshalJSON() ([]byte, error) {
	type Alias Config
	aux := &struct {
		Providers *ProvidersConfig `json:"providers,omitempty"`
		Session   *SessionConfig   `json:"session,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(&c),
	}

	// Only include providers if not empty
	if !c.Providers.IsEmpty() {
		aux.Providers = &c.Providers
	}

	// Only include session if not empty
	if c.Session.DMScope != "" || len(c.Session.IdentityLinks) > 0 {
		aux.Session = &c.Session
	}

	return json.Marshal(aux)
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
	List     []AgentConfig `json:"list,omitempty"`
}

// AgentModelConfig supports both string and structured model config.
// String format: "gpt-4" (just primary, no fallbacks)
// Object format: {"primary": "gpt-4", "fallbacks": ["claude-haiku"]}
type AgentModelConfig struct {
	Primary   string   `json:"primary,omitempty"`
	Fallbacks []string `json:"fallbacks,omitempty"`
}

func (m *AgentModelConfig) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		m.Primary = s
		m.Fallbacks = nil
		return nil
	}
	type raw struct {
		Primary   string   `json:"primary"`
		Fallbacks []string `json:"fallbacks"`
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	m.Primary = r.Primary
	m.Fallbacks = r.Fallbacks
	return nil
}

func (m AgentModelConfig) MarshalJSON() ([]byte, error) {
	if len(m.Fallbacks) == 0 && m.Primary != "" {
		return json.Marshal(m.Primary)
	}
	type raw struct {
		Primary   string   `json:"primary,omitempty"`
		Fallbacks []string `json:"fallbacks,omitempty"`
	}
	return json.Marshal(raw{Primary: m.Primary, Fallbacks: m.Fallbacks})
}

type AgentConfig struct {
	ID        string            `json:"id"`
	Default   bool              `json:"default,omitempty"`
	Name      string            `json:"name,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	Model     *AgentModelConfig `json:"model,omitempty"`
	Skills    []string          `json:"skills,omitempty"`
	Subagents *SubagentsConfig  `json:"subagents,omitempty"`
}

type SubagentsConfig struct {
	AllowAgents []string          `json:"allow_agents,omitempty"`
	Model       *AgentModelConfig `json:"model,omitempty"`
}

type PeerMatch struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type BindingMatch struct {
	Channel   string     `json:"channel"`
	AccountID string     `json:"account_id,omitempty"`
	Peer      *PeerMatch `json:"peer,omitempty"`
	GuildID   string     `json:"guild_id,omitempty"`
	TeamID    string     `json:"team_id,omitempty"`
}

type AgentBinding struct {
	AgentID string       `json:"agent_id"`
	Match   BindingMatch `json:"match"`
}

type SessionConfig struct {
	DMScope       string              `json:"dm_scope,omitempty"`
	IdentityLinks map[string][]string `json:"identity_links,omitempty"`
}

// RoutingConfig controls the intelligent model routing feature.
// When enabled, each incoming message is scored against structural features
// (message length, code blocks, tool call history, conversation depth, attachments).
// Messages scoring below Threshold are sent to LightModel; all others use the
// agent's primary model. This reduces cost and latency for simple tasks without
// requiring any keyword matching — all scoring is language-agnostic.
type RoutingConfig struct {
	Enabled    bool    `json:"enabled"`
	LightModel string  `json:"light_model"` // model_name from model_list to use for simple tasks
	Threshold  float64 `json:"threshold"`   // complexity score in [0,1]; score >= threshold → primary model
}

type ToolFeedbackConfig struct {
	Enabled       bool `json:"enabled"          env:"KHUNQUANT_AGENTS_DEFAULTS_TOOL_FEEDBACK_ENABLED"`
	MaxArgsLength int  `json:"max_args_length"  env:"KHUNQUANT_AGENTS_DEFAULTS_TOOL_FEEDBACK_MAX_ARGS_LENGTH"`
}

type AgentDefaults struct {
	Workspace                 string             `json:"workspace"                       env:"KHUNQUANT_AGENTS_DEFAULTS_WORKSPACE"`
	RestrictToWorkspace       bool               `json:"restrict_to_workspace"           env:"KHUNQUANT_AGENTS_DEFAULTS_RESTRICT_TO_WORKSPACE"`
	AllowReadOutsideWorkspace bool               `json:"allow_read_outside_workspace"    env:"KHUNQUANT_AGENTS_DEFAULTS_ALLOW_READ_OUTSIDE_WORKSPACE"`
	Provider                  string             `json:"provider"                        env:"KHUNQUANT_AGENTS_DEFAULTS_PROVIDER"`
	ModelName                 string             `json:"model_name"                      env:"KHUNQUANT_AGENTS_DEFAULTS_MODEL_NAME"`
	Model                     string             `json:"model,omitempty"                 env:"KHUNQUANT_AGENTS_DEFAULTS_MODEL"` // Deprecated: use model_name instead
	ModelFallbacks            []string           `json:"model_fallbacks,omitempty"`
	ImageModel                string             `json:"image_model,omitempty"           env:"KHUNQUANT_AGENTS_DEFAULTS_IMAGE_MODEL"`
	ImageModelFallbacks       []string           `json:"image_model_fallbacks,omitempty"`
	MaxTokens                 int                `json:"max_tokens"                      env:"KHUNQUANT_AGENTS_DEFAULTS_MAX_TOKENS"`
	ContextWindow             int                `json:"context_window,omitempty"        env:"KHUNQUANT_AGENTS_DEFAULTS_CONTEXT_WINDOW"`
	Temperature               *float64           `json:"temperature,omitempty"           env:"KHUNQUANT_AGENTS_DEFAULTS_TEMPERATURE"`
	MaxToolIterations         int                `json:"max_tool_iterations"             env:"KHUNQUANT_AGENTS_DEFAULTS_MAX_TOOL_ITERATIONS"`
	SummarizeMessageThreshold int                `json:"summarize_message_threshold"     env:"KHUNQUANT_AGENTS_DEFAULTS_SUMMARIZE_MESSAGE_THRESHOLD"`
	SummarizeTokenPercent     int                `json:"summarize_token_percent"         env:"KHUNQUANT_AGENTS_DEFAULTS_SUMMARIZE_TOKEN_PERCENT"`
	MaxMediaSize              int                `json:"max_media_size,omitempty"        env:"KHUNQUANT_AGENTS_DEFAULTS_MAX_MEDIA_SIZE"`
	Routing                   *RoutingConfig     `json:"routing,omitempty"`
	ToolFeedback              ToolFeedbackConfig `json:"tool_feedback,omitempty"`
	ContextManager            string             `json:"context_manager,omitempty"       env:"KHUNQUANT_AGENTS_DEFAULTS_CONTEXT_MANAGER"`
	ContextManagerConfig      json.RawMessage    `json:"context_manager_config,omitempty"`
	FollowUpNudge             bool               `json:"follow_up_nudge"                 env:"KHUNQUANT_AGENTS_DEFAULTS_FOLLOW_UP_NUDGE"`
}

const DefaultMaxMediaSize = 20 * 1024 * 1024 // 20 MB

func (d *AgentDefaults) GetMaxMediaSize() int {
	if d.MaxMediaSize > 0 {
		return d.MaxMediaSize
	}
	return DefaultMaxMediaSize
}

// GetToolFeedbackMaxArgsLength returns the max args preview length for tool feedback messages.
func (d *AgentDefaults) GetToolFeedbackMaxArgsLength() int {
	if d.ToolFeedback.MaxArgsLength > 0 {
		return d.ToolFeedback.MaxArgsLength
	}
	return 300
}

// GetModelName returns the effective model name for the agent defaults.
// It prefers the new "model_name" field but falls back to "model" for backward compatibility.
func (d *AgentDefaults) GetModelName() string {
	if d.ModelName != "" {
		return d.ModelName
	}
	return d.Model
}

type ChannelsConfig struct {
	WhatsApp   WhatsAppConfig   `json:"whatsapp"    yaml:"-"`
	Telegram   TelegramConfig   `json:"telegram"    yaml:"telegram,omitempty"`
	Feishu     FeishuConfig     `json:"feishu"      yaml:"feishu,omitempty"`
	Discord    DiscordConfig    `json:"discord"     yaml:"discord,omitempty"`
	MaixCam    MaixCamConfig    `json:"maixcam"     yaml:"-"`
	QQ         QQConfig         `json:"qq"          yaml:"qq,omitempty"`
	DingTalk   DingTalkConfig   `json:"dingtalk"    yaml:"dingtalk,omitempty"`
	Slack      SlackConfig      `json:"slack"       yaml:"slack,omitempty"`
	Matrix     MatrixConfig     `json:"matrix"      yaml:"matrix,omitempty"`
	LINE       LINEConfig       `json:"line"        yaml:"line,omitempty"`
	OneBot     OneBotConfig     `json:"onebot"      yaml:"onebot,omitempty"`
	WeCom      WeComConfig      `json:"wecom"       yaml:"wecom,omitempty"`
	WeComApp   WeComAppConfig   `json:"wecom_app"   yaml:"wecom_app,omitempty"`
	WeComAIBot WeComAIBotConfig `json:"wecom_aibot" yaml:"wecom_aibot,omitempty"`
	Pico       PicoConfig       `json:"pico"        yaml:"pico,omitempty"`
	IRC        IRCConfig        `json:"irc"         yaml:"irc,omitempty"`
}

// GroupTriggerConfig controls when the bot responds in group chats.
type GroupTriggerConfig struct {
	MentionOnly bool     `json:"mention_only,omitempty"`
	Prefixes    []string `json:"prefixes,omitempty"`
}

// TypingConfig controls typing indicator behavior (Phase 10).
type TypingConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// PlaceholderConfig controls placeholder message behavior (Phase 10).
type PlaceholderConfig struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text,omitempty"`
}

type WhatsAppConfig struct {
	Enabled            bool                `json:"enabled"              yaml:"-" env:"KHUNQUANT_CHANNELS_WHATSAPP_ENABLED"`
	BridgeURL          string              `json:"bridge_url"           yaml:"-" env:"KHUNQUANT_CHANNELS_WHATSAPP_BRIDGE_URL"`
	UseNative          bool                `json:"use_native"           yaml:"-" env:"KHUNQUANT_CHANNELS_WHATSAPP_USE_NATIVE"`
	SessionStorePath   string              `json:"session_store_path"   yaml:"-" env:"KHUNQUANT_CHANNELS_WHATSAPP_SESSION_STORE_PATH"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"           yaml:"-" env:"KHUNQUANT_CHANNELS_WHATSAPP_ALLOW_FROM"`
	ReasoningChannelID string              `json:"reasoning_channel_id" yaml:"-" env:"KHUNQUANT_CHANNELS_WHATSAPP_REASONING_CHANNEL_ID"`
}

type TelegramConfig struct {
	Enabled            bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_TELEGRAM_ENABLED"`
	Token              SecureString        `json:"token,omitzero"          yaml:"token,omitempty" env:"KHUNQUANT_CHANNELS_TELEGRAM_TOKEN"`
	BaseURL            string              `json:"base_url"                yaml:"-" env:"KHUNQUANT_CHANNELS_TELEGRAM_BASE_URL"`
	Proxy              string              `json:"proxy"                   yaml:"-" env:"KHUNQUANT_CHANNELS_TELEGRAM_PROXY"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_TELEGRAM_ALLOW_FROM"`
	PairingEnabled     bool                `json:"pairing_enabled"         yaml:"-" env:"KHUNQUANT_CHANNELS_TELEGRAM_PAIRING_ENABLED"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"        yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"   yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_TELEGRAM_REASONING_CHANNEL_ID"`
}

type FeishuConfig struct {
	Enabled             bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_FEISHU_ENABLED"`
	AppID               string              `json:"app_id"                  yaml:"-" env:"KHUNQUANT_CHANNELS_FEISHU_APP_ID"`
	AppSecret           SecureString        `json:"app_secret,omitzero"     yaml:"app_secret,omitempty" env:"KHUNQUANT_CHANNELS_FEISHU_APP_SECRET"`
	EncryptKey          SecureString        `json:"encrypt_key,omitzero"    yaml:"encrypt_key,omitempty" env:"KHUNQUANT_CHANNELS_FEISHU_ENCRYPT_KEY"`
	VerificationToken   SecureString        `json:"verification_token,omitzero" yaml:"verification_token,omitempty" env:"KHUNQUANT_CHANNELS_FEISHU_VERIFICATION_TOKEN"`
	AllowFrom           FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_FEISHU_ALLOW_FROM"`
	GroupTrigger        GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	Placeholder         PlaceholderConfig   `json:"placeholder,omitempty"   yaml:"-"`
	ReasoningChannelID  string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_FEISHU_REASONING_CHANNEL_ID"`
	RandomReactionEmoji FlexibleStringSlice `json:"random_reaction_emoji"   yaml:"-" env:"KHUNQUANT_CHANNELS_FEISHU_RANDOM_REACTION_EMOJI"`
}

type DiscordConfig struct {
	Enabled            bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_DISCORD_ENABLED"`
	Token              SecureString        `json:"token,omitzero"          yaml:"token,omitempty" env:"KHUNQUANT_CHANNELS_DISCORD_TOKEN"`
	Proxy              string              `json:"proxy"                   yaml:"-" env:"KHUNQUANT_CHANNELS_DISCORD_PROXY"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_DISCORD_ALLOW_FROM"`
	MentionOnly        bool                `json:"mention_only"            yaml:"-" env:"KHUNQUANT_CHANNELS_DISCORD_MENTION_ONLY"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"        yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"   yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_DISCORD_REASONING_CHANNEL_ID"`
}

type MaixCamConfig struct {
	Enabled            bool                `json:"enabled"              yaml:"-" env:"KHUNQUANT_CHANNELS_MAIXCAM_ENABLED"`
	Host               string              `json:"host"                 yaml:"-" env:"KHUNQUANT_CHANNELS_MAIXCAM_HOST"`
	Port               int                 `json:"port"                 yaml:"-" env:"KHUNQUANT_CHANNELS_MAIXCAM_PORT"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"           yaml:"-" env:"KHUNQUANT_CHANNELS_MAIXCAM_ALLOW_FROM"`
	ReasoningChannelID string              `json:"reasoning_channel_id" yaml:"-" env:"KHUNQUANT_CHANNELS_MAIXCAM_REASONING_CHANNEL_ID"`
}

type QQConfig struct {
	Enabled            bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_QQ_ENABLED"`
	AppID              string              `json:"app_id"                  yaml:"-" env:"KHUNQUANT_CHANNELS_QQ_APP_ID"`
	AppSecret          SecureString        `json:"app_secret,omitzero"     yaml:"app_secret,omitempty" env:"KHUNQUANT_CHANNELS_QQ_APP_SECRET"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_QQ_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	MaxMessageLength   int                 `json:"max_message_length"      yaml:"-" env:"KHUNQUANT_CHANNELS_QQ_MAX_MESSAGE_LENGTH"`
	SendMarkdown       bool                `json:"send_markdown"           yaml:"-" env:"KHUNQUANT_CHANNELS_QQ_SEND_MARKDOWN"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_QQ_REASONING_CHANNEL_ID"`
}

type DingTalkConfig struct {
	Enabled            bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_DINGTALK_ENABLED"`
	ClientID           string              `json:"client_id"               yaml:"-" env:"KHUNQUANT_CHANNELS_DINGTALK_CLIENT_ID"`
	ClientSecret       SecureString        `json:"client_secret,omitzero"  yaml:"client_secret,omitempty" env:"KHUNQUANT_CHANNELS_DINGTALK_CLIENT_SECRET"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_DINGTALK_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_DINGTALK_REASONING_CHANNEL_ID"`
}

type SlackConfig struct {
	Enabled            bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_SLACK_ENABLED"`
	BotToken           SecureString        `json:"bot_token,omitzero"      yaml:"bot_token,omitempty" env:"KHUNQUANT_CHANNELS_SLACK_BOT_TOKEN"`
	AppToken           SecureString        `json:"app_token,omitzero"      yaml:"app_token,omitempty" env:"KHUNQUANT_CHANNELS_SLACK_APP_TOKEN"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_SLACK_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"        yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"   yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_SLACK_REASONING_CHANNEL_ID"`
}

type MatrixConfig struct {
	Enabled            bool                `json:"enabled"                  yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_ENABLED"`
	Homeserver         string              `json:"homeserver"               yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_HOMESERVER"`
	UserID             string              `json:"user_id"                  yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_USER_ID"`
	AccessToken        SecureString        `json:"access_token,omitzero"    yaml:"access_token,omitempty" env:"KHUNQUANT_CHANNELS_MATRIX_ACCESS_TOKEN"`
	DeviceID           string              `json:"device_id,omitempty"      yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_DEVICE_ID"`
	JoinOnInvite       bool                `json:"join_on_invite"           yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_JOIN_ON_INVITE"`
	MessageFormat      string              `json:"message_format,omitempty" yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_MESSAGE_FORMAT"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"               yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty"  yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"    yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"     yaml:"-" env:"KHUNQUANT_CHANNELS_MATRIX_REASONING_CHANNEL_ID"`
}

type LINEConfig struct {
	Enabled            bool                `json:"enabled"                        yaml:"-" env:"KHUNQUANT_CHANNELS_LINE_ENABLED"`
	ChannelSecret      SecureString        `json:"channel_secret,omitzero"        yaml:"channel_secret,omitempty" env:"KHUNQUANT_CHANNELS_LINE_CHANNEL_SECRET"`
	ChannelAccessToken SecureString        `json:"channel_access_token,omitzero"  yaml:"channel_access_token,omitempty" env:"KHUNQUANT_CHANNELS_LINE_CHANNEL_ACCESS_TOKEN"`
	WebhookHost        string              `json:"webhook_host"                   yaml:"-" env:"KHUNQUANT_CHANNELS_LINE_WEBHOOK_HOST"`
	WebhookPort        int                 `json:"webhook_port"                   yaml:"-" env:"KHUNQUANT_CHANNELS_LINE_WEBHOOK_PORT"`
	WebhookPath        string              `json:"webhook_path"                   yaml:"-" env:"KHUNQUANT_CHANNELS_LINE_WEBHOOK_PATH"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"                     yaml:"-" env:"KHUNQUANT_CHANNELS_LINE_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty"        yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"               yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"          yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"           yaml:"-" env:"KHUNQUANT_CHANNELS_LINE_REASONING_CHANNEL_ID"`
}

type OneBotConfig struct {
	Enabled            bool                `json:"enabled"                 yaml:"-" env:"KHUNQUANT_CHANNELS_ONEBOT_ENABLED"`
	WSUrl              string              `json:"ws_url"                  yaml:"-" env:"KHUNQUANT_CHANNELS_ONEBOT_WS_URL"`
	AccessToken        SecureString        `json:"access_token,omitzero"   yaml:"access_token,omitempty" env:"KHUNQUANT_CHANNELS_ONEBOT_ACCESS_TOKEN"`
	ReconnectInterval  int                 `json:"reconnect_interval"      yaml:"-" env:"KHUNQUANT_CHANNELS_ONEBOT_RECONNECT_INTERVAL"`
	GroupTriggerPrefix []string            `json:"group_trigger_prefix"    yaml:"-" env:"KHUNQUANT_CHANNELS_ONEBOT_GROUP_TRIGGER_PREFIX"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"              yaml:"-" env:"KHUNQUANT_CHANNELS_ONEBOT_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"        yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"   yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-" env:"KHUNQUANT_CHANNELS_ONEBOT_REASONING_CHANNEL_ID"`
}

type WeComConfig struct {
	Enabled            bool                `json:"enabled"                     yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_ENABLED"`
	Token              SecureString        `json:"token,omitzero"              yaml:"token,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_TOKEN"`
	EncodingAESKey     SecureString        `json:"encoding_aes_key,omitzero"   yaml:"encoding_aes_key,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_ENCODING_AES_KEY"`
	WebhookURL         string              `json:"webhook_url"                 yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_WEBHOOK_URL"`
	WebhookHost        string              `json:"webhook_host"                yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_WEBHOOK_HOST"`
	WebhookPort        int                 `json:"webhook_port"                yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_WEBHOOK_PORT"`
	WebhookPath        string              `json:"webhook_path"                yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_WEBHOOK_PATH"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"                  yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_ALLOW_FROM"`
	ReplyTimeout       int                 `json:"reply_timeout"               yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_REPLY_TIMEOUT"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty"     yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"        yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_REASONING_CHANNEL_ID"`
}

type WeComAppConfig struct {
	Enabled            bool                `json:"enabled"                      yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_ENABLED"`
	CorpID             string              `json:"corp_id"                      yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_CORP_ID"`
	CorpSecret         SecureString        `json:"corp_secret,omitzero"         yaml:"corp_secret,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_APP_CORP_SECRET"`
	AgentID            int64               `json:"agent_id"                     yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_AGENT_ID"`
	Token              SecureString        `json:"token,omitzero"               yaml:"token,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_APP_TOKEN"`
	EncodingAESKey     SecureString        `json:"encoding_aes_key,omitzero"    yaml:"encoding_aes_key,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_APP_ENCODING_AES_KEY"`
	WebhookHost        string              `json:"webhook_host"                 yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_WEBHOOK_HOST"`
	WebhookPort        int                 `json:"webhook_port"                 yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_WEBHOOK_PORT"`
	WebhookPath        string              `json:"webhook_path"                 yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_WEBHOOK_PATH"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"                   yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_ALLOW_FROM"`
	ReplyTimeout       int                 `json:"reply_timeout"                yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_REPLY_TIMEOUT"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty"      yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"         yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_APP_REASONING_CHANNEL_ID"`
}

type WeComAIBotConfig struct {
	Enabled            bool                `json:"enabled"                   yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_ENABLED"`
	Token              SecureString        `json:"token,omitzero"            yaml:"token,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_TOKEN"`
	EncodingAESKey     SecureString        `json:"encoding_aes_key,omitzero" yaml:"encoding_aes_key,omitempty" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_ENCODING_AES_KEY"`
	WebhookPath        string              `json:"webhook_path"              yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_WEBHOOK_PATH"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"                yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_ALLOW_FROM"`
	ReplyTimeout       int                 `json:"reply_timeout"             yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_REPLY_TIMEOUT"`
	MaxSteps           int                 `json:"max_steps"                 yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_MAX_STEPS"`
	WelcomeMessage     string              `json:"welcome_message"           yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_WELCOME_MESSAGE"`
	ReasoningChannelID string              `json:"reasoning_channel_id"      yaml:"-" env:"KHUNQUANT_CHANNELS_WECOM_AIBOT_REASONING_CHANNEL_ID"`
}

type PicoConfig struct {
	Enabled         bool                `json:"enabled"                     yaml:"-" env:"KHUNQUANT_CHANNELS_PICO_ENABLED"`
	Token           SecureString        `json:"token,omitzero"              yaml:"token,omitempty" env:"KHUNQUANT_CHANNELS_PICO_TOKEN"`
	AllowTokenQuery bool                `json:"allow_token_query,omitempty" yaml:"-"`
	AllowOrigins    []string            `json:"allow_origins,omitempty"     yaml:"-"`
	PingInterval    int                 `json:"ping_interval,omitempty"     yaml:"-"`
	ReadTimeout     int                 `json:"read_timeout,omitempty"      yaml:"-"`
	WriteTimeout    int                 `json:"write_timeout,omitempty"     yaml:"-"`
	MaxConnections  int                 `json:"max_connections,omitempty"   yaml:"-"`
	AllowFrom       FlexibleStringSlice `json:"allow_from"                  yaml:"-" env:"KHUNQUANT_CHANNELS_PICO_ALLOW_FROM"`
	Placeholder     PlaceholderConfig   `json:"placeholder,omitempty"       yaml:"-"`
}

type IRCConfig struct {
	Enabled            bool                `json:"enabled"                      yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_ENABLED"`
	Server             string              `json:"server"                       yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_SERVER"`
	TLS                bool                `json:"tls"                          yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_TLS"`
	Nick               string              `json:"nick"                         yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_NICK"`
	User               string              `json:"user,omitempty"               yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_USER"`
	RealName           string              `json:"real_name,omitempty"          yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_REAL_NAME"`
	Password           SecureString        `json:"password,omitzero"            yaml:"password,omitempty" env:"KHUNQUANT_CHANNELS_IRC_PASSWORD"`
	NickServPassword   SecureString        `json:"nickserv_password,omitzero"   yaml:"nickserv_password,omitempty" env:"KHUNQUANT_CHANNELS_IRC_NICKSERV_PASSWORD"`
	SASLUser           string              `json:"sasl_user"                    yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_SASL_USER"`
	SASLPassword       SecureString        `json:"sasl_password,omitzero"       yaml:"sasl_password,omitempty" env:"KHUNQUANT_CHANNELS_IRC_SASL_PASSWORD"`
	Channels           FlexibleStringSlice `json:"channels"                     yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_CHANNELS"`
	RequestCaps        FlexibleStringSlice `json:"request_caps,omitempty"       yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_REQUEST_CAPS"`
	AllowFrom          FlexibleStringSlice `json:"allow_from"                   yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_ALLOW_FROM"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty"      yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"             yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"         yaml:"-" env:"KHUNQUANT_CHANNELS_IRC_REASONING_CHANNEL_ID"`
}

type HeartbeatConfig struct {
	Enabled  bool `json:"enabled"  env:"KHUNQUANT_HEARTBEAT_ENABLED"`
	Interval int  `json:"interval" env:"KHUNQUANT_HEARTBEAT_INTERVAL"` // minutes, min 5
}

type DevicesConfig struct {
	Enabled    bool `json:"enabled"     env:"KHUNQUANT_DEVICES_ENABLED"`
	MonitorUSB bool `json:"monitor_usb" env:"KHUNQUANT_DEVICES_MONITOR_USB"`
}

type VoiceConfig struct {
	EchoTranscription bool `json:"echo_transcription" env:"KHUNQUANT_VOICE_ECHO_TRANSCRIPTION"`
}

type ProvidersConfig struct {
	Anthropic     ProviderConfig       `json:"anthropic"`
	OpenAI        OpenAIProviderConfig `json:"openai"`
	LiteLLM       ProviderConfig       `json:"litellm"`
	OpenRouter    ProviderConfig       `json:"openrouter"`
	Groq          ProviderConfig       `json:"groq"`
	Zhipu         ProviderConfig       `json:"zhipu"`
	VLLM          ProviderConfig       `json:"vllm"`
	Gemini        ProviderConfig       `json:"gemini"`
	Nvidia        ProviderConfig       `json:"nvidia"`
	Ollama        ProviderConfig       `json:"ollama"`
	Moonshot      ProviderConfig       `json:"moonshot"`
	ShengSuanYun  ProviderConfig       `json:"shengsuanyun"`
	DeepSeek      ProviderConfig       `json:"deepseek"`
	Cerebras      ProviderConfig       `json:"cerebras"`
	Vivgrid       ProviderConfig       `json:"vivgrid"`
	VolcEngine    ProviderConfig       `json:"volcengine"`
	GitHubCopilot ProviderConfig       `json:"github_copilot"`
	Antigravity   ProviderConfig       `json:"antigravity"`
	Qwen          ProviderConfig       `json:"qwen"`
	Mistral       ProviderConfig       `json:"mistral"`
	Avian         ProviderConfig       `json:"avian"`
	Minimax       ProviderConfig       `json:"minimax"`
	LongCat       ProviderConfig       `json:"longcat"`
	LlamaCpp      ProviderConfig       `json:"llamacpp"`
	MLXLM         ProviderConfig       `json:"mlx_lm"`
	ModelScope    ProviderConfig       `json:"modelscope"`
}

// IsEmpty checks if all provider configs are empty (no API keys or API bases set)
// Note: WebSearch is an optimization option and doesn't count as "non-empty"
func (p ProvidersConfig) IsEmpty() bool {
	return p.Anthropic.APIKey == "" && p.Anthropic.APIBase == "" &&
		p.OpenAI.APIKey == "" && p.OpenAI.APIBase == "" &&
		p.LiteLLM.APIKey == "" && p.LiteLLM.APIBase == "" &&
		p.OpenRouter.APIKey == "" && p.OpenRouter.APIBase == "" &&
		p.Groq.APIKey == "" && p.Groq.APIBase == "" &&
		p.Zhipu.APIKey == "" && p.Zhipu.APIBase == "" &&
		p.VLLM.APIKey == "" && p.VLLM.APIBase == "" &&
		p.Gemini.APIKey == "" && p.Gemini.APIBase == "" &&
		p.Nvidia.APIKey == "" && p.Nvidia.APIBase == "" &&
		p.Ollama.APIKey == "" && p.Ollama.APIBase == "" &&
		p.Moonshot.APIKey == "" && p.Moonshot.APIBase == "" &&
		p.ShengSuanYun.APIKey == "" && p.ShengSuanYun.APIBase == "" &&
		p.DeepSeek.APIKey == "" && p.DeepSeek.APIBase == "" &&
		p.Cerebras.APIKey == "" && p.Cerebras.APIBase == "" &&
		p.Vivgrid.APIKey == "" && p.Vivgrid.APIBase == "" &&
		p.VolcEngine.APIKey == "" && p.VolcEngine.APIBase == "" &&
		p.GitHubCopilot.APIKey == "" && p.GitHubCopilot.APIBase == "" &&
		p.Antigravity.APIKey == "" && p.Antigravity.APIBase == "" &&
		p.Qwen.APIKey == "" && p.Qwen.APIBase == "" &&
		p.Mistral.APIKey == "" && p.Mistral.APIBase == "" &&
		p.Avian.APIKey == "" && p.Avian.APIBase == "" &&
		p.Minimax.APIKey == "" && p.Minimax.APIBase == "" &&
		p.LongCat.APIKey == "" && p.LongCat.APIBase == "" &&
		p.ModelScope.APIKey == "" && p.ModelScope.APIBase == "" &&
		p.LlamaCpp.APIKey == "" && p.LlamaCpp.APIBase == "" &&
		p.MLXLM.APIKey == "" && p.MLXLM.APIBase == ""
}

// MarshalJSON implements custom JSON marshaling for ProvidersConfig
// to omit the entire section when empty
func (p ProvidersConfig) MarshalJSON() ([]byte, error) {
	if p.IsEmpty() {
		return []byte("null"), nil
	}
	type Alias ProvidersConfig
	return json.Marshal((*Alias)(&p))
}

type ProviderConfig struct {
	APIKey         string `json:"api_key"                   env:"KHUNQUANT_PROVIDERS_{{.Name}}_API_KEY"`
	APIBase        string `json:"api_base"                  env:"KHUNQUANT_PROVIDERS_{{.Name}}_API_BASE"`
	Proxy          string `json:"proxy,omitempty"           env:"KHUNQUANT_PROVIDERS_{{.Name}}_PROXY"`
	RequestTimeout int    `json:"request_timeout,omitempty" env:"KHUNQUANT_PROVIDERS_{{.Name}}_REQUEST_TIMEOUT"`
	AuthMethod     string `json:"auth_method,omitempty"     env:"KHUNQUANT_PROVIDERS_{{.Name}}_AUTH_METHOD"`
	ConnectMode    string `json:"connect_mode,omitempty"    env:"KHUNQUANT_PROVIDERS_{{.Name}}_CONNECT_MODE"` // only for Github Copilot, `stdio` or `grpc`
}

type OpenAIProviderConfig struct {
	ProviderConfig
	WebSearch bool `json:"web_search" env:"KHUNQUANT_PROVIDERS_OPENAI_WEB_SEARCH"`
}

// ModelConfig represents a model-centric provider configuration.
// It allows adding new providers (especially OpenAI-compatible ones) via configuration only.
// The model field uses protocol prefix format: [protocol/]model-identifier
// Supported protocols: openai, anthropic, antigravity, claude-cli, codex-cli, github-copilot
// Default protocol is "openai" if no prefix is specified.
type ModelConfig struct {
	// Required fields
	ModelName string `json:"model_name"` // User-facing alias for the model
	Model     string `json:"model"`      // Protocol/model-identifier (e.g., "openai/gpt-4o", "anthropic/claude-sonnet-4.6")

	// HTTP-based providers
	APIBase string       `json:"api_base,omitempty"` // API endpoint URL
	APIKey  SecureString `json:"api_key"`            // API authentication key
	Proxy   string       `json:"proxy,omitempty"`    // HTTP proxy URL

	// Special providers (CLI-based, OAuth, etc.)
	AuthMethod  string `json:"auth_method,omitempty"`  // Authentication method: oauth, token
	ConnectMode string `json:"connect_mode,omitempty"` // Connection mode: stdio, grpc
	Workspace   string `json:"workspace,omitempty"`    // Workspace path for CLI-based providers

	// Optional optimizations
	RPM            int    `json:"rpm,omitempty"`              // Requests per minute limit
	MaxTokensField string `json:"max_tokens_field,omitempty"` // Field name for max tokens (e.g., "max_completion_tokens")
	RequestTimeout int    `json:"request_timeout,omitempty"`
	ThinkingLevel  string `json:"thinking_level,omitempty"` // Extended thinking: off|low|medium|high|xhigh|adaptive
}

// Validate checks if the ModelConfig has all required fields.
func (c *ModelConfig) Validate() error {
	if c.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	if c.Model == "" {
		return fmt.Errorf("model is required")
	}
	return nil
}

type GatewayConfig struct {
	Host string `json:"host" env:"KHUNQUANT_GATEWAY_HOST"`
	Port int    `json:"port" env:"KHUNQUANT_GATEWAY_PORT"`
}

type ToolDiscoveryConfig struct {
	Enabled          bool `json:"enabled"            env:"KHUNQUANT_TOOLS_DISCOVERY_ENABLED"`
	TTL              int  `json:"ttl"                env:"KHUNQUANT_TOOLS_DISCOVERY_TTL"`
	MaxSearchResults int  `json:"max_search_results" env:"KHUNQUANT_MAX_SEARCH_RESULTS"`
	UseBM25          bool `json:"use_bm25"           env:"KHUNQUANT_TOOLS_DISCOVERY_USE_BM25"`
	UseRegex         bool `json:"use_regex"          env:"KHUNQUANT_TOOLS_DISCOVERY_USE_REGEX"`
}

type ToolConfig struct {
	Enabled bool `json:"enabled" env:"ENABLED"`
}

type BraveConfig struct {
	Enabled    bool     `json:"enabled"     env:"KHUNQUANT_TOOLS_WEB_BRAVE_ENABLED"`
	APIKey     string   `json:"api_key"     env:"KHUNQUANT_TOOLS_WEB_BRAVE_API_KEY"`
	APIKeys    []string `json:"api_keys"    env:"KHUNQUANT_TOOLS_WEB_BRAVE_API_KEYS"`
	MaxResults int      `json:"max_results" env:"KHUNQUANT_TOOLS_WEB_BRAVE_MAX_RESULTS"`
}

type TavilyConfig struct {
	Enabled    bool     `json:"enabled"     env:"KHUNQUANT_TOOLS_WEB_TAVILY_ENABLED"`
	APIKey     string   `json:"api_key"     env:"KHUNQUANT_TOOLS_WEB_TAVILY_API_KEY"`
	APIKeys    []string `json:"api_keys"    env:"KHUNQUANT_TOOLS_WEB_TAVILY_API_KEYS"`
	BaseURL    string   `json:"base_url"    env:"KHUNQUANT_TOOLS_WEB_TAVILY_BASE_URL"`
	MaxResults int      `json:"max_results" env:"KHUNQUANT_TOOLS_WEB_TAVILY_MAX_RESULTS"`
}

type DuckDuckGoConfig struct {
	Enabled    bool `json:"enabled"     env:"KHUNQUANT_TOOLS_WEB_DUCKDUCKGO_ENABLED"`
	MaxResults int  `json:"max_results" env:"KHUNQUANT_TOOLS_WEB_DUCKDUCKGO_MAX_RESULTS"`
}

type PerplexityConfig struct {
	Enabled    bool     `json:"enabled"     env:"KHUNQUANT_TOOLS_WEB_PERPLEXITY_ENABLED"`
	APIKey     string   `json:"api_key"     env:"KHUNQUANT_TOOLS_WEB_PERPLEXITY_API_KEY"`
	APIKeys    []string `json:"api_keys"    env:"KHUNQUANT_TOOLS_WEB_PERPLEXITY_API_KEYS"`
	MaxResults int      `json:"max_results" env:"KHUNQUANT_TOOLS_WEB_PERPLEXITY_MAX_RESULTS"`
}

type SearXNGConfig struct {
	Enabled    bool   `json:"enabled"     env:"KHUNQUANT_TOOLS_WEB_SEARXNG_ENABLED"`
	BaseURL    string `json:"base_url"    env:"KHUNQUANT_TOOLS_WEB_SEARXNG_BASE_URL"`
	MaxResults int    `json:"max_results" env:"KHUNQUANT_TOOLS_WEB_SEARXNG_MAX_RESULTS"`
}

type GLMSearchConfig struct {
	Enabled bool   `json:"enabled"  env:"KHUNQUANT_TOOLS_WEB_GLM_ENABLED"`
	APIKey  string `json:"api_key"  env:"KHUNQUANT_TOOLS_WEB_GLM_API_KEY"`
	BaseURL string `json:"base_url" env:"KHUNQUANT_TOOLS_WEB_GLM_BASE_URL"`
	// SearchEngine specifies the search backend: "search_std" (default),
	// "search_pro", "search_pro_sogou", or "search_pro_quark".
	SearchEngine string `json:"search_engine" env:"KHUNQUANT_TOOLS_WEB_GLM_SEARCH_ENGINE"`
	MaxResults   int    `json:"max_results"   env:"KHUNQUANT_TOOLS_WEB_GLM_MAX_RESULTS"`
}

type WebToolsConfig struct {
	ToolConfig `                 envPrefix:"KHUNQUANT_TOOLS_WEB_"`
	Brave      BraveConfig      `                                json:"brave"`
	Tavily     TavilyConfig     `                                json:"tavily"`
	DuckDuckGo DuckDuckGoConfig `                                json:"duckduckgo"`
	Perplexity PerplexityConfig `                                json:"perplexity"`
	SearXNG    SearXNGConfig    `                                json:"searxng"`
	GLMSearch  GLMSearchConfig  `                                json:"glm_search"`
	// Proxy is an optional proxy URL for web tools (http/https/socks5/socks5h).
	// For authenticated proxies, prefer HTTP_PROXY/HTTPS_PROXY env vars instead of embedding credentials in config.
	Proxy           string `json:"proxy,omitempty"             env:"KHUNQUANT_TOOLS_WEB_PROXY"`
	FetchLimitBytes int64  `json:"fetch_limit_bytes,omitempty" env:"KHUNQUANT_TOOLS_WEB_FETCH_LIMIT_BYTES"`
}

type CronToolsConfig struct {
	ToolConfig         `    envPrefix:"KHUNQUANT_TOOLS_CRON_"`
	ExecTimeoutMinutes int `                                 env:"KHUNQUANT_TOOLS_CRON_EXEC_TIMEOUT_MINUTES" json:"exec_timeout_minutes"` // 0 means no timeout
}

type ExecConfig struct {
	ToolConfig          `         envPrefix:"KHUNQUANT_TOOLS_EXEC_"`
	EnableDenyPatterns  bool     `                                 env:"KHUNQUANT_TOOLS_EXEC_ENABLE_DENY_PATTERNS"  json:"enable_deny_patterns"`
	AllowRemote         bool     `                                 env:"KHUNQUANT_TOOLS_EXEC_ALLOW_REMOTE"          json:"allow_remote"`
	CustomDenyPatterns  []string `                                 env:"KHUNQUANT_TOOLS_EXEC_CUSTOM_DENY_PATTERNS"  json:"custom_deny_patterns"`
	CustomAllowPatterns []string `                                 env:"KHUNQUANT_TOOLS_EXEC_CUSTOM_ALLOW_PATTERNS" json:"custom_allow_patterns"`
	TimeoutSeconds      int      `                                 env:"KHUNQUANT_TOOLS_EXEC_TIMEOUT_SECONDS"       json:"timeout_seconds"` // 0 means use default (60s)
}

type SkillsToolsConfig struct {
	ToolConfig            `                       envPrefix:"KHUNQUANT_TOOLS_SKILLS_"`
	Registries            SkillsRegistriesConfig `                                   json:"registries"`
	Github                SkillsGithubConfig     `                                   json:"github"`
	MaxConcurrentSearches int                    `                                   json:"max_concurrent_searches" env:"KHUNQUANT_TOOLS_SKILLS_MAX_CONCURRENT_SEARCHES"`
	SearchCache           SearchCacheConfig      `                                   json:"search_cache"`
}

type MediaCleanupConfig struct {
	ToolConfig `    envPrefix:"KHUNQUANT_MEDIA_CLEANUP_"`
	MaxAge     int `                                    env:"KHUNQUANT_MEDIA_CLEANUP_MAX_AGE"  json:"max_age_minutes"`
	Interval   int `                                    env:"KHUNQUANT_MEDIA_CLEANUP_INTERVAL" json:"interval_minutes"`
}

type ReadFileToolConfig struct {
	Enabled         bool `json:"enabled"`
	MaxReadFileSize int  `json:"max_read_file_size"`
}

type ToolsConfig struct {
	// FilterSensitiveData controls whether to filter API keys, tokens, and secrets
	// from tool results before sending content to the LLM. Default: true (enabled).
	FilterSensitiveData bool `json:"filter_sensitive_data" yaml:"-" env:"KHUNQUANT_TOOLS_FILTER_SENSITIVE_DATA"`
	// FilterMinLength is the minimum content length required for filtering.
	// Content shorter than this is returned unchanged for performance. Default: 8.
	FilterMinLength int `json:"filter_min_length" yaml:"-" env:"KHUNQUANT_TOOLS_FILTER_MIN_LENGTH"`

	AllowReadPaths  []string           `json:"allow_read_paths"  env:"KHUNQUANT_TOOLS_ALLOW_READ_PATHS"`
	AllowWritePaths []string           `json:"allow_write_paths" env:"KHUNQUANT_TOOLS_ALLOW_WRITE_PATHS"`
	Web             WebToolsConfig     `json:"web"`
	Cron            CronToolsConfig    `json:"cron"`
	Exec            ExecConfig         `json:"exec"`
	Skills          SkillsToolsConfig  `json:"skills"`
	MediaCleanup    MediaCleanupConfig `json:"media_cleanup"`
	MCP             MCPConfig          `json:"mcp"`
	AppendFile      ToolConfig         `json:"append_file"                                              envPrefix:"KHUNQUANT_TOOLS_APPEND_FILE_"`
	EditFile        ToolConfig         `json:"edit_file"                                                envPrefix:"KHUNQUANT_TOOLS_EDIT_FILE_"`
	FindSkills      ToolConfig         `json:"find_skills"                                              envPrefix:"KHUNQUANT_TOOLS_FIND_SKILLS_"`
	I2C             ToolConfig         `json:"i2c"                                                      envPrefix:"KHUNQUANT_TOOLS_I2C_"`
	InstallSkill    ToolConfig         `json:"install_skill"                                            envPrefix:"KHUNQUANT_TOOLS_INSTALL_SKILL_"`
	ListDir         ToolConfig         `json:"list_dir"                                                 envPrefix:"KHUNQUANT_TOOLS_LIST_DIR_"`
	Message         ToolConfig         `json:"message"                                                  envPrefix:"KHUNQUANT_TOOLS_MESSAGE_"`
	ReadFile        ReadFileToolConfig `json:"read_file"                                                envPrefix:"KHUNQUANT_TOOLS_READ_FILE_"`
	SendFile        ToolConfig         `json:"send_file"                                                envPrefix:"KHUNQUANT_TOOLS_SEND_FILE_"`
	Spawn           ToolConfig         `json:"spawn"                                                    envPrefix:"KHUNQUANT_TOOLS_SPAWN_"`
	SPI             ToolConfig         `json:"spi"                                                      envPrefix:"KHUNQUANT_TOOLS_SPI_"`
	Subagent        ToolConfig         `json:"subagent"                                                 envPrefix:"KHUNQUANT_TOOLS_SUBAGENT_"`
	WebFetch        ToolConfig         `json:"web_fetch"                                                envPrefix:"KHUNQUANT_TOOLS_WEB_FETCH_"`
	WriteFile       ToolConfig         `json:"write_file"                                               envPrefix:"KHUNQUANT_TOOLS_WRITE_FILE_"`
	GetAssetsList   ToolConfig         `json:"get_assets_list"                                          envPrefix:"KHUNQUANT_TOOLS_GET_ASSETS_LIST_"`
	GetTotalValue   ToolConfig         `json:"get_total_value"                                          envPrefix:"KHUNQUANT_TOOLS_GET_TOTAL_VALUE_"`
	ListPortfolios  ToolConfig         `json:"list_portfolios"                                          envPrefix:"KHUNQUANT_TOOLS_LIST_PORTFOLIOS_"`
	TakeSnapshot    ToolConfig         `json:"take_snapshot"                                            envPrefix:"KHUNQUANT_TOOLS_TAKE_SNAPSHOT_"`
	QuerySnapshots  ToolConfig         `json:"query_snapshots"                                          envPrefix:"KHUNQUANT_TOOLS_QUERY_SNAPSHOTS_"`
	SnapshotSummary ToolConfig         `json:"snapshot_summary"                                         envPrefix:"KHUNQUANT_TOOLS_SNAPSHOT_SUMMARY_"`
	DeleteSnapshots ToolConfig         `json:"delete_snapshots"                                         envPrefix:"KHUNQUANT_TOOLS_DELETE_SNAPSHOTS_"`

	// Market intelligence tools (Track A)
	GetTicker    ToolConfig `json:"get_ticker"    envPrefix:"KHUNQUANT_TOOLS_GET_TICKER_"`
	GetTickers   ToolConfig `json:"get_tickers"   envPrefix:"KHUNQUANT_TOOLS_GET_TICKERS_"`
	GetOHLCV     ToolConfig `json:"get_ohlcv"     envPrefix:"KHUNQUANT_TOOLS_GET_OHLCV_"`
	GetOrderBook ToolConfig `json:"get_orderbook" envPrefix:"KHUNQUANT_TOOLS_GET_ORDERBOOK_"`
	GetMarkets   ToolConfig `json:"get_markets"   envPrefix:"KHUNQUANT_TOOLS_GET_MARKETS_"`

	// Order execution tools (Track B)
	CreateOrder        ToolConfig `json:"create_order"         envPrefix:"KHUNQUANT_TOOLS_CREATE_ORDER_"`
	CancelOrder        ToolConfig `json:"cancel_order"         envPrefix:"KHUNQUANT_TOOLS_CANCEL_ORDER_"`
	GetOrder           ToolConfig `json:"get_order"            envPrefix:"KHUNQUANT_TOOLS_GET_ORDER_"`
	GetOpenOrders      ToolConfig `json:"get_open_orders"      envPrefix:"KHUNQUANT_TOOLS_GET_OPEN_ORDERS_"`
	GetOrderHistory    ToolConfig `json:"get_order_history"    envPrefix:"KHUNQUANT_TOOLS_GET_ORDER_HISTORY_"`
	GetTradeHistory    ToolConfig `json:"get_trade_history"    envPrefix:"KHUNQUANT_TOOLS_GET_TRADE_HISTORY_"`
	EmergencyStop      ToolConfig `json:"emergency_stop"       envPrefix:"KHUNQUANT_TOOLS_EMERGENCY_STOP_"`
	PaperTrade         ToolConfig `json:"paper_trade"          envPrefix:"KHUNQUANT_TOOLS_PAPER_TRADE_"`
	GetOrderRateStatus ToolConfig `json:"get_order_rate_status" envPrefix:"KHUNQUANT_TOOLS_GET_ORDER_RATE_STATUS_"`

	// Technical analysis tools (Track C)
	CalculateIndicators ToolConfig `json:"calculate_indicators" envPrefix:"KHUNQUANT_TOOLS_CALCULATE_INDICATORS_"`
	MarketAnalysis      ToolConfig `json:"market_analysis"      envPrefix:"KHUNQUANT_TOOLS_MARKET_ANALYSIS_"`
	PortfolioAllocation ToolConfig `json:"portfolio_allocation" envPrefix:"KHUNQUANT_TOOLS_PORTFOLIO_ALLOCATION_"`

	// Alert and transfer tools (Track D)
	SetPriceAlert     ToolConfig `json:"set_price_alert"     envPrefix:"KHUNQUANT_TOOLS_SET_PRICE_ALERT_"`
	SetIndicatorAlert ToolConfig `json:"set_indicator_alert" envPrefix:"KHUNQUANT_TOOLS_SET_INDICATOR_ALERT_"`
	TransferFunds     ToolConfig `json:"transfer_funds"      envPrefix:"KHUNQUANT_TOOLS_TRANSFER_FUNDS_"`

	// DCA — Dollar Cost Averaging (Track E)
	CreateDCAPlan   ToolConfig `json:"create_dca_plan"   envPrefix:"KHUNQUANT_TOOLS_CREATE_DCA_PLAN_"`
	ListDCAPlans    ToolConfig `json:"list_dca_plans"    envPrefix:"KHUNQUANT_TOOLS_LIST_DCA_PLANS_"`
	UpdateDCAPlan   ToolConfig `json:"update_dca_plan"   envPrefix:"KHUNQUANT_TOOLS_UPDATE_DCA_PLAN_"`
	DeleteDCAPlan   ToolConfig `json:"delete_dca_plan"   envPrefix:"KHUNQUANT_TOOLS_DELETE_DCA_PLAN_"`
	ExecuteDCAOrder ToolConfig `json:"execute_dca_order" envPrefix:"KHUNQUANT_TOOLS_EXECUTE_DCA_ORDER_"`
	GetDCAHistory   ToolConfig `json:"get_dca_history"   envPrefix:"KHUNQUANT_TOOLS_GET_DCA_HISTORY_"`
	GetDCASummary   ToolConfig `json:"get_dca_summary"   envPrefix:"KHUNQUANT_TOOLS_GET_DCA_SUMMARY_"`

	// PnL — Profit and Loss (Track F)
	GetPnLSummary ToolConfig `json:"get_pnl_summary" envPrefix:"KHUNQUANT_TOOLS_GET_PNL_SUMMARY_"`
	GetPnLDetail  ToolConfig `json:"get_pnl_detail"  envPrefix:"KHUNQUANT_TOOLS_GET_PNL_DETAIL_"`

	// Security tools
	ConfigEncryptKeys ToolConfig `json:"config_encrypt_keys" envPrefix:"KHUNQUANT_TOOLS_CONFIG_ENCRYPT_KEYS_"`
}

// IsFilterSensitiveDataEnabled returns true if sensitive data filtering is enabled.
func (c *ToolsConfig) IsFilterSensitiveDataEnabled() bool {
	return c.FilterSensitiveData
}

// GetFilterMinLength returns the minimum content length for filtering (default: 8).
func (c *ToolsConfig) GetFilterMinLength() int {
	if c.FilterMinLength <= 0 {
		return 8
	}
	return c.FilterMinLength
}

type SearchCacheConfig struct {
	MaxSize    int `json:"max_size"    env:"KHUNQUANT_SKILLS_SEARCH_CACHE_MAX_SIZE"`
	TTLSeconds int `json:"ttl_seconds" env:"KHUNQUANT_SKILLS_SEARCH_CACHE_TTL_SECONDS"`
}

type SkillsRegistriesConfig struct {
	ClawHub ClawHubRegistryConfig `json:"clawhub"`
}

type SkillsGithubConfig struct {
	Token string `json:"token,omitempty" env:"KHUNQUANT_TOOLS_SKILLS_GITHUB_AUTH_TOKEN"`
	Proxy string `json:"proxy,omitempty" env:"KHUNQUANT_TOOLS_SKILLS_GITHUB_PROXY"`
}

type ClawHubRegistryConfig struct {
	Enabled         bool   `json:"enabled"           env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_ENABLED"`
	BaseURL         string `json:"base_url"          env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_BASE_URL"`
	AuthToken       string `json:"auth_token"        env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_AUTH_TOKEN"`
	SearchPath      string `json:"search_path"       env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_SEARCH_PATH"`
	SkillsPath      string `json:"skills_path"       env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_SKILLS_PATH"`
	DownloadPath    string `json:"download_path"     env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_DOWNLOAD_PATH"`
	Timeout         int    `json:"timeout"           env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_TIMEOUT"`
	MaxZipSize      int    `json:"max_zip_size"      env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_MAX_ZIP_SIZE"`
	MaxResponseSize int    `json:"max_response_size" env:"KHUNQUANT_SKILLS_REGISTRIES_CLAWHUB_MAX_RESPONSE_SIZE"`
}

// MCPServerConfig defines configuration for a single MCP server
type MCPServerConfig struct {
	// Enabled indicates whether this MCP server is active
	Enabled bool `json:"enabled"`
	// Command is the executable to run (e.g., "npx", "python", "/path/to/server")
	Command string `json:"command"`
	// Args are the arguments to pass to the command
	Args []string `json:"args,omitempty"`
	// Env are environment variables to set for the server process (stdio only)
	Env map[string]string `json:"env,omitempty"`
	// EnvFile is the path to a file containing environment variables (stdio only)
	EnvFile string `json:"env_file,omitempty"`
	// Type is "stdio", "sse", or "http" (default: stdio if command is set, sse if url is set)
	Type string `json:"type,omitempty"`
	// URL is used for SSE/HTTP transport
	URL string `json:"url,omitempty"`
	// Headers are HTTP headers to send with requests (sse/http only)
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPConfig defines configuration for all MCP servers
type MCPConfig struct {
	ToolConfig `                    envPrefix:"KHUNQUANT_TOOLS_MCP_"`
	Discovery  ToolDiscoveryConfig `                                json:"discovery"`
	// Servers is a map of server name to server configuration
	Servers map[string]MCPServerConfig `json:"servers,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	// Pre-scan the JSON to check how many model_list entries the user provided.
	// Go's JSON decoder reuses existing slice backing-array elements rather than
	// zero-initializing them, so fields absent from the user's JSON (e.g. api_base)
	// would silently inherit values from the DefaultConfig template at the same
	// index position. We only reset cfg.ModelList when the user actually provides
	// entries; when count is 0 we keep DefaultConfig's built-in list as fallback.
	var tmp Config
	if err := json.Unmarshal(data, &tmp); err != nil {
		return nil, err
	}
	if len(tmp.ModelList) > 0 {
		cfg.ModelList = nil
	}

	// Set up credential resolver so SecureString.UnmarshalYAML can resolve
	// file:// and enc:// references relative to the config directory.
	updateResolver(path)

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Overlay sensitive fields from .security.yml (0o600) onto cfg.
	// This runs after JSON unmarshal so that [NOT_HERE] placeholders are
	// replaced with real values from the security file.
	if err := loadSecurityConfig(cfg, securityPath(path)); err != nil {
		return nil, err
	}

	if err := env.Parse(cfg); err != nil {
		return nil, err
	}

	// Migrate legacy channel config fields to new unified structures
	cfg.migrateChannelConfigs()

	// Auto-migrate: if only legacy providers config exists, convert to model_list
	if len(cfg.ModelList) == 0 && cfg.HasProvidersConfig() {
		cfg.ModelList = ConvertProvidersToModelList(cfg)
	}

	// Inherit credentials from providers to model_list entries (#1635).
	// When both providers and model_list are present, model_list entries
	// whose api_key/api_base are empty will inherit from the matching
	// provider (matched by protocol prefix).  Explicit model_list values
	// always take precedence.
	if cfg.HasProvidersConfig() {
		InheritProviderCredentials(cfg.ModelList, cfg.Providers)
	}

	// Validate model_list for uniqueness and required fields
	if err := cfg.ValidateModelList(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) migrateChannelConfigs() {
	// Discord: mention_only -> group_trigger.mention_only
	if c.Channels.Discord.MentionOnly && !c.Channels.Discord.GroupTrigger.MentionOnly {
		c.Channels.Discord.GroupTrigger.MentionOnly = true
	}

	// OneBot: group_trigger_prefix -> group_trigger.prefixes
	if len(c.Channels.OneBot.GroupTriggerPrefix) > 0 &&
		len(c.Channels.OneBot.GroupTrigger.Prefixes) == 0 {
		c.Channels.OneBot.GroupTrigger.Prefixes = c.Channels.OneBot.GroupTriggerPrefix
	}
}

func SaveConfig(path string, cfg *Config) error {
	// Save sensitive fields (SecureString values) to .security.yml before
	// marshaling config.json, so JSON only ever writes "[NOT_HERE]".
	if err := saveSecurityConfig(securityPath(path), cfg); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	// Use unified atomic write utility with explicit sync for flash storage reliability.
	return fileutil.WriteFileAtomic(path, data, 0o600)
}

func (c *Config) WorkspacePath() string {
	return expandHome(c.Agents.Defaults.Workspace)
}

func (c *Config) GetAPIKey() string {
	if c.Providers.OpenRouter.APIKey != "" {
		return c.Providers.OpenRouter.APIKey
	}
	if c.Providers.Anthropic.APIKey != "" {
		return c.Providers.Anthropic.APIKey
	}
	if c.Providers.OpenAI.APIKey != "" {
		return c.Providers.OpenAI.APIKey
	}
	if c.Providers.Gemini.APIKey != "" {
		return c.Providers.Gemini.APIKey
	}
	if c.Providers.Zhipu.APIKey != "" {
		return c.Providers.Zhipu.APIKey
	}
	if c.Providers.Groq.APIKey != "" {
		return c.Providers.Groq.APIKey
	}
	if c.Providers.VLLM.APIKey != "" {
		return c.Providers.VLLM.APIKey
	}
	if c.Providers.LlamaCpp.APIKey != "" {
		return c.Providers.LlamaCpp.APIKey
	}
	if c.Providers.ShengSuanYun.APIKey != "" {
		return c.Providers.ShengSuanYun.APIKey
	}
	if c.Providers.Cerebras.APIKey != "" {
		return c.Providers.Cerebras.APIKey
	}
	return ""
}

func (c *Config) GetAPIBase() string {
	if c.Providers.OpenRouter.APIKey != "" {
		if c.Providers.OpenRouter.APIBase != "" {
			return c.Providers.OpenRouter.APIBase
		}
		return "https://openrouter.ai/api/v1"
	}
	if c.Providers.Zhipu.APIKey != "" {
		return c.Providers.Zhipu.APIBase
	}
	if c.Providers.VLLM.APIKey != "" && c.Providers.VLLM.APIBase != "" {
		return c.Providers.VLLM.APIBase
	}
	if c.Providers.LlamaCpp.APIKey != "" && c.Providers.LlamaCpp.APIBase != "" {
		return c.Providers.LlamaCpp.APIBase
	}
	return ""
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) > 1 && path[1] == '/' {
			return home + path[1:]
		}
		return home
	}
	return path
}

// GetModelConfig returns the ModelConfig for the given model name.
// If multiple configs exist with the same model_name, it uses round-robin
// selection for load balancing. Returns an error if the model is not found.
func (c *Config) GetModelConfig(modelName string) (*ModelConfig, error) {
	matches := c.findMatches(modelName)
	if len(matches) == 0 {
		return nil, fmt.Errorf("model %q not found in model_list or providers", modelName)
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple configs - use round-robin for load balancing
	idx := (rrCounter.Add(1) - 1) % uint64(len(matches))
	return &matches[idx], nil
}

// findMatches finds all ModelConfig entries with the given model_name.
func (c *Config) findMatches(modelName string) []ModelConfig {
	var matches []ModelConfig
	for i := range c.ModelList {
		if c.ModelList[i].ModelName == modelName {
			matches = append(matches, c.ModelList[i])
		}
	}
	return matches
}

// HasProvidersConfig checks if any provider in the old providers config has configuration.
func (c *Config) HasProvidersConfig() bool {
	return !c.Providers.IsEmpty()
}

// ValidateModelList validates all ModelConfig entries in the model_list.
// It checks that each model config is valid.
// Note: Multiple entries with the same model_name are allowed for load balancing.
func (c *Config) ValidateModelList() error {
	for i := range c.ModelList {
		if err := c.ModelList[i].Validate(); err != nil {
			return fmt.Errorf("model_list[%d]: %w", i, err)
		}
	}
	return nil
}

func MergeAPIKeys(apiKey string, apiKeys []string) []string {
	seen := make(map[string]struct{})
	var all []string

	if k := strings.TrimSpace(apiKey); k != "" {
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			all = append(all, k)
		}
	}

	for _, k := range apiKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			if _, exists := seen[trimmed]; !exists {
				seen[trimmed] = struct{}{}
				all = append(all, trimmed)
			}
		}
	}

	return all
}

func (t *ToolsConfig) IsToolEnabled(name string) bool {
	switch name {
	case "web":
		return t.Web.Enabled
	case "cron":
		return t.Cron.Enabled
	case "exec":
		return t.Exec.Enabled
	case "skills":
		return t.Skills.Enabled
	case "media_cleanup":
		return t.MediaCleanup.Enabled
	case "append_file":
		return t.AppendFile.Enabled
	case "edit_file":
		return t.EditFile.Enabled
	case "find_skills":
		return t.FindSkills.Enabled
	case "i2c":
		return t.I2C.Enabled
	case "install_skill":
		return t.InstallSkill.Enabled
	case "list_dir":
		return t.ListDir.Enabled
	case "message":
		return t.Message.Enabled
	case "read_file":
		return t.ReadFile.Enabled
	case "spawn":
		return t.Spawn.Enabled
	case "spi":
		return t.SPI.Enabled
	case "subagent":
		return t.Subagent.Enabled
	case "web_fetch":
		return t.WebFetch.Enabled
	case "send_file":
		return t.SendFile.Enabled
	case "write_file":
		return t.WriteFile.Enabled
	case "mcp":
		return t.MCP.Enabled
	case "get_assets_list":
		return t.GetAssetsList.Enabled
	case "get_total_value":
		return t.GetTotalValue.Enabled
	case "list_portfolios":
		return t.ListPortfolios.Enabled
	case "take_snapshot":
		return t.TakeSnapshot.Enabled
	case "query_snapshots":
		return t.QuerySnapshots.Enabled
	case "snapshot_summary":
		return t.SnapshotSummary.Enabled
	case "delete_snapshots":
		return t.DeleteSnapshots.Enabled
	case "get_ticker":
		return t.GetTicker.Enabled
	case "get_tickers":
		return t.GetTickers.Enabled
	case "get_ohlcv":
		return t.GetOHLCV.Enabled
	case "get_orderbook":
		return t.GetOrderBook.Enabled
	case "get_markets":
		return t.GetMarkets.Enabled
	case "create_order":
		return t.CreateOrder.Enabled
	case "cancel_order":
		return t.CancelOrder.Enabled
	case "get_order":
		return t.GetOrder.Enabled
	case "get_open_orders":
		return t.GetOpenOrders.Enabled
	case "get_order_history":
		return t.GetOrderHistory.Enabled
	case "get_trade_history":
		return t.GetTradeHistory.Enabled
	case "emergency_stop":
		return t.EmergencyStop.Enabled
	case "paper_trade":
		return t.PaperTrade.Enabled
	case "get_order_rate_status":
		return t.GetOrderRateStatus.Enabled
	case "calculate_indicators":
		return t.CalculateIndicators.Enabled
	case "market_analysis":
		return t.MarketAnalysis.Enabled
	case "portfolio_allocation":
		return t.PortfolioAllocation.Enabled
	case "set_price_alert":
		return t.SetPriceAlert.Enabled
	case "set_indicator_alert":
		return t.SetIndicatorAlert.Enabled
	case "transfer_funds":
		return t.TransferFunds.Enabled
	case "config_encrypt_keys":
		return t.ConfigEncryptKeys.Enabled
	case "create_dca_plan":
		return t.CreateDCAPlan.Enabled
	case "list_dca_plans":
		return t.ListDCAPlans.Enabled
	case "update_dca_plan":
		return t.UpdateDCAPlan.Enabled
	case "delete_dca_plan":
		return t.DeleteDCAPlan.Enabled
	case "execute_dca_order":
		return t.ExecuteDCAOrder.Enabled
	case "get_dca_history":
		return t.GetDCAHistory.Enabled
	case "get_dca_summary":
		return t.GetDCASummary.Enabled
	case "get_pnl_summary":
		return t.GetPnLSummary.Enabled
	case "get_pnl_detail":
		return t.GetPnLDetail.Enabled
	default:
		return true
	}
}
