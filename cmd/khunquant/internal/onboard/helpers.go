package onboard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
)

func onboard(encrypt bool) {
	configPath := internal.GetConfigPath()

	// useDefaults controls whether we start from config.DefaultConfig() or
	// load the existing config file. Starts true (fresh defaults); set to false
	// when config already exists and we don't need to overwrite.
	useDefaults := true

	if _, err := os.Stat(configPath); err == nil {
		// Config file already exists.
		if encrypt {
			sshKeyPath, _ := credential.DefaultSSHKeyPath()
			if _, err := os.Stat(sshKeyPath); err == nil {
				// Both config AND SSH key already exist — ask before clobbering defaults.
				fmt.Printf("Config already exists at %s\n", configPath)
				fmt.Print("Overwrite config with defaults? (y/n): ")
				var response string
				fmt.Scanln(&response)
				if response != "y" {
					fmt.Println("Aborted.")
					return
				}
				// useDefaults remains true — reset to defaults below.
			} else {
				// Config exists but no SSH key yet — load existing config and layer enc setup.
				useDefaults = false
			}
		} else {
			// No --enc: keep original fork behavior — prompt before overwriting.
			fmt.Printf("Config already exists at %s\n", configPath)
			fmt.Print("Overwrite? (y/n): ")
			var response string
			fmt.Scanln(&response)
			if response != "y" {
				fmt.Println("Aborted.")
				return
			}
			// useDefaults remains true.
		}
	}

	if encrypt {
		fmt.Println("\nSet up credential encryption")
		fmt.Println("-----------------------------")
		passphrase, err := promptPassphrase()
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		os.Setenv(credential.PassphraseEnvVar, passphrase)

		if err := setupSSHKey(); err != nil {
			fmt.Printf("Error generating SSH key: %v\n", err)
			os.Exit(1)
		}
	}

	var cfg *config.Config
	if useDefaults {
		cfg = config.DefaultConfig()
	} else {
		var err error
		cfg, err = config.LoadConfig(configPath)
		if err != nil {
			fmt.Printf("Error loading existing config: %v\n", err)
			os.Exit(1)
		}
	}

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
		if encrypt {
			fmt.Println("     Note: credentials stored via the TUI / auth subcommand will be")
			fmt.Println("     encrypted using the SSH key at ~/.ssh/khunquant_ed25519.key")
			fmt.Println("")
		}
	}

	fmt.Println("  3. Chat: khunquant agent -m \"Hello!\"")
}

func promptPassphrase() (string, error) {
	fmt.Print("Enter passphrase for credential encryption: ")
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading passphrase: %w", err)
	}
	if len(p1) == 0 {
		return "", fmt.Errorf("passphrase must not be empty")
	}

	fmt.Print("Confirm passphrase: ")
	p2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading passphrase confirmation: %w", err)
	}

	if string(p1) != string(p2) {
		return "", fmt.Errorf("passphrases do not match")
	}
	return string(p1), nil
}

func setupSSHKey() error {
	keyPath, err := credential.DefaultSSHKeyPath()
	if err != nil {
		return fmt.Errorf("cannot determine SSH key path: %w", err)
	}

	if _, err := os.Stat(keyPath); err == nil {
		fmt.Printf("\nWARNING: %s already exists.\n", keyPath)
		fmt.Println("    Overwriting will invalidate any credentials previously encrypted with this key.")
		fmt.Print("    Overwrite? (y/n): ")
		var response string
		fmt.Scanln(&response)
		if response != "y" {
			fmt.Println("Keeping existing SSH key.")
			return nil
		}
	}

	if err := credential.GenerateSSHKey(keyPath); err != nil {
		return err
	}
	fmt.Printf("SSH key generated: %s\n", keyPath)
	return nil
}

func createWorkspaceTemplates(workspace string) {
	err := copyEmbeddedToTarget(workspace)
	if err != nil {
		fmt.Printf("Error copying workspace templates: %v\n", err)
	}
}

func copyEmbeddedToTarget(targetDir string) error {
	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("Failed to create target directory: %w", err)
	}

	// Walk through all files in embed.FS
	err := fs.WalkDir(embeddedFiles, "workspace", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if d.IsDir() {
			return nil
		}

		// Read embedded file
		data, err := embeddedFiles.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Failed to read embedded file %s: %w", path, err)
		}

		new_path, err := filepath.Rel("workspace", path)
		if err != nil {
			return fmt.Errorf("Failed to get relative path for %s: %v\n", path, err)
		}

		// Build target file path
		targetPath := filepath.Join(targetDir, new_path)

		// Ensure target file's directory exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("Failed to create directory %s: %w", filepath.Dir(targetPath), err)
		}

		// Write file
		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return fmt.Errorf("Failed to write file %s: %w", targetPath, err)
		}

		return nil
	})

	return err
}
