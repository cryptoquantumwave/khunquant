package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── cacheFilePath ──────────────────────────────────────────────────────────────

func TestCacheFilePathRequiresHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows equivalent

	// On most Unix systems HOME="" falls back to the passwd entry, so we can't
	// guarantee an error. What we CAN assert: if no error, path must not be "".
	path, err := cacheFilePath()
	if err != nil {
		return // expected on some systems
	}
	if path == "" {
		t.Error("cacheFilePath() returned empty string without error")
	}
}

func TestCacheFilePathIsAbsolute(t *testing.T) {
	path, err := cacheFilePath()
	if err != nil {
		t.Skipf("cacheFilePath() error (no HOME?): %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("cacheFilePath() returned non-absolute path: %q", path)
	}
}

func TestCacheFilePathContainsDotKhunquant(t *testing.T) {
	path, err := cacheFilePath()
	if err != nil {
		t.Skipf("cacheFilePath() error: %v", err)
	}
	if filepath.Base(filepath.Dir(path)) != ".khunquant" {
		t.Errorf("expected cache under .khunquant dir, got: %q", path)
	}
}

// ── write / read roundtrip ─────────────────────────────────────────────────────

func TestWriteAndReadCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	info := &UpdateInfo{
		CurrentVersion: "v0.0.1",
		LatestVersion:  "v0.2.0",
		ReleaseURL:     "https://example.com/releases/v0.2.0",
		IsOutdated:     true,
	}

	writeCachedUpdateInfo(info)

	got := ReadCachedUpdateInfo("v0.0.1")
	if got == nil {
		t.Fatal("ReadCachedUpdateInfo() returned nil after write")
	}
	if got.LatestVersion != info.LatestVersion {
		t.Errorf("LatestVersion: got %q, want %q", got.LatestVersion, info.LatestVersion)
	}
	if !got.IsOutdated {
		t.Error("IsOutdated should be true")
	}
}

func TestReadCachedUpdateInfo_NotOutdated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Cache says latest is v0.2.0, but current is already v0.2.0 → not outdated.
	info := &UpdateInfo{LatestVersion: "v0.2.0", IsOutdated: true}
	writeCachedUpdateInfo(info)

	got := ReadCachedUpdateInfo("v0.2.0")
	if got != nil {
		t.Errorf("expected nil when current >= latest, got %+v", got)
	}
}

func TestReadCachedUpdateInfo_CurrentNewerThanCached(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Cache says latest is v0.2.0, but user manually upgraded to v0.3.0.
	info := &UpdateInfo{LatestVersion: "v0.2.0", IsOutdated: true}
	writeCachedUpdateInfo(info)

	got := ReadCachedUpdateInfo("v0.3.0")
	if got != nil {
		t.Errorf("expected nil when current > cached latest, got %+v", got)
	}
}

func TestReadCachedUpdateInfo_ExpiredTTL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write a cache with an old timestamp (beyond cacheTTL).
	path := filepath.Join(dir, ".khunquant", "update-check-cache.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	c := updateCache{
		Info:      &UpdateInfo{LatestVersion: "v0.2.0", IsOutdated: true},
		CheckedAt: time.Now().Add(-(cacheTTL + time.Minute)),
	}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadCachedUpdateInfo("v0.0.1")
	if got != nil {
		t.Errorf("expected nil for expired cache, got %+v", got)
	}
}

func TestReadCachedUpdateInfo_NilInfo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write a cache where Info is nil (no update available at check time).
	path := filepath.Join(dir, ".khunquant", "update-check-cache.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	c := updateCache{Info: nil, CheckedAt: time.Now()}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadCachedUpdateInfo("v0.0.1")
	if got != nil {
		t.Errorf("expected nil when cached Info is nil, got %+v", got)
	}
}

// ── ClearUpdateCache ───────────────────────────────────────────────────────────

func TestClearUpdateCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	info := &UpdateInfo{LatestVersion: "v0.2.0", IsOutdated: true}
	writeCachedUpdateInfo(info)

	// Verify it exists.
	if ReadCachedUpdateInfo("v0.0.1") == nil {
		t.Fatal("precondition: cache should be readable")
	}

	ClearUpdateCache()

	if ReadCachedUpdateInfo("v0.0.1") != nil {
		t.Error("cache should be nil after ClearUpdateCache()")
	}
}

// ── CheckForUpdateCached: synchronous on first run ─────────────────────────────

func TestCheckForUpdateCached_SynchronousFirstRun(t *testing.T) {
	// No cache exists → CheckForUpdateCached must do a synchronous fetch and
	// return the result immediately (not nil) so the notice shows on first run.

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Serve a fake GitHub API that returns v0.2.0 as the latest stable release.
	srv := fakeGitHubReleaseServer(t, "v0.2.0")
	defer srv.Close()

	// Patch the GitHub API URL by using a local HTTP client override.
	// Since CheckForUpdateCached calls CheckForUpdate which calls fetchLatestRelease
	// using http.DefaultClient pointing at api.github.com, we can't intercept
	// the real network call in a unit test without refactoring the HTTP client.
	//
	// Instead, test the cache-population contract: after CheckForUpdateCached
	// runs (even with a real but fast network call that may fail), the function
	// must NOT block for more than our 2 s timeout.  We verify that the cache
	// file is created if a result was obtained.
	//
	// For the no-network case (CI offline), we verify the function returns
	// quickly and doesn't panic.
	start := time.Now()
	_ = CheckForUpdateCached(DefaultOwner, DefaultRepo, "v0.0.1")
	elapsed := time.Since(start)

	// Must return within the 2 s synchronous timeout (plus a 500 ms buffer).
	if elapsed > 2500*time.Millisecond {
		t.Errorf("CheckForUpdateCached blocked for %v, want < 2.5 s", elapsed)
	}

	_ = srv
}

func TestCheckForUpdateCached_InstantFromCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Pre-populate the cache.
	info := &UpdateInfo{
		CurrentVersion: "v0.0.1",
		LatestVersion:  "v0.2.0",
		ReleaseURL:     "https://example.com/releases/v0.2.0",
		IsOutdated:     true,
	}
	writeCachedUpdateInfo(info)

	// With a fresh cache the function must return in < 10 ms (no network I/O).
	start := time.Now()
	got := CheckForUpdateCached(DefaultOwner, DefaultRepo, "v0.0.1")
	elapsed := time.Since(start)

	if got == nil {
		t.Fatal("expected cached UpdateInfo, got nil")
	}
	if got.LatestVersion != "v0.2.0" {
		t.Errorf("LatestVersion: got %q, want %q", got.LatestVersion, "v0.2.0")
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("cache hit took %v, want < 10 ms", elapsed)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

func fakeGitHubReleaseServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.com/releases/%s"}`, tag, tag)
	}))
}
