package caddy_maxmind_geolocation

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/oschwald/maxminddb-golang"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		repo      string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{"P3TERX/GeoLite.mmdb", "P3TERX", "GeoLite.mmdb", false},
		{"owner/repo", "owner", "repo", false},
		{"owner/repo/", "owner", "repo", false},
		{"", "", "", true},
		{"single", "", "", true},
		{"a/", "", "", true},
		{"/b", "", "", true},
	}
	for _, tt := range tests {
		owner, name, err := parseRepo(tt.repo)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRepo(%q) err = %v, wantErr %v", tt.repo, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (owner != tt.wantOwner || name != tt.wantName) {
			t.Errorf("parseRepo(%q) = %q, %q; want %q, %q", tt.repo, owner, name, tt.wantOwner, tt.wantName)
		}
	}
}

func TestFetchLatestReleaseTagAndAssetURL_Mock(t *testing.T) {
	release := githubReleaseResponse{
		TagName: "2026.02.25",
		Assets: []githubAsset{
			{Name: "GeoLite2-Country.mmdb", BrowserDownloadURL: "https://example.com/country.mmdb"},
			{Name: "GeoLite2-City.mmdb", BrowserDownloadURL: "https://example.com/city.mmdb"},
		},
	}
	body, _ := json.Marshal(release)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/P3TERX/GeoLite.mmdb/releases/latest" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("missing Accept header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer server.Close()

	oldBase := githubAPIBaseURL
	oldClient := githubHTTPClient
	githubAPIBaseURL = server.URL
	githubHTTPClient = server.Client()
	defer func() {
		githubAPIBaseURL = oldBase
		githubHTTPClient = oldClient
	}()

	tag, url, err := fetchLatestReleaseTagAndAssetURL("P3TERX/GeoLite.mmdb", "GeoLite2-Country.mmdb", "")
	if err != nil {
		t.Fatalf("fetchLatestReleaseTagAndAssetURL: %v", err)
	}
	if tag != "2026.02.25" {
		t.Errorf("tag = %q, want 2026.02.25", tag)
	}
	if url != "https://example.com/country.mmdb" {
		t.Errorf("download URL = %q, want https://example.com/country.mmdb", url)
	}

	_, _, err = fetchLatestReleaseTagAndAssetURL("P3TERX/GeoLite.mmdb", "Missing.mmdb", "")
	if err == nil {
		t.Error("expected error for missing asset")
	}
}

func TestFetchLatestReleaseTagAndAssetURL_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer server.Close()

	oldBase := githubAPIBaseURL
	oldClient := githubHTTPClient
	githubAPIBaseURL = server.URL
	githubHTTPClient = server.Client()
	defer func() {
		githubAPIBaseURL = oldBase
		githubHTTPClient = oldClient
	}()

	_, _, err := fetchLatestReleaseTagAndAssetURL("P3TERX/GeoLite.mmdb", "GeoLite2-Country.mmdb", "")
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestDownloadFile_Mock(t *testing.T) {
	fakeContent := []byte("fake mmdb content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakeContent)
	}))
	defer server.Close()

	oldClient := githubHTTPClient
	githubHTTPClient = server.Client()
	defer func() { githubHTTPClient = oldClient }()

	dir := t.TempDir()
	dest := filepath.Join(dir, "GeoLite2-Country.mmdb")

	err := downloadFile(server.URL, dest, "")
	if err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(fakeContent) {
		t.Errorf("content = %q, want %q", got, fakeContent)
	}
}

func TestSyncFromGitHubRelease_Mock(t *testing.T) {
	fakeContent := []byte("fake mmdb")
	var downloadURL string
	release := githubReleaseResponse{
		TagName: "v1.0.0",
		Assets:  []githubAsset{{Name: "GeoLite2-Country.mmdb", BrowserDownloadURL: ""}},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/P3TERX/GeoLite.mmdb/releases/latest" {
			release.Assets[0].BrowserDownloadURL = "http://" + r.Host + "/asset"
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/asset" {
			downloadURL = "http://" + r.Host + "/asset"
			w.Write(fakeContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	release.Assets[0].BrowserDownloadURL = server.URL + "/asset"

	oldBase := githubAPIBaseURL
	oldClient := githubHTTPClient
	githubAPIBaseURL = server.URL
	githubHTTPClient = server.Client()
	defer func() {
		githubAPIBaseURL = oldBase
		githubHTTPClient = oldClient
	}()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "GeoLite2-Country.mmdb")

	tag, updated, err := syncFromGitHubRelease("P3TERX/GeoLite.mmdb", "GeoLite2-Country.mmdb", cachePath, "")
	if err != nil {
		t.Fatalf("syncFromGitHubRelease: %v", err)
	}
	if tag != "v1.0.0" {
		t.Errorf("tag = %q, want v1.0.0", tag)
	}
	if !updated {
		t.Error("expected updated = true on first sync")
	}
	content, _ := os.ReadFile(cachePath)
	if string(content) != string(fakeContent) {
		t.Errorf("cache content = %q", content)
	}
	if readStoredTag(cachePath) != "v1.0.0" {
		t.Error("stored tag mismatch")
	}

	// Second call: same tag -> no update
	_, updated2, err := syncFromGitHubRelease("P3TERX/GeoLite.mmdb", "GeoLite2-Country.mmdb", cachePath, "")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if updated2 {
		t.Error("expected updated = false when tag unchanged")
	}

	_ = downloadURL
}

func TestCleanupStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	base := "GeoLite2-Country.mmdb"
	tagPath := filepath.Join(dir, base+".tag")
	realPath := filepath.Join(dir, base)
	stalePath := filepath.Join(dir, base+".stale123")

	if err := os.WriteFile(tagPath, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupStaleTempFiles(dir, base)

	if _, err := os.Stat(tagPath); os.IsNotExist(err) {
		t.Error(".tag file was removed but should be kept")
	}
	if _, err := os.Stat(realPath); os.IsNotExist(err) {
		t.Error("real file was removed")
	}
	if _, err := os.Stat(stalePath); err == nil {
		t.Error("stale temp file was not removed")
	}
}

func TestTagPathReadWrite(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "db.mmdb")

	if readStoredTag(cachePath) != "" {
		t.Error("readStoredTag on missing file should return empty")
	}
	if err := writeStoredTag(cachePath, "v2.0"); err != nil {
		t.Fatal(err)
	}
	if readStoredTag(cachePath) != "v2.0" {
		t.Errorf("readStoredTag = %q", readStoredTag(cachePath))
	}
}

// TestGitHubIntegration hits the real GitHub API. Run with:
//   go test -v -run TestGitHubIntegration
// Optional: GITHUB_TOKEN for higher rate limit.
func TestGitHubIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	token := os.Getenv("GITHUB_TOKEN")

	tag, url, err := fetchLatestReleaseTagAndAssetURL("P3TERX/GeoLite.mmdb", "GeoLite2-Country.mmdb", token)
	if err != nil {
		t.Fatalf("fetchLatestReleaseTagAndAssetURL (real API): %v", err)
	}
	if tag == "" {
		t.Error("tag is empty")
	}
	if url == "" {
		t.Error("download URL is empty")
	}
	t.Logf("latest release tag: %s", tag)
	t.Logf("download URL: %s", url)
}

// TestDownloadFromGitHubIntegration performs a real download from GitHub and verifies
// the file is a valid MaxMind DB. Run with:
//
//	go test -v -run TestDownloadFromGitHubIntegration
//
// Skips in short mode. Optional: GITHUB_TOKEN for rate limit.
func TestDownloadFromGitHubIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	token := os.Getenv("GITHUB_TOKEN")

	tag, downloadURL, err := fetchLatestReleaseTagAndAssetURL("P3TERX/GeoLite.mmdb", "GeoLite2-Country.mmdb", token)
	if err != nil {
		t.Fatalf("fetch latest release: %v", err)
	}

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "GeoLite2-Country.mmdb")

	if err := downloadFile(downloadURL, cachePath, token); err != nil {
		t.Fatalf("download file: %v", err)
	}

	fi, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	t.Logf("downloaded %s, size %d bytes", tag, fi.Size())
	if fi.Size() == 0 {
		t.Fatal("downloaded file is empty")
	}

	db, err := maxminddb.Open(cachePath)
	if err != nil {
		t.Fatalf("open as maxmind db: %v", err)
	}
	defer db.Close()

	var record Record
	ip := net.ParseIP("8.8.8.8")
	if err := db.Lookup(ip, &record); err != nil {
		t.Fatalf("lookup 8.8.8.8: %v", err)
	}
	t.Logf("lookup 8.8.8.8 -> country: %s", record.Country.ISOCode)
}
