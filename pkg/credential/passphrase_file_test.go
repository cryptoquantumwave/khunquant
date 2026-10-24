package credential

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPassphraseFilePath_DefaultHome tests PassphraseFilePath with default home.
func TestPassphraseFilePath_DefaultHome(t *testing.T) {
	t.Setenv(khunquantHome, "")

	path := PassphraseFilePath()
	if path == "" {
		t.Fatal("PassphraseFilePath should not return empty")
	}

	// Should contain .khunquant and .passphrase
	if !strings.Contains(path, ".khunquant") {
		t.Errorf("path should contain '.khunquant': %q", path)
	}
	if !strings.Contains(path, passphraseFileName) {
		t.Errorf("path should contain '%s': %q", passphraseFileName, path)
	}
}

// TestPassphraseFilePath_WithKHUNQUANT_HOME tests PassphraseFilePath respects KHUNQUANT_HOME.
func TestPassphraseFilePath_WithKHUNQUANT_HOME(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	path := PassphraseFilePath()
	expected := filepath.Join(customHome, passphraseFileName)
	if path != expected {
		t.Errorf("PassphraseFilePath() = %q, want %q", path, expected)
	}
}

// TestSavePassphraseFile_CreatesFile tests SavePassphraseFile creates the file.
func TestSavePassphraseFile_CreatesFile(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	passphrase := "test-passphrase"
	err := SavePassphraseFile(passphrase)
	if err != nil {
		t.Fatalf("SavePassphraseFile: %v", err)
	}

	// Verify file was created
	path := PassphraseFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Content should be passphrase + newline
	expected := passphrase + "\n"
	if string(data) != expected {
		t.Errorf("file content = %q, want %q", string(data), expected)
	}
}

// TestSavePassphraseFile_Permissions tests SavePassphraseFile uses 0600 permissions.
func TestSavePassphraseFile_Permissions(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	err := SavePassphraseFile("secret")
	if err != nil {
		t.Fatalf("SavePassphraseFile: %v", err)
	}

	path := PassphraseFilePath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Check permissions are 0600
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file permissions = %o, want 0o600", info.Mode().Perm())
	}
}

// TestSavePassphraseFile_CreatesDirectory tests SavePassphraseFile creates parent directory.
func TestSavePassphraseFile_CreatesDirectory(t *testing.T) {
	customHome := t.TempDir()
	nestedPath := filepath.Join(customHome, "nested", "deep", "home")
	t.Setenv(khunquantHome, nestedPath)

	err := SavePassphraseFile("test")
	if err != nil {
		t.Fatalf("SavePassphraseFile: %v", err)
	}

	path := PassphraseFilePath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist: %v", err)
	}

	// Check parent directory exists
	parentPath := filepath.Dir(path)
	info, err := os.Stat(parentPath)
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if !info.IsDir() {
		t.Error("parent should be a directory")
	}
	// Check directory permissions are 0700
	if info.Mode().Perm() != 0o700 {
		t.Errorf("directory permissions = %o, want 0o700", info.Mode().Perm())
	}
}

// TestSavePassphraseFile_Overwrites tests SavePassphraseFile overwrites existing file.
func TestSavePassphraseFile_Overwrites(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Save first passphrase
	err := SavePassphraseFile("first-pass")
	if err != nil {
		t.Fatalf("SavePassphraseFile (first): %v", err)
	}

	// Overwrite with second passphrase
	err = SavePassphraseFile("second-pass")
	if err != nil {
		t.Fatalf("SavePassphraseFile (second): %v", err)
	}

	// Verify content is updated
	path := PassphraseFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	expected := "second-pass\n"
	if string(data) != expected {
		t.Errorf("file content = %q, want %q", string(data), expected)
	}
}

// TestLoadPassphraseFile_Missing returns empty string.
func TestLoadPassphraseFile_Missing(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Don't create the file
	result := LoadPassphraseFile()
	if result != "" {
		t.Errorf("LoadPassphraseFile for missing file = %q, want empty", result)
	}
}

// TestLoadPassphraseFile_Reads tests LoadPassphraseFile reads file correctly.
func TestLoadPassphraseFile_Reads(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	passphrase := "saved-passphrase"
	err := SavePassphraseFile(passphrase)
	if err != nil {
		t.Fatalf("SavePassphraseFile: %v", err)
	}

	result := LoadPassphraseFile()
	if result != passphrase {
		t.Errorf("LoadPassphraseFile() = %q, want %q", result, passphrase)
	}
}

// TestLoadPassphraseFile_Trims tests LoadPassphraseFile trims whitespace.
func TestLoadPassphraseFile_Trims(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Manually write file with extra whitespace
	path := PassphraseFilePath()
	dirPath := filepath.Dir(path)
	os.MkdirAll(dirPath, 0o700)
	os.WriteFile(path, []byte("  \n  test-pass  \n  \n"), 0o600)

	result := LoadPassphraseFile()
	if result != "test-pass" {
		t.Errorf("LoadPassphraseFile() = %q, want 'test-pass'", result)
	}
}

// TestInstallFileBackedProvider_UsesEnvVar tests InstallFileBackedProvider prefers env var.
func TestInstallFileBackedProvider_UsesEnvVar(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)
	t.Setenv(PassphraseEnvVar, "env-passphrase")

	// Save a passphrase to file
	SavePassphraseFile("file-passphrase")

	// Install file-backed provider, restoring after test
	oldProvider := PassphraseProvider
	t.Cleanup(func() { PassphraseProvider = oldProvider })
	InstallFileBackedProvider()

	result := PassphraseProvider()
	if result != "env-passphrase" {
		t.Errorf("PassphraseProvider() = %q, want 'env-passphrase' (env var takes priority)", result)
	}
}

// TestInstallFileBackedProvider_FallbackToFile tests InstallFileBackedProvider falls back to file.
func TestInstallFileBackedProvider_FallbackToFile(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)
	t.Setenv(PassphraseEnvVar, "")

	// Save a passphrase to file
	SavePassphraseFile("file-passphrase")

	// Install file-backed provider, restoring after test
	oldProvider := PassphraseProvider
	t.Cleanup(func() { PassphraseProvider = oldProvider })
	InstallFileBackedProvider()

	result := PassphraseProvider()
	if result != "file-passphrase" {
		t.Errorf("PassphraseProvider() = %q, want 'file-passphrase' (fallback to file)", result)
	}
}

// TestInstallFileBackedProvider_NoFileNoEnv tests file-backed provider with neither.
func TestInstallFileBackedProvider_NoFileNoEnv(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)
	t.Setenv(PassphraseEnvVar, "")

	// Install file-backed provider, restoring after test (no file created)
	oldProvider := PassphraseProvider
	t.Cleanup(func() { PassphraseProvider = oldProvider })
	InstallFileBackedProvider()

	result := PassphraseProvider()
	if result != "" {
		t.Errorf("PassphraseProvider() = %q, want empty (no file, no env)", result)
	}
}

// TestLoadPassphraseFile_EmptyFile tests LoadPassphraseFile with empty file.
func TestLoadPassphraseFile_EmptyFile(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Create empty file
	path := PassphraseFilePath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(""), 0o600)

	result := LoadPassphraseFile()
	if result != "" {
		t.Errorf("LoadPassphraseFile() = %q, want empty", result)
	}
}

// TestLoadPassphraseFile_OnlyWhitespace tests LoadPassphraseFile with only whitespace.
func TestLoadPassphraseFile_OnlyWhitespace(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Create file with only whitespace
	path := PassphraseFilePath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("   \n  \t  \n"), 0o600)

	result := LoadPassphraseFile()
	if result != "" {
		t.Errorf("LoadPassphraseFile() = %q, want empty (only whitespace)", result)
	}
}

// TestPassphraseFile_Roundtrip tests Save and Load roundtrip.
func TestPassphraseFile_Roundtrip(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	testCases := []string{
		"simple-pass",
		"pass with spaces",
		"pass-with-special-!@#$%",
		"very-long-" + strings.Repeat("x", 1000),
	}

	for _, tc := range testCases {
		// Save
		err := SavePassphraseFile(tc)
		if err != nil {
			t.Fatalf("SavePassphraseFile: %v", err)
		}

		// Load
		result := LoadPassphraseFile()
		if result != tc {
			t.Errorf("roundtrip failed: saved %q, loaded %q", tc, result)
		}
	}
}

// TestLoadPassphraseFile_UnreadableFile tests behavior with permission-denied file.
func TestLoadPassphraseFile_UnreadableFile(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Create file but remove read permission
	path := PassphraseFilePath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("secret"), 0o000)
	t.Cleanup(func() {
		os.Chmod(path, 0o600)
	})

	// LoadPassphraseFile should return empty string (error is silently ignored)
	result := LoadPassphraseFile()
	if result != "" {
		t.Errorf("LoadPassphraseFile on unreadable file = %q, want empty", result)
	}
}

// TestSavePassphraseFile_MultipleWrites tests overwriting passphrase multiple times.
func TestSavePassphraseFile_MultipleWrites(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Write multiple times
	passphrases := []string{"first", "second", "third"}
	for _, pass := range passphrases {
		err := SavePassphraseFile(pass)
		if err != nil {
			t.Fatalf("SavePassphraseFile: %v", err)
		}

		loaded := LoadPassphraseFile()
		if loaded != pass {
			t.Errorf("after write: loaded %q, want %q", loaded, pass)
		}
	}

	// Final load should have the last value
	final := LoadPassphraseFile()
	if final != "third" {
		t.Errorf("final load = %q, want 'third'", final)
	}
}

// TestInstallFileBackedProvider_EnvVarPriority tests environment variable priority.
func TestInstallFileBackedProvider_EnvVarPriority(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Create both env var and file with different values
	envVal := "env-value"
	fileVal := "file-value"

	SavePassphraseFile(fileVal)
	t.Setenv(PassphraseEnvVar, envVal)

	// Install file-backed provider, restoring after test
	oldProvider := PassphraseProvider
	t.Cleanup(func() { PassphraseProvider = oldProvider })
	InstallFileBackedProvider()

	// Env var should take priority
	result := PassphraseProvider()
	if result != envVal {
		t.Errorf("PassphraseProvider() = %q, want %q (env takes priority)", result, envVal)
	}
}

// TestLoadPassphraseFile_Symlink tests loading from a symlink.
func TestLoadPassphraseFile_Symlink(t *testing.T) {
	customHome := t.TempDir()
	t.Setenv(khunquantHome, customHome)

	// Create actual file at a different location
	actualFile := filepath.Join(customHome, "actual_pass")
	os.WriteFile(actualFile, []byte("symlink-test\n"), 0o600)

	// Create symlink to it
	linkPath := PassphraseFilePath()
	os.MkdirAll(filepath.Dir(linkPath), 0o700)
	os.Symlink(actualFile, linkPath)

	result := LoadPassphraseFile()
	if result != "symlink-test" {
		t.Errorf("LoadPassphraseFile via symlink = %q, want 'symlink-test'", result)
	}
}

// H-14: a passphrase file other users can read is refused.
func TestLoadPassphraseFile_RefusesGroupOrWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix mode bits on Windows")
	}
	t.Setenv(khunquantHome, t.TempDir())
	path := PassphraseFilePath()
	if err := os.WriteFile(path, []byte("perm-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadPassphraseFile(); got != "perm-test" {
		t.Fatalf("0600 file not loaded (%q); the refusals below prove nothing", got)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if got := LoadPassphraseFile(); got != "" {
			t.Errorf("mode %#o: passphrase loaded (%q), want refused", mode, got)
		}
	}
}
