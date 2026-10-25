package credential

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
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
//
// H-14: the file holds the key that decrypts every stored credential, so it is
// refused - with a warning naming the fix - when group or others can read it,
// the way ssh refuses a private key. Permissions are checked on the file
// actually read (os.Stat follows a symlink). Windows has no equivalent mode
// bits, so the check is skipped there.
func LoadPassphraseFile() string {
	path := PassphraseFilePath()
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			return ""
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			logger.WarnCF("credential", "Ignoring passphrase file readable by other users",
				map[string]any{
					"path": path,
					"mode": fmt.Sprintf("%#o", perm),
					"fix":  "chmod 600 " + path,
				})
			return ""
		}
	}
	data, err := os.ReadFile(path)
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
