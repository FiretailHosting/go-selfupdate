package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub serves one release and its asset.
type fakeGitHub struct {
	release    Release
	assetBody  []byte
	assetStore *httptest.Server

	// recorded from the last request
	gotAuth      string
	gotAccept    string
	gotUserAgent string
	storeAuth    string
}

func newFakeGitHub(t *testing.T, tag string, assetBody []byte) *fakeGitHub {
	t.Helper()

	fake := &fakeGitHub{assetBody: assetBody}

	// asset bytes live on a separate host, the way GitHub redirects to storage
	fake.assetStore = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.storeAuth = r.Header.Get("Authorization")
		w.Write(fake.assetBody)
	}))
	t.Cleanup(fake.assetStore.Close)

	fake.release = Release{
		TagName: tag,
		Assets:  []Asset{{ID: 7, Name: DefaultAssetName("app", tag), Size: int64(len(assetBody))}},
	}

	return fake
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		f.gotAccept = r.Header.Get("Accept")
		f.gotUserAgent = r.Header.Get("User-Agent")

		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			json.NewEncoder(w).Encode(f.release)

		case strings.Contains(r.URL.Path, "/releases/assets/"):
			http.Redirect(w, r, f.assetStore.URL+"/bundle.tar.gz", http.StatusFound)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func updaterFor(t *testing.T, apiBase, installDir, version, token string) *Updater {
	t.Helper()

	u, err := New(Config{
		Owner:          "acme",
		Repo:           "app",
		Token:          token,
		BinaryName:     "app",
		CurrentVersion: version,
		InstallDir:     installDir,
		Dirs:           map[string]string{"frontend": "index.html"},
		APIBase:        apiBase,
	})
	if err != nil {
		t.Fatal(err)
	}

	return u
}

func TestLatestSendsAuthAndHeaders(t *testing.T) {
	fake := newFakeGitHub(t, "v1.2.0", nil)
	server := fake.start(t)

	u := updaterFor(t, server.URL, t.TempDir(), "v1.0.0", "secret-token")

	release, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if release.TagName != "v1.2.0" {
		t.Errorf("tag = %q", release.TagName)
	}
	if fake.gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q", fake.gotAuth)
	}
	if fake.gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", fake.gotAccept)
	}
	if fake.gotUserAgent == "" {
		t.Error("no User-Agent sent; GitHub rejects requests without one")
	}
}

func TestAvailableComparesVersions(t *testing.T) {
	fake := newFakeGitHub(t, "v1.2.0", nil)
	server := fake.start(t)

	for _, c := range []struct {
		current string
		want    bool
	}{{"v1.1.0", true}, {"v1.2.0", false}, {"v1.3.0", false}, {"dev", true}} {
		u := updaterFor(t, server.URL, t.TempDir(), c.current, "")

		_, newer, err := u.Available(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if newer != c.want {
			t.Errorf("running %s: newer = %v, want %v", c.current, newer, c.want)
		}
	}
}

func TestGitHubErrorsAreActionable(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized: "rejected the token",
		http.StatusForbidden:    "read access",
		http.StatusNotFound:     "check the owner and repo",
	}

	for status, want := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", status)
		}))

		u := updaterFor(t, server.URL, t.TempDir(), "v1.0.0", "t")

		_, err := u.Latest(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: err = %v, want it to mention %q", status, err, want)
		}

		server.Close()
	}
}

func TestRateLimitIsCalledOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, "limit", http.StatusForbidden)
	}))
	defer server.Close()

	u := updaterFor(t, server.URL, t.TempDir(), "v1.0.0", "t")

	_, err := u.Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v, want a rate limit message", err)
	}
}

// End to end over HTTP: fetch the release, follow the redirect to storage,
// unpack, and swap. This is the whole path a device runs.
func TestInstallLatestEndToEnd(t *testing.T) {
	dir := liveInstall(t)
	bundle := tarball(t, goodBundle("new"))

	fake := newFakeGitHub(t, "v2.0.0", bundle)
	server := fake.start(t)

	u := updaterFor(t, server.URL, dir, "v1.0.0", "secret-token")

	result, err := u.InstallLatest(context.Background())
	if err != nil {
		t.Fatalf("InstallLatest: %v", err)
	}

	if result.Version != "v2.0.0" {
		t.Errorf("version = %q", result.Version)
	}
	if result.Asset != DefaultAssetName("app", "v2.0.0") {
		t.Errorf("asset = %q", result.Asset)
	}

	binary, err := os.ReadFile(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary, []byte("\x7fELFnew")) {
		t.Errorf("binary = %q, want the new build", binary)
	}

	// The token must not follow the redirect to storage; that URL is already
	// signed, and sending it there leaks the token to another host.
	if fake.storeAuth != "" {
		t.Errorf("Authorization leaked to the asset host: %q", fake.storeAuth)
	}

	assertNoScratch(t, dir)
}

func TestInstallLatestRefusesOlderRelease(t *testing.T) {
	fake := newFakeGitHub(t, "v1.0.0", nil)
	server := fake.start(t)

	u := updaterFor(t, server.URL, liveInstall(t), "v2.0.0", "")

	if _, err := u.InstallLatest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not newer") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

func TestInstallReportsMissingAsset(t *testing.T) {
	fake := newFakeGitHub(t, "v2.0.0", nil)
	fake.release.Assets = []Asset{{ID: 1, Name: "something-else.tar.gz"}}
	server := fake.start(t)

	u := updaterFor(t, server.URL, liveInstall(t), "v1.0.0", "")

	_, err := u.InstallLatest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "something-else.tar.gz") {
		t.Errorf("err = %v, want it to list what the release does have", err)
	}
}

// An asset far larger than any real release is refused before it lands on a
// device's disk.
func TestOversizedAssetIsRefused(t *testing.T) {
	fake := newFakeGitHub(t, "v2.0.0", nil)
	fake.release.Assets = []Asset{{ID: 1, Name: DefaultAssetName("app", "v2.0.0"), Size: 1 << 40}}
	server := fake.start(t)

	u := updaterFor(t, server.URL, liveInstall(t), "v1.0.0", "")

	_, err := u.InstallLatest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want a size refusal", err)
	}
}

func TestNewRequiresIdentity(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no owner":  {Repo: "app", BinaryName: "app", InstallDir: "/tmp"},
		"no repo":   {Owner: "acme", BinaryName: "app", InstallDir: "/tmp"},
		"no binary": {Owner: "acme", Repo: "app", InstallDir: "/tmp"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}

func TestDefaultAssetName(t *testing.T) {
	got := DefaultAssetName("cms", "v1.2.3")
	want := fmt.Sprintf("cms_v1.2.3_%s_%s.tar.gz", goos(), goarch())
	if got != want {
		t.Errorf("DefaultAssetName = %q, want %q", got, want)
	}
}
