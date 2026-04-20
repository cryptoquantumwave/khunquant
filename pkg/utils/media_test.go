package utils_test

import (
	"testing"

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
