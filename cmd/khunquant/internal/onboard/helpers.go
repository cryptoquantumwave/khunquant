package onboard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
)

func onboard() {
	configPath := internal.GetConfigPath()

	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Config already exists at %s\n", configPath)
		fmt.Print("Overwrite config with defaults? (y/n): ")
		var response string
		fmt.Scanln(&response)
		if response != "y" {
			fmt.Println("Aborted.")
			return
		}
	}

	encrypted := promptEncryptionSetup(configPath)

	cfg := config.DefaultConfig()

	if err := config.SaveConfig(configPath, cfg); err != nil {
		fmt.Printf("Error saving config: %v\n", err)
		os.Exit(1)
	}

	// Portfolio / exchange setup.
	enabledExchanges, err := setupPortfolios(cfg)
	if err != nil {
		fmt.Printf("Warning: portfolio setup failed: %v\n", err)
	} else if len(enabledExchanges) > 0 {
		if err := config.SaveConfig(configPath, cfg); err != nil {
			fmt.Printf("Error saving config after portfolio setup: %v\n", err)
			os.Exit(1)
		}
	}

	workspace := cfg.WorkspacePath()
	createWorkspaceTemplates(workspace)

	fmt.Printf("%s khunquant is ready!\n", internal.Logo)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Add your LLM API key to", configPath)
	fmt.Println("")
	fmt.Println("     Recommended LLM providers:")
	fmt.Println("     - OpenRouter: https://openrouter.ai/keys (access 100+ models)")
	fmt.Println("     - Ollama:     https://ollama.com (local, free)")
	fmt.Println("     - llama.cpp:  https://github.com/ggml-org/llama.cpp (local, lightweight)")
	fmt.Println("     - mlx_lm:    https://github.com/ml-explore/mlx-lm (local, Apple Silicon)")
	fmt.Println("")
	fmt.Println("     See README.md for 17+ supported providers.")
	fmt.Println("")

	if len(enabledExchanges) > 0 {
		fmt.Println("  2. Fill in exchange credentials in", configPath)
		fmt.Println("     Enabled exchanges (placeholder 'main' account added for each):")
		for _, ex := range enabledExchanges {
			switch ex {
			case "binance":
				fmt.Println("     - binance:   https://www.binance.com/en/my/settings/api-management")
			case "binanceth":
				fmt.Println("     - binanceth: https://www.binance.th/en/my/settings/api-management")
			case "bitkub":
				fmt.Println("     - bitkub:    https://www.bitkub.com/en/myaccount/api-key")
			case "okx":
				fmt.Println("     - okx:       https://www.okx.com/account/my-api")
			case "settrade":
				fmt.Println("     - settrade:  https://settrade.com/developer (app_code, broker_id, account_no)")
			}
		}
		fmt.Println("")
		fmt.Println("     Or run khunquant-launcher-tui to configure via the TUI.")
		fmt.Println("")
		if encrypted {
			fmt.Println("     Note: credentials stored via the TUI / auth subcommand will be")
			fmt.Println("     encrypted using the SSH key at ~/.ssh/khunquant_ed25519.key")
			fmt.Println("")
		}
	}

	fmt.Println("  3. Chat: khunquant agent -m \"Hello!\"")
}

// promptEncryptionSetup asks whether to encrypt credentials.
// On "Y" (default): prompts for passphrase, generates SSH key, persists the passphrase file.
// On "n": prints a warning and returns false.
func promptEncryptionSetup(configPath string) bool {
	fmt.Println("\nEncrypt stored credentials with a passphrase? (Y/n): ")
	var response string
	fmt.Scanln(&response)

	if response == "n" || response == "N" {
		fmt.Println()
		fmt.Printf("WARNING: Credentials will be stored in plaintext at %s/.security.yml\n", filepath.Dir(configPath))
		fmt.Println("         To encrypt later, run:  khunquant auth encrypt")
		fmt.Println()
		return false
	}

	fmt.Println("\nSet up credential encryption")
	fmt.Println("-----------------------------")
	passphrase, err := credential.PromptPassphrase()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	if err := credential.SetupSSHKey(); err != nil {
		fmt.Printf("Error generating SSH key: %v\n", err)
		os.Exit(1)
	}

	os.Setenv(credential.PassphraseEnvVar, passphrase)
	if err := credential.SavePassphraseFile(passphrase); err != nil {
		fmt.Printf("Warning: could not save passphrase file: %v\n", err)
	} else {
		fmt.Printf("Passphrase saved to %s\n", credential.PassphraseFilePath())
	}

	return true
}

func createWorkspaceTemplates(workspace string) {
	err := copyEmbeddedToTarget(workspace)
	if err != nil {
		fmt.Printf("Error copying workspace templates: %v\n", err)
	}
}

func copyEmbeddedToTarget(targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("Failed to create target directory: %w", err)
	}

	err := fs.WalkDir(embeddedFiles, "workspace", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		data, err := embeddedFiles.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Failed to read embedded file %s: %w", path, err)
		}

		newPath, err := filepath.Rel("workspace", path)
		if err != nil {
			return fmt.Errorf("Failed to get relative path for %s: %v\n", path, err)
		}

		targetPath := filepath.Join(targetDir, newPath)

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("Failed to create directory %s: %w", filepath.Dir(targetPath), err)
		}

		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return fmt.Errorf("Failed to write file %s: %w", targetPath, err)
		}

		return nil
	})

	return err
}
