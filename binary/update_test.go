package binary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const fakeReleaseBinary = "#!/bin/sh\necho 0.2.0\n"

func useFakeReleaseServer(t *testing.T, binary, checksums string) *int {
	t.Helper()
	lookups := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing GitHub bearer token")
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/latest":
			lookups++
			if request.Header.Get("Accept") != "application/vnd.github+json" {
				t.Error("wrong release Accept")
			}
			fmt.Fprintf(response, `{"tag_name":"v0.2.0","assets":[{"name":"checksums.txt","url":"http://%[1]s/checksums"},{"name":"ynab-linux-%[2]s","url":"http://%[1]s/binary"}]}`, request.Host, runtime.GOARCH)
		case "/checksums":
			if request.Header.Get("Accept") != "application/octet-stream" {
				t.Error("wrong asset Accept")
			}
			io.WriteString(response, checksums)
		case "/binary":
			io.WriteString(response, binary)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	previousAPI, previousVersion := releaseAPI, version
	releaseAPI, version = server.URL+"/latest", "0.1.0"
	t.Cleanup(func() { releaseAPI, version = previousAPI, previousVersion })
	t.Setenv(releaseTokenVariable, "test-token")
	return &lookups
}

func checksumLine(binary string) string {
	digest := sha256.Sum256([]byte(binary))
	return hex.EncodeToString(digest[:]) + "  ynab-linux-" + runtime.GOARCH + "\n"
}

func temporaryExecutable(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("self-update requires Linux amd64/arm64")
	}
	executable := filepath.Join(t.TempDir(), "ynab")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestVersionComparison(t *testing.T) {
	for _, test := range []struct {
		candidate, current string
		newer              bool
	}{
		{"0.2.0", "0.1.0", true}, {"0.10.0", "0.9.9", true},
		{"1.0.0", "0.99.99", true}, {"0.2.0", "0.2.0", false},
		{"0.1.9", "0.2.0", false}, {"0.2.0", "dev", true},
		{"", "0.1.0", false}, {"garbage", "0.1.0", false},
		{"1.0.0-rc1", "0.1.0", false}, {"1.-1.0", "0.1.0", false},
		{"01.0.0", "0.1.0", false}, {"+1.0.0", "0.1.0", false},
	} {
		if isNewerVersion(test.candidate, test.current) != test.newer {
			t.Errorf("%+v", test)
		}
	}
}

func TestUpdateReplacesVerifiedBinaryThroughSymlink(t *testing.T) {
	useFakeReleaseServer(t, fakeReleaseBinary, checksumLine(fakeReleaseBinary))
	executable := temporaryExecutable(t)
	link := filepath.Join(t.TempDir(), "ynab-link")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if err := installLatestRelease(context.Background(), link, io.Discard); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(executable)
	if string(content) != fakeReleaseBinary {
		t.Fatalf("got %q", content)
	}
	info, _ := os.Stat(executable)
	if info.Mode().Perm() != 0o755 {
		t.Fatal("wrong executable permissions")
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("replaced symlink rather than its target")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(executable), ".ynab-update-*")); len(leftovers) != 0 {
		t.Fatalf("left temporary files: %v", leftovers)
	}
}

func TestUpdateRejectsInvalidDownloads(t *testing.T) {
	for _, test := range []struct{ name, binary, checksums, message string }{
		{"checksum", fakeReleaseBinary, checksumLine("tampered"), "checksum"},
		{"checksum prefix", fakeReleaseBinary, "prefix" + checksumLine(fakeReleaseBinary), "checksum"},
		{"version", "#!/bin/sh\necho 0.1.5\n", checksumLine("#!/bin/sh\necho 0.1.5\n"), "version check"},
		{"not executable", "not a binary", checksumLine("not a binary"), "version check"},
	} {
		t.Run(test.name, func(t *testing.T) {
			useFakeReleaseServer(t, test.binary, test.checksums)
			executable := temporaryExecutable(t)
			err := installLatestRelease(context.Background(), executable, io.Discard)
			content, _ := os.ReadFile(executable)
			if err == nil || !strings.Contains(err.Error(), test.message) || string(content) != "old" {
				t.Fatalf("got %q, %v", content, err)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(executable), ".ynab-update-*")); len(leftovers) != 0 {
				t.Fatal("left temporary files")
			}
		})
	}
}

func TestUpdateLeavesCurrentReleaseAlone(t *testing.T) {
	useFakeReleaseServer(t, fakeReleaseBinary, checksumLine(fakeReleaseBinary))
	version = "0.2.0"
	executable := temporaryExecutable(t)
	if err := installLatestRelease(context.Background(), executable, io.Discard); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(executable)
	if string(content) != "old" {
		t.Fatal("replaced current version")
	}
}

func TestPublicRepositoryNeedsNoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Error("unexpected token")
		}
		io.WriteString(writer, `{"tag_name":"v0.2.0"}`)
	}))
	defer server.Close()
	updater := Updater{Name: "ynab", Version: "0.2.0", ReleaseURL: server.URL}
	if _, err := updater.Install(context.Background(), temporaryExecutable(t)); err != nil {
		t.Fatal(err)
	}
	updater.Version = "0.1.0"
	if got := updater.Check(context.Background(), filepath.Join(t.TempDir(), "cache"), time.Now()); got != "0.2.0" {
		t.Fatalf("got %q", got)
	}
}

func TestUpdateNoticeCache(t *testing.T) {
	lookups := useFakeReleaseServer(t, fakeReleaseBinary, checksumLine(fakeReleaseBinary))
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	now := time.Now()
	expected := "ynab 0.2.0 is available (you have 0.1.0). Run: ynab update\n"
	for _, moment := range []time.Time{now, now.Add(time.Hour), now.Add(updateCheckInterval)} {
		if notice := updateNotice(context.Background(), cachePath, moment); notice != expected {
			t.Fatalf("got %q", notice)
		}
	}
	if *lookups != 2 {
		t.Fatalf("expected 2 lookups, got %d", *lookups)
	}
	version = "0.2.0"
	if updateNotice(context.Background(), cachePath, now.Add(updateCheckInterval)) != "" {
		t.Fatal("notice for current version")
	}
}

func TestUpdateNoticeSilentForDev(t *testing.T) {
	lookups := useFakeReleaseServer(t, fakeReleaseBinary, checksumLine(fakeReleaseBinary))
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	version = "dev"
	if updateNotice(context.Background(), cachePath, time.Now()) != "" {
		t.Fatal("dev notice")
	}
	if *lookups != 0 {
		t.Fatal("dev build contacted GitHub")
	}
}

func TestReleaseFailuresAndSilentNotice(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		body, message string
	}{
		{"missing release", 404, "test-token", "HTTP 404"},
		{"invalid json", 200, "not json", "invalid GitHub"},
		{"invalid tag", 200, `{"tag_name":"v1.0.0-rc1"}`, "stable vX.Y.Z"},
		{"missing asset", 200, `{"tag_name":"v0.2.0","assets":[]}`, "has no checksums.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			useFakeReleaseServer(t, "", "")
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				io.WriteString(writer, test.body)
			}))
			defer server.Close()
			releaseAPI = server.URL
			err := installLatestRelease(context.Background(), temporaryExecutable(t), io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "test-token") {
				t.Fatalf("got %v", err)
			}
			if test.name != "missing asset" {
				cache := filepath.Join(t.TempDir(), "cache.json")
				if updateNotice(context.Background(), cache, time.Now()) != "" {
					t.Fatal("error notice")
				}
				if _, err := os.Stat(cache); err != nil {
					t.Fatal("failed checks should be cached")
				}
			}
		})
	}
}

func TestGitHubCredentialBoundaries(t *testing.T) {
	useFakeReleaseServer(t, "", "")
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Error("leaked token across redirect")
		}
		io.WriteString(writer, "asset")
	}))
	defer destination.Close()
	if _, err := githubRequest(context.Background(), time.Second, destination.URL, "application/octet-stream"); err == nil {
		t.Fatal("sent token to untrusted asset URL")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()
	releaseAPI = redirect.URL
	body, err := githubRequest(context.Background(), time.Second, releaseAPI, "application/octet-stream")
	if err != nil || string(body) != "asset" {
		t.Fatalf("redirect download: %q, %v", body, err)
	}
}

// These adapters retain the CLI's regression fixtures while exercising the package.
var releaseAPI, version string

const releaseTokenVariable = "SELFUPDATE_TEST_TOKEN"
const updateCheckInterval = CheckInterval

var isNewerVersion = Newer

func testUpdater() Updater {
	return Updater{Name: "ynab", Version: version, Token: os.Getenv(releaseTokenVariable), ReleaseURL: releaseAPI}
}
func installLatestRelease(ctx context.Context, executable string, _ io.Writer) error {
	_, err := testUpdater().Install(ctx, executable)
	return err
}
func updateNotice(ctx context.Context, path string, now time.Time) string {
	latest := testUpdater().Check(ctx, path, now)
	if latest == "" {
		return ""
	}
	return fmt.Sprintf("ynab %s is available (you have %s). Run: ynab update\n", latest, version)
}
func githubRequest(ctx context.Context, timeout time.Duration, address, accept string) ([]byte, error) {
	return testUpdater().request(ctx, timeout, address, accept)
}
