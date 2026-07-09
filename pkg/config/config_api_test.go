package config

import (
	"testing"
)

func TestGetAPIKey_Priority(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.OpenRouter.APIKey = "or-key"
	cfg.Providers.Anthropic.APIKey = "ant-key"

	if got := cfg.GetAPIKey(); got != "or-key" {
		t.Errorf("GetAPIKey() = %q, want OpenRouter first", got)
	}
}

func TestGetAPIKey_Anthropic(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.Anthropic.APIKey = "ant-key"
	cfg.Providers.OpenAI.APIKey = "oai-key"

	if got := cfg.GetAPIKey(); got != "ant-key" {
		t.Errorf("GetAPIKey() = %q, want Anthropic when OpenRouter empty", got)
	}
}

func TestGetAPIKey_OpenAI(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.OpenAI.APIKey = "oai-key"

	if got := cfg.GetAPIKey(); got != "oai-key" {
		t.Errorf("GetAPIKey() = %q, want oai-key", got)
	}
}

func TestGetAPIKey_Empty(t *testing.T) {
	cfg := &Config{}
	if got := cfg.GetAPIKey(); got != "" {
		t.Errorf("GetAPIKey() = %q, want empty string", got)
	}
}

func TestGetAPIKey_Gemini(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.Gemini.APIKey = "gem-key"
	if got := cfg.GetAPIKey(); got != "gem-key" {
		t.Errorf("GetAPIKey() = %q, want gem-key", got)
	}
}

func TestGetAPIBase_OpenRouter(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.OpenRouter.APIKey = "or-key"
	cfg.Providers.OpenRouter.APIBase = "https://custom.or.ai/v1"

	if got := cfg.GetAPIBase(); got != "https://custom.or.ai/v1" {
		t.Errorf("GetAPIBase() = %q, want custom base", got)
	}
}

func TestGetAPIBase_OpenRouterDefault(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.OpenRouter.APIKey = "or-key"

	got := cfg.GetAPIBase()
	if got != "https://openrouter.ai/api/v1" {
		t.Errorf("GetAPIBase() = %q, want default openrouter base", got)
	}
}

func TestGetAPIBase_Empty(t *testing.T) {
	cfg := &Config{}
	if got := cfg.GetAPIBase(); got != "" {
		t.Errorf("GetAPIBase() = %q, want empty", got)
	}
}

func TestGetAPIBase_VLLM(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.VLLM.APIKey = "vllm-key"
	cfg.Providers.VLLM.APIBase = "http://localhost:8000"

	if got := cfg.GetAPIBase(); got != "http://localhost:8000" {
		t.Errorf("GetAPIBase() = %q, want VLLM base", got)
	}
}

func TestWorkspacePath_Empty(t *testing.T) {
	cfg := &Config{}
	if got := cfg.WorkspacePath(); got != "" {
		t.Errorf("WorkspacePath() = %q, want empty for empty workspace", got)
	}
}

func TestWorkspacePath_Absolute(t *testing.T) {
	cfg := &Config{}
	cfg.Agents.Defaults.Workspace = "/tmp/workspace"
	if got := cfg.WorkspacePath(); got != "/tmp/workspace" {
		t.Errorf("WorkspacePath() = %q, want /tmp/workspace", got)
	}
}

func TestExpandHome_Tilde(t *testing.T) {
	result := expandHome("~/projects")
	if result == "~/projects" {
		t.Errorf("expandHome should expand ~ but got %q", result)
	}
	if len(result) == 0 {
		t.Error("expandHome returned empty string for ~/projects")
	}
}

func TestExpandHome_Empty(t *testing.T) {
	if got := expandHome(""); got != "" {
		t.Errorf("expandHome('') = %q, want ''", got)
	}
}

func TestExpandHome_TildeOnly(t *testing.T) {
	result := expandHome("~")
	if result == "~" {
		t.Errorf("expandHome('~') should expand to home dir, got %q", result)
	}
}

func TestExpandHome_Absolute(t *testing.T) {
	if got := expandHome("/absolute/path"); got != "/absolute/path" {
		t.Errorf("expandHome('/absolute/path') = %q, want unchanged", got)
	}
}

func TestMergeAPIKeys_Deduplication(t *testing.T) {
	result := MergeAPIKeys("key1", []string{"key1", "key2"})
	if len(result) != 2 {
		t.Errorf("MergeAPIKeys dedup: got %d keys, want 2", len(result))
	}
	if result[0] != "key1" || result[1] != "key2" {
		t.Errorf("MergeAPIKeys dedup: got %v", result)
	}
}

func TestMergeAPIKeys_EmptyPrimary(t *testing.T) {
	result := MergeAPIKeys("", []string{"key1", "key2"})
	if len(result) != 2 {
		t.Errorf("MergeAPIKeys empty primary: got %d keys, want 2", len(result))
	}
}

func TestMergeAPIKeys_EmptyAll(t *testing.T) {
	result := MergeAPIKeys("", []string{})
	if len(result) != 0 {
		t.Errorf("MergeAPIKeys all empty: got %d keys, want 0", len(result))
	}
}

func TestMergeAPIKeys_Trimming(t *testing.T) {
	result := MergeAPIKeys("  key1  ", []string{"  key2  "})
	if len(result) != 2 {
		t.Errorf("MergeAPIKeys trimming: got %d keys, want 2", len(result))
	}
	if result[0] != "key1" || result[1] != "key2" {
		t.Errorf("MergeAPIKeys trimming: got %v", result)
	}
}

func TestMergeAPIKeys_WhitespaceOnly(t *testing.T) {
	result := MergeAPIKeys("   ", []string{"  "})
	if len(result) != 0 {
		t.Errorf("MergeAPIKeys whitespace-only: got %d keys, want 0", len(result))
	}
}

func TestIsToolEnabled_Known(t *testing.T) {
	tc := &ToolsConfig{}
	tc.Web.Enabled = true
	tc.Cron.Enabled = false

	if !tc.IsToolEnabled("web") {
		t.Error("IsToolEnabled('web') should return true")
	}
	if tc.IsToolEnabled("cron") {
		t.Error("IsToolEnabled('cron') should return false")
	}
}

func TestIsToolEnabled_Unknown(t *testing.T) {
	tc := &ToolsConfig{}
	// unknown tool names fall through to default: true
	if !tc.IsToolEnabled("nonexistent_tool_xyz") {
		t.Error("IsToolEnabled unknown tool should return true (default case)")
	}
}

func TestIsToolEnabled_AllKnown(t *testing.T) {
	tc := &ToolsConfig{}
	tc.Exec.Enabled = true
	tc.Skills.Enabled = true
	tc.MediaCleanup.Enabled = true
	tc.AppendFile.Enabled = true
	tc.EditFile.Enabled = true

	for _, name := range []string{"exec", "skills", "media_cleanup", "append_file", "edit_file"} {
		if !tc.IsToolEnabled(name) {
			t.Errorf("IsToolEnabled(%q) should return true", name)
		}
	}
}

func TestHasPermission_EmptyPermissions(t *testing.T) {
	acc := ExchangeAccount{}
	if !acc.HasPermission(PermissionScope("read")) {
		t.Error("HasPermission with empty Permissions should return true (all allowed)")
	}
}

func TestHasPermission_Match(t *testing.T) {
	acc := ExchangeAccount{
		Permissions: []PermissionScope{"read", "trade"},
	}
	if !acc.HasPermission("trade") {
		t.Error("HasPermission should return true for matching scope")
	}
}

func TestHasPermission_NoMatch(t *testing.T) {
	acc := ExchangeAccount{
		Permissions: []PermissionScope{"read"},
	}
	if acc.HasPermission("trade") {
		t.Error("HasPermission should return false for non-matching scope")
	}
}

func TestRedactedAPIKey_Short(t *testing.T) {
	acc := ExchangeAccount{APIKey: *NewSecureString("abc")}
	if got := acc.RedactedAPIKey(); got != "***" {
		t.Errorf("RedactedAPIKey short key = %q, want ***", got)
	}
}

func TestRedactedAPIKey_Long(t *testing.T) {
	acc := ExchangeAccount{APIKey: *NewSecureString("sk-ant-1234567890abcdef")}
	got := acc.RedactedAPIKey()
	if len(got) == 0 {
		t.Fatal("RedactedAPIKey should not be empty")
	}
	if got[len(got)-4:] != "cdef" {
		t.Errorf("RedactedAPIKey last 4 chars = %q, want cdef", got[len(got)-4:])
	}
}

func TestRedactedAPIKey_ExactlyFour(t *testing.T) {
	acc := ExchangeAccount{APIKey: *NewSecureString("1234")}
	if got := acc.RedactedAPIKey(); got != "***" {
		t.Errorf("RedactedAPIKey 4-char key = %q, want ***", got)
	}
}

func TestResolveAccount_Binance_ByName(t *testing.T) {
	cfg := &BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "main", APIKey: *NewSecureString("k1")},
			{Name: "test", APIKey: *NewSecureString("k2")},
		},
	}
	acc, ok := cfg.ResolveAccount("test")
	if !ok || acc.Name != "test" {
		t.Errorf("ResolveAccount('test') = %v, %v", acc, ok)
	}
}

func TestResolveAccount_Binance_FirstWhenEmpty(t *testing.T) {
	cfg := &BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "first"},
			{Name: "second"},
		},
	}
	acc, ok := cfg.ResolveAccount("")
	if !ok || acc.Name != "first" {
		t.Errorf("ResolveAccount('') = %v, %v; want first", acc, ok)
	}
}

func TestResolveAccount_Binance_NotFound(t *testing.T) {
	cfg := &BinanceExchangeConfig{
		Accounts: []ExchangeAccount{{Name: "main"}},
	}
	_, ok := cfg.ResolveAccount("nonexistent")
	if ok {
		t.Error("ResolveAccount should return false for nonexistent name")
	}
}

func TestResolveAccount_Binance_AutoNameNumbered(t *testing.T) {
	cfg := &BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{APIKey: *NewSecureString("k1")}, // no name
		},
	}
	acc, ok := cfg.ResolveAccount("1")
	if !ok || acc.Name != "1" {
		t.Errorf("ResolveAccount('1') for unnamed account = %v, %v", acc, ok)
	}
}

func TestResolveAccount_OKX_ByName(t *testing.T) {
	cfg := &OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "trading"}},
		},
	}
	acc, ok := cfg.ResolveAccount("trading")
	if !ok || acc.Name != "trading" {
		t.Errorf("OKX ResolveAccount('trading') = %v, %v", acc, ok)
	}
}

func TestResolveAccount_OKX_CaseInsensitive(t *testing.T) {
	cfg := &OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "Trading"}},
		},
	}
	_, ok := cfg.ResolveAccount("trading")
	if !ok {
		t.Error("ResolveAccount should be case-insensitive")
	}
}

func TestResolveAccount_Settrade_ByName(t *testing.T) {
	cfg := &SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "default"}},
		},
	}
	acc, ok := cfg.ResolveAccount("default")
	if !ok || acc.Name != "default" {
		t.Errorf("Settrade ResolveAccount = %v, %v", acc, ok)
	}
}

func TestGetMaxMediaSize_Default(t *testing.T) {
	d := &AgentDefaults{}
	if got := d.GetMaxMediaSize(); got != DefaultMaxMediaSize {
		t.Errorf("GetMaxMediaSize() = %d, want %d", got, DefaultMaxMediaSize)
	}
}

func TestGetMaxMediaSize_Custom(t *testing.T) {
	d := &AgentDefaults{MaxMediaSize: 5 * 1024 * 1024}
	if got := d.GetMaxMediaSize(); got != 5*1024*1024 {
		t.Errorf("GetMaxMediaSize() = %d, want 5MB", got)
	}
}

func TestGetToolFeedbackMaxArgsLength_Default(t *testing.T) {
	d := &AgentDefaults{}
	if got := d.GetToolFeedbackMaxArgsLength(); got != 300 {
		t.Errorf("GetToolFeedbackMaxArgsLength() = %d, want 300", got)
	}
}

func TestGetToolFeedbackMaxArgsLength_Custom(t *testing.T) {
	d := &AgentDefaults{}
	d.ToolFeedback.MaxArgsLength = 500
	if got := d.GetToolFeedbackMaxArgsLength(); got != 500 {
		t.Errorf("GetToolFeedbackMaxArgsLength() = %d, want 500", got)
	}
}

func TestGetAPIKey_Zhipu(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.Zhipu.APIKey = "zhipu-key"
	if got := cfg.GetAPIKey(); got != "zhipu-key" {
		t.Errorf("GetAPIKey() = %q, want zhipu-key", got)
	}
}

func TestGetAPIKey_Groq(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.Groq.APIKey = "groq-key"
	if got := cfg.GetAPIKey(); got != "groq-key" {
		t.Errorf("GetAPIKey() = %q, want groq-key", got)
	}
}

func TestGetAPIKey_VLLM(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.VLLM.APIKey = "vllm-key"
	if got := cfg.GetAPIKey(); got != "vllm-key" {
		t.Errorf("GetAPIKey() = %q, want vllm-key", got)
	}
}

func TestGetAPIKey_LlamaCpp(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.LlamaCpp.APIKey = "llama-key"
	if got := cfg.GetAPIKey(); got != "llama-key" {
		t.Errorf("GetAPIKey() = %q, want llama-key", got)
	}
}

func TestGetAPIKey_ShengSuanYun(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.ShengSuanYun.APIKey = "ssy-key"
	if got := cfg.GetAPIKey(); got != "ssy-key" {
		t.Errorf("GetAPIKey() = %q, want ssy-key", got)
	}
}

func TestGetAPIKey_Cerebras(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.Cerebras.APIKey = "cerebras-key"
	if got := cfg.GetAPIKey(); got != "cerebras-key" {
		t.Errorf("GetAPIKey() = %q, want cerebras-key", got)
	}
}

func TestGetAPIBase_Zhipu(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.Zhipu.APIKey = "zhipu-key"
	cfg.Providers.Zhipu.APIBase = "https://open.bigmodel.cn/api/paas/v4"
	if got := cfg.GetAPIBase(); got != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("GetAPIBase() Zhipu = %q", got)
	}
}

func TestGetAPIBase_LlamaCpp(t *testing.T) {
	cfg := &Config{}
	cfg.Providers.LlamaCpp.APIKey = "llama-key"
	cfg.Providers.LlamaCpp.APIBase = "http://localhost:8080/v1"
	if got := cfg.GetAPIBase(); got != "http://localhost:8080/v1" {
		t.Errorf("GetAPIBase() LlamaCpp = %q", got)
	}
}

func TestGetFilterMinLength_Custom(t *testing.T) {
	tc := &ToolsConfig{FilterMinLength: 20}
	if got := tc.GetFilterMinLength(); got != 20 {
		t.Errorf("GetFilterMinLength() = %d, want 20", got)
	}
}

func TestMigrateChannelConfigs_Discord(t *testing.T) {
	cfg := &Config{}
	cfg.Channels.Discord.MentionOnly = true
	cfg.migrateChannelConfigs()
	if !cfg.Channels.Discord.GroupTrigger.MentionOnly {
		t.Error("migrateChannelConfigs should migrate Discord.MentionOnly to GroupTrigger.MentionOnly")
	}
}

func TestMigrateChannelConfigs_OneBot(t *testing.T) {
	cfg := &Config{}
	cfg.Channels.OneBot.GroupTriggerPrefix = []string{"!", "~"}
	cfg.migrateChannelConfigs()
	if len(cfg.Channels.OneBot.GroupTrigger.Prefixes) != 2 {
		t.Errorf("migrateChannelConfigs should migrate OneBot.GroupTriggerPrefix, got %v", cfg.Channels.OneBot.GroupTrigger.Prefixes)
	}
}

func TestResolveAccount_OKX_Positional(t *testing.T) {
	cfg := &OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{}, // no Name — positional "1"
		},
	}
	acc, ok := cfg.ResolveAccount("1")
	if !ok {
		t.Error("OKX ResolveAccount should resolve positional name '1'")
	}
	if acc.Name != "1" {
		t.Errorf("OKX positional account Name = %q, want '1'", acc.Name)
	}
}

func TestResolveAccount_Settrade_Positional(t *testing.T) {
	cfg := &SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{}, // no Name — positional "1"
		},
	}
	acc, ok := cfg.ResolveAccount("1")
	if !ok {
		t.Error("Settrade ResolveAccount should resolve positional name '1'")
	}
	if acc.Name != "1" {
		t.Errorf("Settrade positional account Name = %q, want '1'", acc.Name)
	}
}

func TestResolveAccount_Webull_ByName(t *testing.T) {
	cfg := &WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main"},
				AccountID:       "acct-1",
			},
		},
	}
	acc, ok := cfg.ResolveAccount("main")
	if !ok || acc.Name != "main" {
		t.Errorf("Webull ResolveAccount = %v, %v", acc, ok)
	}
}

func TestResolveAccount_Webull_EmptyName_ReturnsFirst(t *testing.T) {
	cfg := &WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}
	acc, ok := cfg.ResolveAccount("")
	if !ok || acc.Name != "main" {
		t.Errorf("Webull ResolveAccount(\"\") should return first account, got %v", acc)
	}
}

func TestResolveAccount_Webull_Positional(t *testing.T) {
	cfg := &WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{}, // no Name — positional "1"
		},
	}
	acc, ok := cfg.ResolveAccount("1")
	if !ok {
		t.Error("Webull ResolveAccount should resolve positional name '1'")
	}
	if acc.Name != "1" {
		t.Errorf("Webull positional account Name = %q, want '1'", acc.Name)
	}
}

func TestResolveAccount_Webull_NotFound(t *testing.T) {
	cfg := &WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}
	_, ok := cfg.ResolveAccount("nope")
	if ok {
		t.Error("Webull ResolveAccount should return false for non-existent account")
	}
}

func TestResolveAccount_OKX_EmptyName_ReturnsFirst(t *testing.T) {
	cfg := &OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}
	acc, ok := cfg.ResolveAccount("")
	if !ok || acc.Name != "main" {
		t.Errorf("OKX ResolveAccount('') = %v, %v", acc, ok)
	}
}
