package caddy_maxmind_geolocation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	githubAPIBase = "https://api.github.com"
	defaultRef    = "main"
)

// For tests: override to use mock server.
var (
	githubAPIBaseURL = githubAPIBase
	githubHTTPClient = http.DefaultClient
)

// githubReleaseResponse represents the relevant part of GitHub Releases API response.
// See: https://docs.github.com/en/rest/releases/releases#get-the-latest-release
type githubReleaseResponse struct {
	TagName string          `json:"tag_name"`
	Assets []githubAsset    `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// fetchLatestReleaseTagAndAssetURL returns tag name and download URL for the given asset name.
func fetchLatestReleaseTagAndAssetURL(repo, assetName, token string) (tag string, downloadURL string, err error) {
	owner, name, err := parseRepo(repo)
	if err != nil {
		return "", "", err
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", githubAPIBaseURL, owner, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	setGitHubHeaders(req, token)

	resp, err := githubHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("github request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("github API %s: %s", resp.Status, string(body))
	}

	var release githubReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", fmt.Errorf("github response decode: %w", err)
	}

	for _, a := range release.Assets {
		if a.Name == assetName {
			return release.TagName, a.BrowserDownloadURL, nil
		}
	}
	return "", "", fmt.Errorf("asset %q not found in release %s (assets: %v)", assetName, release.TagName, assetNames(release.Assets))
}

func assetNames(a []githubAsset) []string {
	names := make([]string, len(a))
	for i := range a {
		names[i] = a[i].Name
	}
	return names
}

func parseRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(strings.TrimSuffix(repo, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid github_repo %q: expected owner/repo", repo)
	}
	return parts[0], parts[1], nil
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// cleanupStaleTempFiles removes temp files in dir with prefix baseName+"."
// (leftover from CreateTemp), but keeps baseName and baseName+".tag".
func cleanupStaleTempFiles(dir, baseName string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := baseName + "."
	tagFile := baseName + ".tag"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == tagFile {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// downloadFile downloads url to dest path. Uses a temp file and rename for atomic write.
func downloadFile(downloadURL, destPath, token string) error {
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	if token != "" && strings.Contains(downloadURL, "api.github.com") {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := githubHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download %s: %s", resp.Status, string(body))
	}

	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// Remove stale temp files from previous runs (e.g. crashed before rename).
	cleanupStaleTempFiles(dir, filepath.Base(destPath))
	tmp, err := os.CreateTemp(dir, filepath.Base(destPath)+".*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	_, err = io.Copy(tmp, resp.Body)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// tagPath returns the path of the file storing the current release tag (e.g. cache.mmdb -> cache.mmdb.tag).
func tagPath(cachePath string) string {
	return cachePath + ".tag"
}

func readStoredTag(cachePath string) string {
	b, err := os.ReadFile(tagPath(cachePath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeStoredTag(cachePath, tag string) error {
	return os.WriteFile(tagPath(cachePath), []byte(tag+"\n"), 0644)
}

// syncFromGitHubRelease downloads the given asset from the latest GitHub release to cachePath if tag changed.
// Returns the tag and true if a new file was written, false if already up to date.
// If the cache file is missing (e.g. was deleted), re-downloads regardless of .tag.
func syncFromGitHubRelease(repo, assetName, cachePath, token string) (tag string, updated bool, err error) {
	tag, downloadURL, err := fetchLatestReleaseTagAndAssetURL(repo, assetName, token)
	if err != nil {
		return "", false, err
	}
	if _, statErr := os.Stat(cachePath); os.IsNotExist(statErr) {
		// Cache file missing — force download; .tag is stale.
		_ = os.Remove(tagPath(cachePath))
	} else {
		stored := readStoredTag(cachePath)
		if stored == tag {
			return tag, false, nil
		}
	}
	if err := downloadFile(downloadURL, cachePath, token); err != nil {
		return "", false, err
	}
	if err := writeStoredTag(cachePath, tag); err != nil {
		// non-fatal
	}
	return tag, true, nil
}

