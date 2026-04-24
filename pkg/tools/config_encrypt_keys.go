package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
)

// ConfigEncryptKeysTool encrypts all SecureString credentials in .security.yml
// using AES-256-GCM with a user-supplied passphrase and an SSH private key.
//
// IMPORTANT: Always confirm with the user before calling this tool. Never log,
// echo, or include the passphrase in ForLLM output. The passphrase is a secret.
type ConfigEncryptKeysTool struct {
	cfg *config.Config
}

// NewConfigEncryptKeysTool creates the tool.
func NewConfigEncryptKeysTool(cfg *config.Config) *ConfigEncryptKeysTool {
	return &ConfigEncryptKeysTool{cfg: cfg}
}

func (t *ConfigEncryptKeysTool) Name() string {
	return NameConfigEncryptKeys
}

func (t *ConfigEncryptKeysTool) Description() string {
	return `Encrypt all exchange API keys and secrets in .security.yml using AES-256-GCM.

IMPORTANT SECURITY RULES:
- Always ask the user to provide a passphrase via a private channel or direct input.
- Never include the passphrase in your response text or in any tool output shown publicly.
- Confirm with the user before executing — this overwrites .security.yml.
- Remind the user to keep a backup of their passphrase; it cannot be recovered.

This tool:
1. Generates ~/.ssh/khunquant_ed25519.key if absent.
2. Encrypts every credential in .security.yml as enc:// blobs.
3. Persists the passphrase to $KHUNQUANT_HOME/.passphrase (0600) for future auto-decrypt.

After encryption, khunquant decrypts credentials automatically on startup.`
}

func (t *ConfigEncryptKeysTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"passphrase": map[string]any{
				"type":        "string",
				"description": "Passphrase for AES-256-GCM encryption. Must not be empty. Keep this secret — never echo it back to the user.",
			},
			"rotate": map[string]any{
				"type":        "boolean",
				"description": "If true, re-encrypts already-encrypted credentials with the new passphrase. Default false.",
			},
		},
		"required": []string{"passphrase"},
	}
}

func (t *ConfigEncryptKeysTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	passphrase, _ := args["passphrase"].(string)
	if passphrase == "" {
		return ErrorResult("passphrase must not be empty")
	}

	rotate, _ := args["rotate"].(bool)

	// Guard: if already encrypted and rotate not requested, refuse.
	if !rotate && credential.PassphraseProvider() != "" {
		return ErrorResult("credentials appear to be already encrypted; pass rotate=true to re-encrypt with a new passphrase")
	}

	// Ensure SSH key exists.
	sshKeyPath, err := credential.DefaultSSHKeyPath()
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot determine SSH key path: %v", err))
	}
	if _, err := os.Stat(sshKeyPath); os.IsNotExist(err) {
		if err := credential.GenerateSSHKey(sshKeyPath); err != nil {
			return ErrorResult(fmt.Sprintf("failed to generate SSH key: %v", err))
		}
	}

	// Wire passphrase for this process so MarshalYAML encrypts on save.
	os.Setenv(credential.PassphraseEnvVar, passphrase)

	if err := credential.SavePassphraseFile(passphrase); err != nil {
		return ErrorResult(fmt.Sprintf("failed to save passphrase file: %v", err))
	}

	configPath := resolveConfigPath()

	// Reload config so SecureString raw values are fresh, then re-save — MarshalYAML
	// auto-encrypts every field when PassphraseProvider() is non-empty.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to load config: %v", err))
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		return ErrorResult(fmt.Sprintf("failed to save encrypted config: %v", err))
	}

	passphraseFile := credential.PassphraseFilePath()
	summary := fmt.Sprintf(
		"Credentials encrypted successfully.\nPassphrase persisted to %s.\nFuture khunquant invocations will decrypt automatically.",
		passphraseFile,
	)

	return &ToolResult{
		ForUser: summary,
		ForLLM:  fmt.Sprintf(`{"status":"encrypted","passphrase_file":%q,"config_path":%q}`, passphraseFile, configPath),
	}
}

// resolveConfigPath returns the config.json path using the same priority as the CLI.
func resolveConfigPath() string {
	if p := os.Getenv("KHUNQUANT_CONFIG"); p != "" {
		return p
	}
	if home := os.Getenv("KHUNQUANT_HOME"); home != "" {
		return filepath.Join(home, "config.json")
	}
	userHome, _ := os.UserHomeDir()
	return filepath.Join(userHome, ".khunquant", "config.json")
}
