package media

import (
	"os"
	"path/filepath"
)

const TempDirName = "khunquant_media"

// TempDir returns the shared temporary directory used for downloaded media.
func TempDir() string {
	return filepath.Join(os.TempDir(), TempDirName)
}
