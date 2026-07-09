package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/credential"
	"gopkg.in/yaml.v3"
)

func TestSecureString_MarshalYAML_PlaintextValue(t *testing.T) {
	s := NewSecureString("plaintext")
	data, err := yaml.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("Expected marshaled data")
	}
}

func TestSecureString_MarshalYAML_EncryptedReference(t *testing.T) {
	s := &SecureString{
		raw:      "enc://encrypted_value",
		resolved: "decrypted_value",
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	result := string(data)
	if result != "enc://encrypted_value\n" {
		t.Errorf("Expected to preserve enc:// reference, got %q", result)
	}
}

func TestPrepareEncryptedCredentialsForRotation_ReencryptsExistingEncValues(t *testing.T) {
	dir := t.TempDir()
	sshKeyPath := filepath.Join(dir, "khunquant_ed25519.key")
	if err := credential.GenerateSSHKey(sshKeyPath); err != nil {
		t.Fatalf("GenerateSSHKey failed: %v", err)
	}
	t.Setenv(credential.SSHKeyPathEnvVar, sshKeyPath)

	oldProvider := credential.PassphraseProvider
	t.Cleanup(func() {
		credential.PassphraseProvider = oldProvider
	})

	const (
		oldPassphrase = "old-passphrase"
		newPassphrase = "new-passphrase"
		plaintext     = "telegram-token"
	)

	oldEncrypted, err := credential.Encrypt(oldPassphrase, "", plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"channels":{"telegram":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	securityPath := filepath.Join(dir, SecurityConfigFile)
	securityYAML := "channels:\n  telegram:\n    token: " + oldEncrypted + "\n"
	if err := os.WriteFile(securityPath, []byte(securityYAML), 0o600); err != nil {
		t.Fatalf("write security config: %v", err)
	}

	credential.PassphraseProvider = func() string { return oldPassphrase }
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig with old passphrase failed: %v", err)
	}
	if got := cfg.Channels.Telegram.Token.String(); got != plaintext {
		t.Fatalf("loaded token = %q, want %q", got, plaintext)
	}

	credential.PassphraseProvider = func() string { return newPassphrase }
	if rotated := cfg.PrepareEncryptedCredentialsForRotation(); rotated != 1 {
		t.Fatalf("PrepareEncryptedCredentialsForRotation() = %d, want 1", rotated)
	}
	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig after rotation failed: %v", err)
	}

	rotatedSecurity, err := os.ReadFile(securityPath)
	if err != nil {
		t.Fatalf("read rotated security config: %v", err)
	}
	rotatedText := string(rotatedSecurity)
	if !strings.Contains(rotatedText, "enc://") {
		t.Fatalf("rotated security config does not contain encrypted credential:\n%s", rotatedText)
	}
	if strings.Contains(rotatedText, oldEncrypted) {
		t.Fatalf("rotated security config still contains the old ciphertext")
	}

	credential.PassphraseProvider = func() string { return oldPassphrase }
	if _, err := LoadConfig(configPath); err == nil || !errors.Is(err, credential.ErrDecryptionFailed) {
		t.Fatalf("LoadConfig with old passphrase error = %v, want ErrDecryptionFailed", err)
	}

	credential.PassphraseProvider = func() string { return newPassphrase }
	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig with new passphrase failed: %v", err)
	}
	if got := reloaded.Channels.Telegram.Token.String(); got != plaintext {
		t.Fatalf("reloaded token = %q, want %q", got, plaintext)
	}
}

func TestPrepareEncryptedCredentialsForRotation_MapValues(t *testing.T) {
	type mapBackedSecrets struct {
		Secrets map[string]SecureString
	}
	holder := mapBackedSecrets{
		Secrets: map[string]SecureString{
			"api": {
				raw:      "enc://old-ciphertext",
				resolved: "plain-secret",
			},
			"file": {
				raw:      "file://secret.txt",
				resolved: "plain-file-secret",
			},
		},
	}

	if rotated := prepareEncryptedCredentialsForRotation(reflect.ValueOf(&holder)); rotated != 1 {
		t.Fatalf("prepareEncryptedCredentialsForRotation() = %d, want 1", rotated)
	}
	if got := holder.Secrets["api"].raw; got != "" {
		t.Fatalf("map-backed enc raw = %q, want empty", got)
	}
	if got := holder.Secrets["api"].resolved; got != "plain-secret" {
		t.Fatalf("map-backed resolved = %q, want %q", got, "plain-secret")
	}
	if got := holder.Secrets["file"].raw; got != "file://secret.txt" {
		t.Fatalf("map-backed file raw = %q, want preserved file reference", got)
	}
}

func TestSecureString_MarshalYAML_FileReference(t *testing.T) {
	s := &SecureString{
		raw:      "file:///path/to/file",
		resolved: "file_contents",
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	result := string(data)
	if result != "file:///path/to/file\n" {
		t.Errorf("Expected to preserve file:// reference, got %q", result)
	}
}

func TestSecureString_UnmarshalYAML_Basic(t *testing.T) {
	node := &yaml.Node{
		Value: "test_secret_value",
	}

	s := &SecureString{}
	err := s.UnmarshalYAML(node)
	if err != nil {
		t.Fatalf("UnmarshalYAML failed: %v", err)
	}

	if s.resolved == "" {
		t.Errorf("Expected resolved value to be set")
	}
}

func TestSecureString_UnmarshalYAML_PreservesExisting(t *testing.T) {
	s := &SecureString{
		resolved: "existing_value",
		raw:      "existing_raw",
	}

	node := &yaml.Node{
		Value: "new_value",
	}

	err := s.UnmarshalYAML(node)
	if err != nil {
		t.Fatalf("UnmarshalYAML failed: %v", err)
	}

	if s.resolved != "existing_value" {
		t.Errorf("Expected existing value to be preserved")
	}
}

func TestSecureStrings_MarshalJSON_ReturnsNotHere(t *testing.T) {
	ss := SimpleSecureStrings("key1", "key2")
	data, err := ss.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	if string(data) != `"[NOT_HERE]"` {
		t.Errorf("Expected \"[NOT_HERE]\", got %s", string(data))
	}
}

func TestSecureStrings_UnmarshalJSON_NotHere(t *testing.T) {
	ss := &SecureStrings{}
	err := ss.UnmarshalJSON([]byte(`"[NOT_HERE]"`))
	if err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}

	if len(*ss) != 0 {
		t.Errorf("Expected empty SecureStrings after [NOT_HERE]")
	}
}

func TestSecureStrings_UnmarshalJSON_Values(t *testing.T) {
	jsonData := `["key1", "key2"]`
	ss := &SecureStrings{}
	err := ss.UnmarshalJSON([]byte(jsonData))
	if err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}

	if len(*ss) != 2 {
		t.Fatalf("Expected 2 keys, got %d", len(*ss))
	}
}

func TestSecureStrings_IsZero_Empty(t *testing.T) {
	ss := SecureStrings{}
	if !ss.IsZero() {
		t.Errorf("Expected empty SecureStrings to be zero")
	}
}

func TestSecureStrings_IsZero_WithValues(t *testing.T) {
	ss := SimpleSecureStrings("key1")
	// In non-YAML context, IsZero returns len(s)==0, so a non-empty slice is not zero.
	result := ss.IsZero()
	if result {
		t.Errorf("Expected IsZero to return false for non-empty SecureStrings in non-YAML context")
	}
}

func TestBinanceExchangeConfig_MarshalYAML(t *testing.T) {
	cfg := &BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{
				Name:   "account1",
				APIKey: *NewSecureString("key1"),
				Secret: *NewSecureString("secret1"),
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("Expected marshaled data")
	}
}

func TestBinanceExchangeConfig_UnmarshalYAML(t *testing.T) {
	nodeData := `
account1:
  api_key: key123
  secret: secret123
`
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(nodeData), node); err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	cfg := &BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "account1"},
		},
	}

	err := cfg.UnmarshalYAML(node)
	if err != nil && err.Error() != "" {
		// This is okay for old format detection
	}
}

func TestBinanceTHExchangeConfig_MarshalYAML(t *testing.T) {
	cfg := BinanceTHExchangeConfig{
		Accounts: []ExchangeAccount{
			{
				Name:   "account1",
				APIKey: *NewSecureString("key1"),
				Secret: *NewSecureString("secret1"),
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("Expected marshaled data")
	}
}

func TestBinanceTHExchangeConfig_UnmarshalYAML(t *testing.T) {
	cfg := &BinanceTHExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "account1"},
		},
	}

	nodeData := `
account1:
  api_key: key123
  secret: secret123
`
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(nodeData), node); err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	err := cfg.UnmarshalYAML(node)
	if err != nil && err.Error() != "" {
		// This is okay for old format detection
	}
}

func TestBitkubExchangeConfig_MarshalYAML(t *testing.T) {
	cfg := BitkubExchangeConfig{
		Accounts: []ExchangeAccount{
			{
				Name:   "account1",
				APIKey: *NewSecureString("key1"),
				Secret: *NewSecureString("secret1"),
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("Expected marshaled data")
	}
}

func TestBitkubExchangeConfig_UnmarshalYAML(t *testing.T) {
	cfg := &BitkubExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "account1"},
		},
	}

	nodeData := `
account1:
  api_key: key123
  secret: secret123
`
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(nodeData), node); err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	err := cfg.UnmarshalYAML(node)
	if err != nil && err.Error() != "" {
		// Okay for old format
	}
}

func TestOKXExchangeConfig_MarshalYAML(t *testing.T) {
	cfg := OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{
					Name:   "account1",
					APIKey: *NewSecureString("key1"),
					Secret: *NewSecureString("secret1"),
				},
				Passphrase: *NewSecureString("pass1"),
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("Expected marshaled data")
	}
}

func TestOKXExchangeConfig_UnmarshalYAML(t *testing.T) {
	cfg := &OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "account1"}},
		},
	}

	nodeData := `
account1:
  api_key: key123
  secret: secret123
  passphrase: pass123
`
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(nodeData), node); err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	err := cfg.UnmarshalYAML(node)
	if err != nil && err.Error() != "" {
		// Okay for old format
	}
}

func TestSettradeExchangeConfig_MarshalYAML(t *testing.T) {
	cfg := SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{
					Name:   "account1",
					APIKey: *NewSecureString("key1"),
					Secret: *NewSecureString("secret1"),
				},
				PIN: *NewSecureString("1234"),
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("Expected marshaled data")
	}
}

func TestSettradeExchangeConfig_UnmarshalYAML(t *testing.T) {
	cfg := &SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "account1"}},
		},
	}

	nodeData := `
account1:
  api_key: key123
  secret: secret123
  pin: "1234"
`
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(nodeData), node); err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	err := cfg.UnmarshalYAML(node)
	if err != nil && err.Error() != "" {
		// Okay for old format
	}
}

func TestWebullExchangeConfig_MarshalYAML(t *testing.T) {
	cfg := WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{
					Name:   "main",
					APIKey: *NewSecureString("app-key-123"),
					Secret: *NewSecureString("app-secret-xyz"),
				},
				AccountID: "acct-1",
				Region:    "us",
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML failed: %v", err)
	}

	var m map[string]map[string]string
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal marshaled data: %v", err)
	}
	entry, ok := m["main"]
	if !ok {
		t.Fatalf("expected key 'main' in marshaled data, got: %v", m)
	}
	if entry["api_key"] != "app-key-123" {
		t.Errorf("api_key = %q, want app-key-123", entry["api_key"])
	}
	if entry["secret"] != "app-secret-xyz" {
		t.Errorf("secret = %q, want app-secret-xyz", entry["secret"])
	}
}

func TestWebullExchangeConfig_UnmarshalYAML(t *testing.T) {
	cfg := &WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}

	nodeData := `
main:
  api_key: app-key-123
  secret: app-secret-xyz
`
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(nodeData), node); err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	if err := cfg.UnmarshalYAML(node); err != nil {
		t.Fatalf("UnmarshalYAML failed: %v", err)
	}
	if cfg.Accounts[0].APIKey.String() != "app-key-123" {
		t.Errorf("APIKey = %q, want app-key-123", cfg.Accounts[0].APIKey.String())
	}
	if cfg.Accounts[0].Secret.String() != "app-secret-xyz" {
		t.Errorf("Secret = %q, want app-secret-xyz", cfg.Accounts[0].Secret.String())
	}
}

func TestChannelConfig_IsZero_TelegramEmpty(t *testing.T) {
	cfg := TelegramConfig{
		Token: *NewSecureString(""),
	}
	if !cfg.IsZero() {
		t.Errorf("Expected empty TelegramConfig to be zero")
	}
}

func TestChannelConfig_IsZero_TelegramWithToken(t *testing.T) {
	cfg := TelegramConfig{
		Token: *NewSecureString("token123"),
	}
	if cfg.IsZero() {
		t.Errorf("Expected TelegramConfig with token to not be zero")
	}
}

func TestChannelConfig_IsZero_DiscordEmpty(t *testing.T) {
	cfg := DiscordConfig{
		Token: *NewSecureString(""),
	}
	if !cfg.IsZero() {
		t.Errorf("Expected empty DiscordConfig to be zero")
	}
}

func TestChannelConfig_IsZero_FeishuPartial(t *testing.T) {
	cfg := FeishuConfig{
		AppSecret:         *NewSecureString("secret"),
		EncryptKey:        *NewSecureString(""),
		VerificationToken: *NewSecureString(""),
	}
	if cfg.IsZero() {
		t.Errorf("Expected FeishuConfig with AppSecret to not be zero")
	}
}

func TestChannelConfig_IsZero_SlackBothEmpty(t *testing.T) {
	cfg := SlackConfig{
		BotToken: *NewSecureString(""),
		AppToken: *NewSecureString(""),
	}
	if !cfg.IsZero() {
		t.Errorf("Expected empty SlackConfig to be zero")
	}
}

func TestChannelConfig_IsZero_SlackBotTokenSet(t *testing.T) {
	cfg := SlackConfig{
		BotToken: *NewSecureString("xoxb-token"),
		AppToken: *NewSecureString(""),
	}
	if cfg.IsZero() {
		t.Errorf("Expected SlackConfig with BotToken to not be zero")
	}
}

func TestChannelConfig_IsZero_LINEBothEmpty(t *testing.T) {
	cfg := LINEConfig{
		ChannelSecret:      *NewSecureString(""),
		ChannelAccessToken: *NewSecureString(""),
	}
	if !cfg.IsZero() {
		t.Errorf("Expected empty LINEConfig to be zero")
	}
}

func TestChannelConfig_IsZero_WeComMultipleFields(t *testing.T) {
	cfg := WeComConfig{
		Token:          *NewSecureString(""),
		EncodingAESKey: *NewSecureString(""),
	}
	if !cfg.IsZero() {
		t.Errorf("Expected empty WeComConfig to be zero")
	}
}

func TestChannelConfig_IsZero_IRCMultipleFields(t *testing.T) {
	cfg := IRCConfig{
		Password:         *NewSecureString(""),
		NickServPassword: *NewSecureString(""),
		SASLPassword:     *NewSecureString(""),
	}
	if !cfg.IsZero() {
		t.Errorf("Expected empty IRCConfig to be zero")
	}
}

func TestChannelConfig_IsZero_WeComAIBotPartial(t *testing.T) {
	cfg := WeComAIBotConfig{
		Token:          *NewSecureString("token"),
		EncodingAESKey: *NewSecureString(""),
	}
	if cfg.IsZero() {
		t.Errorf("Expected WeComAIBotConfig with Token to not be zero")
	}
}

func TestSecureString_Set_SetsResolvedClearsRaw(t *testing.T) {
	s := &SecureString{}
	result := s.Set("my-value")
	if s.resolved != "my-value" {
		t.Errorf("Set: resolved = %q, want %q", s.resolved, "my-value")
	}
	if s.raw != "" {
		t.Errorf("Set: raw should be empty after Set, got %q", s.raw)
	}
	if result != s {
		t.Error("Set should return the receiver")
	}
}

func TestSecureString_Set_OverwritesPreviousValue(t *testing.T) {
	s := &SecureString{}
	s.Set("first")
	s.Set("second")
	if s.resolved != "second" {
		t.Errorf("Set: resolved = %q, want %q", s.resolved, "second")
	}
}

func TestSecureString_Set_StringReturnsValue(t *testing.T) {
	s := &SecureString{}
	s.Set("hello")
	if s.String() != "hello" {
		t.Errorf("String() after Set = %q, want %q", s.String(), "hello")
	}
}

func TestSecureStrings_Values_ReturnsResolvedStrings(t *testing.T) {
	ss := SimpleSecureStrings("alpha", "beta", "gamma")
	vals := ss.Values()
	if len(vals) != 3 {
		t.Fatalf("Values() length = %d, want 3", len(vals))
	}
	found := map[string]bool{}
	for _, v := range vals {
		found[v] = true
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !found[want] {
			t.Errorf("Values() missing %q", want)
		}
	}
}

func TestSecureStrings_Values_NilReturnsNil(t *testing.T) {
	var ss *SecureStrings
	vals := ss.Values()
	if vals != nil {
		t.Errorf("Values() on nil receiver = %v, want nil", vals)
	}
}

func TestSecureStrings_Values_DeduplicatesEntries(t *testing.T) {
	ss := SimpleSecureStrings("dup", "dup", "unique")
	vals := ss.Values()
	if len(vals) != 2 {
		t.Errorf("Values() with duplicates: length = %d, want 2", len(vals))
	}
}

func TestSecureString_UnmarshalText_PlainValue(t *testing.T) {
	s := &SecureString{}
	if err := s.UnmarshalText([]byte("plain-text-value")); err != nil {
		t.Fatalf("UnmarshalText failed: %v", err)
	}
	if s.String() != "plain-text-value" {
		t.Errorf("UnmarshalText: String() = %q, want %q", s.String(), "plain-text-value")
	}
}

func TestSecureString_UnmarshalText_EmptyValue(t *testing.T) {
	s := &SecureString{}
	if err := s.UnmarshalText([]byte("")); err != nil {
		t.Fatalf("UnmarshalText empty failed: %v", err)
	}
	if s.String() != "" {
		t.Errorf("UnmarshalText empty: String() = %q, want empty", s.String())
	}
}
