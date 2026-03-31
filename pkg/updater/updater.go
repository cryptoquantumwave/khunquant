// Package updater checks for new releases on GitHub and returns update info.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

const (
	// DefaultOwner is the GitHub repository owner for khunquant releases.
	DefaultOwner = "armmer016"
	// DefaultRepo is the GitHub repository name for khunquant releases.
	DefaultRepo = "khunquant"
)

// ProgressFunc is called during download with bytes downloaded so far and the
// total size (-1 if unknown).
type ProgressFunc func(downloaded, total int64)

// UpdateInfo describes an available update.
type UpdateInfo struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	ReleaseURL     string `json:"release_url"`
	IsOutdated     bool   `json:"is_outdated"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// CheckForUpdate queries the GitHub Releases API and returns UpdateInfo when a
// newer version is available. Returns nil, nil when the current version is
// "dev" (local/CI builds) or is already up-to-date.
//
// Strategy: try /releases/latest first (stable releases only). If that returns
// 404 (e.g. only pre-releases exist), fall back to /releases and use the most
// recent entry regardless of pre-release status.
func CheckForUpdate(ctx context.Context, owner, repo, currentVersion string) (*UpdateInfo, error) {
	if currentVersion == "" || currentVersion == "dev" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	release, err := fetchLatestRelease(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, nil
	}

	info := &UpdateInfo{
		CurrentVersion: currentVersion,
		LatestVersion:  release.TagName,
		ReleaseURL:     release.HTMLURL,
		IsOutdated:     compareVersions(currentVersion, release.TagName) < 0,
	}
	return info, nil
}

// fetchLatestRelease tries /releases/latest; if the repo has no stable release
// (404), it falls back to listing all releases and returning the first one.
func fetchLatestRelease(ctx context.Context, owner, repo string) (*githubRelease, error) {
	userAgent := fmt.Sprintf("%s/%s-update-check", owner, repo)

	doGet := func(url string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/vnd.github+json")
		return http.DefaultClient.Do(req)
	}

	// Primary: stable releases only.
	resp, err := doGet(fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var r githubRelease
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, err
		}
		if r.TagName != "" {
			return &r, nil
		}
	}

	// Fallback: list all releases (includes pre-releases) and take the newest.
	if resp.StatusCode == http.StatusNotFound {
		resp2, err := doGet(fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=1", owner, repo))
		if err != nil {
			return nil, err
		}
		defer resp2.Body.Close()

		if resp2.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("github API returned %d", resp2.StatusCode)
		}

		var releases []githubRelease
		if err := json.NewDecoder(resp2.Body).Decode(&releases); err != nil {
			return nil, err
		}
		if len(releases) > 0 && releases[0].TagName != "" {
			return &releases[0], nil
		}
	}

	return nil, nil
}

// compareVersions compares two semver version strings (e.g. "v1.2.3" or
// "1.2.3-rc1"). Returns negative if a < b, zero if equal, positive if a > b.
// Pre-release versions are correctly ordered below their release counterparts
// per the semver spec (e.g. "1.2.0-rc1" < "1.2.0"). Falls back to lexicographic
// comparison for non-semver strings.
func compareVersions(a, b string) int {
	va, errA := semver.NewVersion(a)
	vb, errB := semver.NewVersion(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	return va.Compare(vb)
}
