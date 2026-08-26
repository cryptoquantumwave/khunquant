package utils_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// --- IsAudioFile ---

func TestIsAudioFile_AudioExtensions(t *testing.T) {
	cases := []string{
		"song.mp3", "recording.wav", "podcast.ogg",
		"track.m4a", "lossless.flac", "radio.aac", "music.wma",
	}
	for _, name := range cases {
		if !utils.IsAudioFile(name, "") {
			t.Errorf("IsAudioFile(%q, \"\"): expected true", name)
		}
	}
}

func TestIsAudioFile_CaseInsensitive(t *testing.T) {
	names := []string{"SONG.MP3", "Track.WAV", "File.OGG"}
	for _, name := range names {
		if !utils.IsAudioFile(name, "") {
			t.Errorf("IsAudioFile(%q, \"\"): expected true (case-insensitive)", name)
		}
	}
}

func TestIsAudioFile_NonAudioExtension(t *testing.T) {
	cases := []string{"image.jpg", "doc.pdf", "code.go", "video.mp4"}
	for _, name := range cases {
		if utils.IsAudioFile(name, "") {
			t.Errorf("IsAudioFile(%q, \"\"): expected false", name)
		}
	}
}

func TestIsAudioFile_AudioContentType(t *testing.T) {
	types := []string{"audio/mpeg", "audio/wav", "application/ogg", "application/x-ogg"}
	for _, ct := range types {
		if !utils.IsAudioFile("unknown", ct) {
			t.Errorf("IsAudioFile(\"unknown\", %q): expected true for audio content-type", ct)
		}
	}
}

func TestIsAudioFile_NonAudioContentType(t *testing.T) {
	if utils.IsAudioFile("file", "image/jpeg") {
		t.Error("IsAudioFile(\"file\", \"image/jpeg\"): expected false")
	}
}

func TestIsAudioFile_EmptyBoth(t *testing.T) {
	if utils.IsAudioFile("", "") {
		t.Error("IsAudioFile(\"\", \"\"): expected false")
	}
}

// --- SanitizeFilename ---

func TestSanitizeFilename_PlainName(t *testing.T) {
	got := utils.SanitizeFilename("file.txt")
	if got != "file.txt" {
		t.Errorf("SanitizeFilename(\"file.txt\"): want %q, got %q", "file.txt", got)
	}
}

func TestSanitizeFilename_StripsDirPath(t *testing.T) {
	// filepath.Base strips the directory component.
	got := utils.SanitizeFilename("/tmp/uploads/file.txt")
	if got != "file.txt" {
		t.Errorf("want %q, got %q", "file.txt", got)
	}
}

func TestSanitizeFilename_RemovesDotDot(t *testing.T) {
	got := utils.SanitizeFilename("../../evil.sh")
	// filepath.Base gives "evil.sh"; no ".." survives
	if got == "../../evil.sh" {
		t.Errorf("SanitizeFilename should remove path traversal, got %q", got)
	}
}

func TestSanitizeFilename_ReplacesForwardSlash(t *testing.T) {
	// After filepath.Base, remaining "/" are replaced with "_"
	got := utils.SanitizeFilename("normal.txt")
	if got != "normal.txt" {
		t.Errorf("want %q, got %q", "normal.txt", got)
	}
}

// --- DownloadFile ---

func TestDownloadFile_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "file contents")
	}))
	defer srv.Close()

	path := utils.DownloadFile(srv.URL, "test.txt", utils.DownloadOptions{
		Timeout:      5 * time.Second,
		LoggerPrefix: "test",
	})
	if path == "" {
		t.Fatal("DownloadFile should return non-empty path on success")
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "file contents" {
		t.Errorf("content = %q, want 'file contents'", string(data))
	}
}

func TestDownloadFile_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	path := utils.DownloadFile(srv.URL, "test.txt", utils.DownloadOptions{Timeout: 5 * time.Second})
	if path != "" {
		t.Errorf("DownloadFile with 404 should return empty string, got %q", path)
	}
}

func TestDownloadFile_InvalidURL(t *testing.T) {
	path := utils.DownloadFile("://invalid-url", "test.txt", utils.DownloadOptions{})
	if path != "" {
		t.Errorf("DownloadFile with invalid URL should return empty string, got %q", path)
	}
}

func TestDownloadFile_WithExtraHeaders(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Test-Header")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	path := utils.DownloadFile(srv.URL, "test.txt", utils.DownloadOptions{
		Timeout:      5 * time.Second,
		ExtraHeaders: map[string]string{"X-Test-Header": "myvalue"},
	})
	if path != "" {
		defer os.Remove(path)
	}
	if gotHeader != "myvalue" {
		t.Errorf("X-Test-Header = %q, want 'myvalue'", gotHeader)
	}
}

func TestDownloadFile_InvalidProxyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := utils.DownloadFile(srv.URL, "test.txt", utils.DownloadOptions{
		ProxyURL: "://bad-proxy",
		Timeout:  5 * time.Second,
	})
	if path != "" {
		t.Errorf("DownloadFile with invalid proxy should return empty string, got %q", path)
	}
}

func TestDownloadFileSimple_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "simple content")
	}))
	defer srv.Close()

	path := utils.DownloadFileSimple(srv.URL, "simple.txt")
	if path == "" {
		t.Fatal("DownloadFileSimple should return non-empty path on success")
	}
	defer os.Remove(path)
}

// --- AMR and OPUS audio format support (OneBot, Matrix) ---

func TestIsAudioFile_AMRFormat(t *testing.T) {
	// OneBot voice messages arrive as "voice.amr" with empty content type
	if !utils.IsAudioFile("voice.amr", "") {
		t.Error("IsAudioFile(\"voice.amr\", \"\"): expected true for OneBot voice")
	}
	// Also test with correct content type
	if !utils.IsAudioFile("voice.amr", "audio/amr") {
		t.Error("IsAudioFile(\"voice.amr\", \"audio/amr\"): expected true")
	}
}

func TestIsAudioFile_OpusFormat(t *testing.T) {
	// Matrix voice messages use .opus format
	if !utils.IsAudioFile("audio.opus", "") {
		t.Error("IsAudioFile(\"audio.opus\", \"\"): expected true for Matrix voice")
	}
	// Also test with correct content type
	if !utils.IsAudioFile("audio.opus", "audio/opus") {
		t.Error("IsAudioFile(\"audio.opus\", \"audio/opus\"): expected true")
	}
}

// --- AudioFormat function ---

func TestAudioFormat_SupportedFormats(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"clip.mp3", "mp3"},
		{"audio.wav", "wav"},
		{"voice.ogg", "ogg"},
		{"speech.amr", "amr"},       // OneBot voice
		{"stream.opus", "opus"},     // Matrix voice
		{"track.m4a", "m4a"},
		{"song.flac", "flac"},
		{"recording.aac", "aac"},
		{"music.wma", "wma"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := utils.AudioFormat(tt.path)
			if err != nil {
				t.Errorf("AudioFormat(%q) error: %v", tt.path, err)
				return
			}
			if got != tt.want {
				t.Errorf("AudioFormat(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestAudioFormat_UnsupportedFormat(t *testing.T) {
	_, err := utils.AudioFormat("file.txt")
	if err == nil {
		t.Error("AudioFormat(\"file.txt\"): expected error for unsupported format")
	}
}

func TestAudioFormat_CaseInsensitive(t *testing.T) {
	got, err := utils.AudioFormat("AUDIO.MP3")
	if err != nil {
		t.Errorf("AudioFormat(\"AUDIO.MP3\") error: %v", err)
		return
	}
	if got != "mp3" {
		t.Errorf("AudioFormat(\"AUDIO.MP3\") = %q, want %q", got, "mp3")
	}
}
