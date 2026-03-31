package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SelfUpdate downloads the latest release for the current platform and atomically
// replaces destPath with the updated binary named binaryName.
//
// Returns (info, nil) on successful update, (nil, nil) if already up-to-date or
// currentVersion is "dev". Returns an error if the update fails.
func SelfUpdate(ctx context.Context, owner, repo, currentVersion, binaryName, destPath string) (*UpdateInfo, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("automatic update is not supported on Windows — please download manually")
	}

	info, err := CheckForUpdate(ctx, owner, repo, currentVersion)
	if err != nil {
		return nil, fmt.Errorf("checking for update: %w", err)
	}
	if info == nil || !info.IsOutdated {
		return nil, nil // already up-to-date
	}

	assetName, assetExt, err := platformAsset()
	if err != nil {
		return nil, err
	}

	dlURL, csURL, err := fetchReleaseAssetURLs(ctx, owner, repo, info.LatestVersion, assetName)
	if err != nil {
		return nil, fmt.Errorf("fetching release assets: %w", err)
	}
	if dlURL == "" {
		return nil, fmt.Errorf(
			"no binary found for %s/%s in release %s — please download manually: %s",
			runtime.GOOS, runtime.GOARCH, info.LatestVersion, info.ReleaseURL,
		)
	}

	if err := downloadVerifyReplace(ctx, dlURL, csURL, assetName, assetExt, binaryName, destPath); err != nil {
		return nil, err
	}
	return info, nil
}

// platformAsset returns the archive filename and extension for the current OS/arch,
// following the naming convention in .goreleaser.yaml.
func platformAsset() (name, ext string, err error) {
	osNames := map[string]string{
		"linux": "Linux", "darwin": "Darwin", "windows": "Windows",
		"freebsd": "Freebsd", "netbsd": "Netbsd",
	}
	archNames := map[string]string{
		"amd64": "x86_64", "386": "i386",
		"arm64": "arm64", "arm": "armv7",
		"riscv64": "riscv64", "loong64": "loong64",
		"s390x": "s390x", "mipsle": "mipsle",
	}

	osName, ok := osNames[runtime.GOOS]
	if !ok {
		return "", "", fmt.Errorf("unsupported OS for self-update: %s", runtime.GOOS)
	}
	archName, ok := archNames[runtime.GOARCH]
	if !ok {
		archName = runtime.GOARCH
	}

	ext = "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	name = fmt.Sprintf("khunquant_%s_%s.%s", osName, archName, ext)
	return name, ext, nil
}

// downloadVerifyReplace downloads the archive, verifies the SHA256 checksum,
// extracts binaryName, and atomically replaces destPath.
func downloadVerifyReplace(ctx context.Context, dlURL, csURL, assetName, assetExt, binaryName, destPath string) error {
	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Download archive to a temp file.
	tmp, err := os.CreateTemp("", "khunquant-update-*.download")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := httpDownload(dlCtx, dlURL, tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("downloading archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Verify SHA256 against checksums.txt (best-effort; skip if unavailable).
	if err := verifyChecksums(dlCtx, csURL, assetName, tmpPath); err != nil {
		return err
	}

	// Extract the binary to a temp path alongside destPath, then atomically rename.
	newPath := destPath + ".new"
	defer os.Remove(newPath)

	if assetExt == "zip" {
		if err := extractFromZip(tmpPath, binaryName, newPath); err != nil {
			return fmt.Errorf("extracting from zip: %w", err)
		}
	} else {
		if err := extractFromTarGz(tmpPath, binaryName, newPath); err != nil {
			return fmt.Errorf("extracting from tar.gz: %w", err)
		}
	}

	// Preserve original file permissions.
	perm := os.FileMode(0755)
	if fi, err := os.Stat(destPath); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := os.Chmod(newPath, perm); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}

	// Atomic replace (safe on Unix — old process keeps old inode).
	if err := os.Rename(newPath, destPath); err != nil {
		return fmt.Errorf("replacing binary (permission issue?): %w", err)
	}
	return nil
}

func httpDownload(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "khunquant/selfupdate")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// verifyChecksums fetches checksums.txt and verifies the SHA256 of localPath.
// Silently skips if the checksum file is unavailable.
func verifyChecksums(ctx context.Context, csURL, assetName, localPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, csURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "khunquant/selfupdate")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil // checksums unavailable — skip
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	// Find line: "<sha256>  <assetName>"
	var expected string
	for _, line := range strings.Split(string(body), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == assetName {
			expected = parts[0]
			break
		}
	}
	if expected == "" {
		return nil // asset not listed — skip
	}

	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expected {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, expected)
	}
	return nil
}

func extractFromTarGz(archivePath, binaryName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == binaryName {
			return writeStream(tr, destPath)
		}
	}
	return fmt.Errorf("binary %q not found in archive", binaryName)
}

func extractFromZip(archivePath, binaryName, destPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if filepath.Base(f.Name) == binaryName {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			writeErr := writeStream(rc, destPath)
			rc.Close()
			return writeErr
		}
	}
	return fmt.Errorf("binary %q not found in zip archive", binaryName)
}

// fetchReleaseAssetURLs looks up the download URL for assetName and the
// checksums.txt file in the given release tag via the GitHub Releases API.
// Returns ("", "", nil) when the release exists but has no assets yet.
func fetchReleaseAssetURLs(ctx context.Context, owner, repo, tag, assetName string) (dlURL, csURL string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "khunquant/selfupdate")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github API returned %d for release %s", resp.StatusCode, tag)
	}

	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}

	for _, a := range release.Assets {
		switch a.Name {
		case assetName:
			dlURL = a.BrowserDownloadURL
		case "checksums.txt":
			csURL = a.BrowserDownloadURL
		}
	}
	return dlURL, csURL, nil
}

func writeStream(r io.Reader, destPath string) error {
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, r) //nolint:gosec // trusted release artifact
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
