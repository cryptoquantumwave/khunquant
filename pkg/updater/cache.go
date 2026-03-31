package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const cacheTTL = 24 * time.Hour

type updateCache struct {
	Info      *UpdateInfo `json:"info"`
	CheckedAt time.Time   `json:"checked_at"`
}

func cacheFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	return filepath.Join(home, ".khunquant", "update-check-cache.json"), nil
}

// ReadCachedUpdateInfo returns the cached UpdateInfo if it is still within the
// TTL and the current version is still outdated. Returns nil otherwise.
func ReadCachedUpdateInfo(currentVersion string) *UpdateInfo {
	path, err := cacheFilePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c updateCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if time.Since(c.CheckedAt) > cacheTTL {
		return nil
	}
	if c.Info == nil {
		return nil
	}
	// Re-evaluate in case the user manually installed the new version.
	if compareVersions(currentVersion, c.Info.LatestVersion) >= 0 {
		return nil
	}
	c.Info.CurrentVersion = currentVersion
	c.Info.IsOutdated = true
	return c.Info
}

// writeCachedUpdateInfo saves the result of a GitHub update check to disk.
func writeCachedUpdateInfo(info *UpdateInfo) {
	path, err := cacheFilePath()
	if err != nil {
		return
	}
	c := updateCache{Info: info, CheckedAt: time.Now()}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, data, 0o644)
}

// ClearUpdateCache removes the cached update result. Call this after a
// successful self-update so the next run does not show a stale notice.
func ClearUpdateCache() {
	path, err := cacheFilePath()
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

// CheckForUpdateCached returns update info without blocking the caller for more
// than a short time.
//
// Strategy:
//   - If a fresh cache exists: return it immediately and refresh in background.
//   - If no cache exists: do a synchronous check (2 s timeout) so the notice
//     appears on the first run and the cache is warm for subsequent runs.
//     Short-lived commands (e.g. `khunquant version`) exit before a background
//     goroutine can write the cache, so the synchronous path is necessary.
func CheckForUpdateCached(owner, repo, currentVersion string) *UpdateInfo {
	cached := ReadCachedUpdateInfo(currentVersion)

	if cached != nil {
		// Fresh cache found — return instantly and refresh in the background
		// so the next invocation stays up-to-date.
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			info, err := CheckForUpdate(bgCtx, owner, repo, currentVersion)
			if err == nil {
				writeCachedUpdateInfo(info)
			}
		}()
		return cached
	}

	// No cache: populate it now with a short timeout.  GitHub API typically
	// responds in < 300 ms, so 2 s is generous without blocking startup noticeably.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := CheckForUpdate(ctx, owner, repo, currentVersion)
	if err == nil {
		writeCachedUpdateInfo(info)
	}
	return info
}
