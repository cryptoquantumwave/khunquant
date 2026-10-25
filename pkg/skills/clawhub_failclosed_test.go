package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moderationServer serves metadata via metaHandler and a valid skill ZIP,
// counting downloads so tests can prove nothing was fetched.
func moderationServer(t *testing.T, metaHandler http.HandlerFunc) (*httptest.Server, *int) {
	t.Helper()
	zipBuf := createTestZip(t, map[string]string{
		"SKILL.md": "---\nname: mod-skill\ndescription: A test\n---\nHello",
	})
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/skills/mod-skill":
			metaHandler(w, r)
		case "/api/v1/download":
			downloads++
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBuf)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func metadataUnavailable(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

func assertNotInstalled(t *testing.T, targetDir string) {
	t.Helper()
	_, err := os.Stat(targetDir)
	assert.True(t, os.IsNotExist(err), "target directory exists: %v", err)
}

func TestDownloadAndInstall_RefusesWhenModerationIsUnavailable(t *testing.T) {
	srv, downloads := moderationServer(t, metadataUnavailable)
	targetDir := filepath.Join(t.TempDir(), "mod-skill")

	_, err := newTestRegistry(srv.URL, "").DownloadAndInstall(context.Background(), "mod-skill", "1.0.0", targetDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_unverified")
	assert.Equal(t, 0, *downloads, "downloaded code it could not verify")
	assertNotInstalled(t, targetDir)
}

// Positive control for the test above: the same server installs once the
// operator opts in, and the result says it was unverified.
func TestDownloadAndInstall_AllowUnverifiedInstallsAndSaysSo(t *testing.T) {
	srv, _ := moderationServer(t, metadataUnavailable)
	targetDir := filepath.Join(t.TempDir(), "mod-skill")

	reg := NewClawHubRegistry(ClawHubConfig{Enabled: true, BaseURL: srv.URL, AllowUnverified: true})
	result, err := reg.DownloadAndInstall(context.Background(), "mod-skill", "1.0.0", targetDir)

	require.NoError(t, err)
	assert.True(t, result.Unverified)
	_, err = os.Stat(filepath.Join(targetDir, "SKILL.md"))
	require.NoError(t, err)
}

func TestDownloadAndInstall_MalwareIsRefusedBeforeDownload(t *testing.T) {
	srv, downloads := moderationServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(clawhubSkillResponse{
			Slug:          "mod-skill",
			LatestVersion: &clawhubVersionInfo{Version: "1.0.0"},
			Moderation:    &clawhubModerationInfo{IsMalwareBlocked: true},
		})
	})
	targetDir := filepath.Join(t.TempDir(), "mod-skill")

	result, err := newTestRegistry(srv.URL, "").DownloadAndInstall(context.Background(), "mod-skill", "", targetDir)

	require.NoError(t, err) // callers refuse on the flag, as before
	assert.True(t, result.IsMalwareBlocked)
	assert.Equal(t, 0, *downloads, "downloaded a skill already known to be malware")
	assertNotInstalled(t, targetDir)
}

// A failed extraction leaves neither the skill nor its staging directory.
func TestDownloadAndInstall_FailedExtractionLeavesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/skills/mod-skill":
			_ = json.NewEncoder(w).Encode(clawhubSkillResponse{
				Slug: "mod-skill", LatestVersion: &clawhubVersionInfo{Version: "1.0.0"},
			})
		case "/api/v1/download":
			_, _ = w.Write([]byte("this is not a zip"))
		}
	}))
	defer srv.Close()
	parent := t.TempDir()
	targetDir := filepath.Join(parent, "mod-skill")

	_, err := newTestRegistry(srv.URL, "").DownloadAndInstall(context.Background(), "mod-skill", "", targetDir)

	require.Error(t, err)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	for _, e := range entries {
		t.Errorf("left %q behind in the skills directory", e.Name())
	}
}

// The staging directory is dot-prefixed so a concurrent skills scan cannot load
// a half-extracted skill. Real skill directories still load.
func TestListSkills_SkipsHiddenDirectories(t *testing.T) {
	ws := t.TempDir()
	skillsDir := filepath.Join(ws, "skills")
	createSkillDir(t, skillsDir, ".mod-skill.installing-123", "staged-skill", "half extracted")
	createSkillDir(t, skillsDir, "real-skill", "real-skill", "installed")

	var names []string
	for _, s := range NewSkillsLoader(ws, "", "").ListSkills() {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"real-skill"}, names, "loaded: %s", strings.Join(names, ","))
}
