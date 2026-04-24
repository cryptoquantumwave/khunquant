package auth

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
)

func newEncryptCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "encrypt",
		Short: "Encrypt plaintext credentials in .security.yml with a passphrase",
		Long: `Prompts for a passphrase, generates an SSH key if one does not exist,
then re-saves .security.yml with all SecureString values encrypted as enc:// blobs.

The passphrase is persisted to $KHUNQUANT_HOME/.passphrase (0600) so future
khunquant invocations decrypt automatically. KHUNQUANT_KEY_PASSPHRASE env var
still takes precedence when set.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEncrypt()
		},
	}
}

func runEncrypt() error {
	configPath := internal.GetConfigPath()

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("cannot load config from %s: %w", configPath, err)
	}

	// Check if already encrypted by seeing whether PassphraseProvider returns a value
	// and the file has enc:// blobs. Simple heuristic: ask user to confirm rotation.
	if credential.PassphraseProvider() != "" {
		fmt.Println("Credentials appear to already be encrypted.")
		fmt.Print("Re-encrypt with a new passphrase? (y/n): ")
		var response string
		fmt.Scanln(&response)
		if response != "y" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	fmt.Println("\nSet up credential encryption")
	fmt.Println("-----------------------------")
	passphrase, err := credential.PromptPassphrase()
	if err != nil {
		return fmt.Errorf("passphrase: %w", err)
	}

	if err := credential.SetupSSHKey(); err != nil {
		return fmt.Errorf("SSH key setup: %w", err)
	}

	// Wire passphrase for this process so MarshalYAML encrypts on save.
	os.Setenv(credential.PassphraseEnvVar, passphrase)

	if err := credential.SavePassphraseFile(passphrase); err != nil {
		fmt.Printf("Warning: could not save passphrase file: %v\n", err)
	}

	// Re-save — SecureString.MarshalYAML sees a non-empty PassphraseProvider() and
	// encrypts every field automatically (pkg/config/config_struct.go:158-180).
	if err := config.SaveConfig(configPath, cfg); err != nil {
		return fmt.Errorf("saving encrypted config: %w", err)
	}

	passphraseFile := credential.PassphraseFilePath()
	fmt.Println("\nCredentials encrypted successfully.")
	fmt.Printf("Passphrase saved to %s\n", passphraseFile)
	fmt.Println("\nFuture khunquant invocations will decrypt automatically.")
	fmt.Printf("To override, set:  export %s=<passphrase>\n", credential.PassphraseEnvVar)

	return nil
}
