package selfupdate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancellationAndVersionTimeoutPreserveExecutable(t *testing.T) {
	for _, test := range []struct {
		name, binary string
		cancel       bool
	}{
		{"cancelled", fakeReleaseBinary, true},
		{"version timeout", "#!/bin/sh\nexec sleep 30\n", false},
		{"child holds output open", "#!/bin/sh\necho 0.2.0\nsleep 30 &\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			useFakeReleaseServer(t, test.binary, checksumLine(test.binary))
			executable := temporaryExecutable(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				cancel()
			}
			started := time.Now()
			_, err := testUpdater().Install(ctx, executable)
			if err == nil {
				t.Fatal("expected failure")
			}
			if time.Since(started) > 10*time.Second {
				t.Fatal("version subprocess did not time out promptly")
			}
			content, err := os.ReadFile(executable)
			if err != nil || string(content) != "old" {
				t.Fatalf("original changed: %q, %v", content, err)
			}
			leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(executable), ".ynab-update-*"))
			if len(leftovers) != 0 {
				t.Fatal("staging file not removed")
			}
		})
	}
}

func TestCacheRecoversFromCorruptionAndClockRollback(t *testing.T) {
	for _, content := range []string{"not json", `{"checkedAt":"2999-01-01T00:00:00Z","latestVersion":"9.0.0"}`} {
		lookups := useFakeReleaseServer(t, "", "")
		path := filepath.Join(t.TempDir(), "cache.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := testUpdater().Check(context.Background(), path, time.Now()); got != "0.2.0" || *lookups != 1 {
			t.Fatalf("got %q, %d lookups", got, *lookups)
		}
	}
}

func TestFailedChecksAreNotRepeatedWithinInterval(t *testing.T) {
	lookups := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lookups++
		http.Error(writer, "private response", http.StatusForbidden)
	}))
	defer server.Close()
	updater := Updater{Name: "test", Version: "1.0.0", ReleaseURL: server.URL}
	cache := filepath.Join(t.TempDir(), "cache")
	now := time.Now()
	for _, moment := range []time.Time{now, now.Add(time.Hour)} {
		if got := updater.Check(context.Background(), cache, moment); got != "" {
			t.Fatalf("got %q", got)
		}
	}
	if lookups != 1 {
		t.Fatalf("got %d lookups", lookups)
	}
}

func TestRejectsDraftAndPrerelease(t *testing.T) {
	for _, flag := range []string{"draft", "prerelease"} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			io.WriteString(writer, `{"tag_name":"v1.0.0","`+flag+`":true}`)
		}))
		updater := Updater{ReleaseURL: server.URL}
		_, err := updater.latest(context.Background(), time.Second)
		server.Close()
		if err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}

func TestHTTPSRedirectCannotDowngrade(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("followed insecure redirect")
	}))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = origin.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	updater := Updater{ReleaseURL: origin.URL, Token: "secret-test-token"}
	_, err := updater.request(context.Background(), time.Second, origin.URL, "application/octet-stream")
	if err == nil || strings.Contains(err.Error(), updater.Token) {
		t.Fatalf("got %v", err)
	}
}

func TestDownloadLimit(t *testing.T) {
	// Exercise the same bounded reader without allocating a 128 MiB fixture
	// (race instrumentation makes that unnecessarily expensive in CI).
	const limit = 1024
	for _, size := range []int{limit - 1, limit, limit + 1} {
		body, err := readResponse(strings.NewReader(strings.Repeat("x", size)), limit)
		if size <= limit {
			if err != nil || len(body) != size {
				t.Fatalf("size %d: got %d bytes, %v", size, len(body), err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("got %v", err)
		}
	}
}
