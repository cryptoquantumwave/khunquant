package utils_test

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// writeZip creates a ZIP file at zipPath containing the given entries.
// Each entry is (name, content) where content is the file body.
func writeZip(t *testing.T, zipPath string, entries []struct{ name, body string }) {
	t.Helper()
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	for _, e := range entries {
		fw, err := w.Create(e.name)
		if err != nil {
			t.Fatalf("zip create entry %q: %v", e.name, err)
		}
		if _, err := fw.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip write entry %q: %v", e.name, err)
		}
	}
}

func TestExtractZipFile_NormalFile(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")
	dest := filepath.Join(dir, "out")

	writeZip(t, zipPath, []struct{ name, body string }{
		{"hello.txt", "hello world"},
	})

	if err := utils.ExtractZipFile(zipPath, dest); err != nil {
		t.Fatalf("ExtractZipFile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("content: want %q, got %q", "hello world", string(data))
	}
}

func TestExtractZipFile_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	dest := filepath.Join(dir, "out")

	writeZip(t, zipPath, []struct{ name, body string }{
		{"../../evil.txt", "malicious"},
	})

	err := utils.ExtractZipFile(zipPath, dest)
	if err == nil {
		t.Fatal("expected error for path traversal entry, got nil")
	}

	// The evil file must not have been created outside dest.
	parentEvil := filepath.Join(filepath.Dir(dir), "evil.txt")
	if _, statErr := os.Stat(parentEvil); statErr == nil {
		t.Error("path traversal file was created — security violation")
	}
}

func TestExtractZipFile_InvalidZipReturnsError(t *testing.T) {
	dir := t.TempDir()
	badZip := filepath.Join(dir, "bad.zip")
	if err := os.WriteFile(badZip, []byte("not a zip file"), 0o600); err != nil {
		t.Fatalf("write bad zip: %v", err)
	}

	err := utils.ExtractZipFile(badZip, filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("expected error for invalid ZIP, got nil")
	}
}

func TestExtractZipFile_SubdirectoryEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "nested.zip")
	dest := filepath.Join(dir, "out")

	writeZip(t, zipPath, []struct{ name, body string }{
		{"subdir/file.txt", "nested content"},
	})

	if err := utils.ExtractZipFile(zipPath, dest); err != nil {
		t.Fatalf("ExtractZipFile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "subdir", "file.txt"))
	if err != nil {
		t.Fatalf("read nested file: %v", err)
	}
	if string(data) != "nested content" {
		t.Errorf("want %q, got %q", "nested content", string(data))
	}
}

func TestExtractZipFile_OversizedEntryRejected(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "big.zip")
	dest := filepath.Join(dir, "out")

	// 5MB + 1 byte exceeds the limit.
	const maxSize = 5 * 1024 * 1024
	bigContent := make([]byte, maxSize+1)

	writeZip(t, zipPath, []struct{ name, body string }{
		{"big.bin", string(bigContent)},
	})

	err := utils.ExtractZipFile(zipPath, dest)
	if err == nil {
		t.Fatal("expected error for oversized zip entry, got nil")
	}
}
