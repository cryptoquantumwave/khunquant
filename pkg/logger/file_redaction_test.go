package logger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file sink has no field formatter, so it used to write secrets verbatim
// even though the console redacted them. Every path into it is checked here:
// the message, a string field, an error field and a structured field.
func TestFileLogRedactsRegisteredSecrets(t *testing.T) {
	const secret = "sk-file-sink-secret-4f9a2c"
	RegisterSecret(secret)

	path := filepath.Join(t.TempDir(), "k.log")
	if err := EnableFileLogging(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(DisableFileLogging)

	InfoCF("test", "calling with key "+secret, map[string]any{
		"url":    "https://api.example/v1?key=" + secret,
		"err":    errors.New("401 for key " + secret),
		"params": map[string]any{"api_key": secret, "n": 1},
	})
	DisableFileLogging()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if out == "" {
		t.Fatal("nothing was written to the log file; the assertions below would be vacuous")
	}
	if strings.Contains(out, secret) {
		t.Fatalf("file log contains the secret:\n%s", out)
	}
	// All four placements were redacted, not dropped.
	if n := strings.Count(out, "***REDACTED***"); n < 4 {
		t.Fatalf("expected 4 redactions (message, url, err, params), got %d:\n%s", n, out)
	}
}
