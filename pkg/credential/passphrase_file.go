package credential

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
)

const passphraseFileName = ".passphrase"

// PassphraseFilePath returns the path to the persisted passphrase file.
// Priority: $KHUNQUANT_HOME/.passphrase > ~/.khunquant/.passphrase
func PassphraseFilePath() string {
	if home := os.Getenv(khunquantHome); home != "" {
		return filepath.Join(home, passphraseFileName)
	}
	userHome, _ := os.UserHomeDir()
	return filepath.Join(userHome, ".khunquant", passphraseFileName)
}

// SavePassphraseFile writes passphrase to the passphrase file with 0600 permissions.
func SavePassphraseFile(passphrase string) error {
	path := PassphraseFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(path, []byte(passphrase+"\n"), 0o600)
}

// LoadPassphraseFile reads the passphrase file and returns its trimmed content.
// Returns "" if the file does not exist or cannot be read.
func LoadPassphraseFile() string {
	data, err := os.ReadFile(PassphraseFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// InstallFileBackedProvider installs a PassphraseProvider that checks
// KHUNQUANT_KEY_PASSPHRASE first, then falls back to the passphrase file.
// Call this once at binary startup before any config is loaded.
func InstallFileBackedProvider() {
	PassphraseProvider = func() string {
		if v := os.Getenv(PassphraseEnvVar); v != "" {
			return v
		}
		return LoadPassphraseFile()
	}
}
