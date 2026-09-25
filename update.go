// Package selfupdate updates Linux CLI executables from stable GitHub releases.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Updater describes a CLI whose releases contain <Name>-linux-<arch> and
// checksums.txt. The executable must print its bare X.Y.Z version for --version.
// Tokens are optional for public repositories. Callers own environment lookup,
// command selection, and output; this package never restarts the program.
type Updater struct {
	Owner, Repo   string
	Name, Version string
	Token         string
	// ReleaseURL overrides the latest-release endpoint, primarily for tests.
	ReleaseURL string
}

const CheckInterval = 24 * time.Hour
const maxDownload = 128 << 20

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

func parseVersion(text string) ([3]uint64, bool) {
	var numbers [3]uint64
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return numbers, false
	}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return [3]uint64{}, false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return [3]uint64{}, false
			}
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return [3]uint64{}, false
		}
		numbers[index] = number
	}
	return numbers, true
}

// Newer compares stable X.Y.Z versions. Invalid candidates are never newer;
// an invalid current version (including dev) compares as 0.0.0.
func Newer(candidate, current string) bool {
	candidateNumbers, valid := parseVersion(candidate)
	if !valid {
		return false
	}
	currentNumbers, _ := parseVersion(current)
	for index := range candidateNumbers {
		if candidateNumbers[index] != currentNumbers[index] {
			return candidateNumbers[index] > currentNumbers[index]
		}
	}
	return false
}

func (updater Updater) endpoint() string {
	if updater.ReleaseURL != "" {
		return updater.ReleaseURL
	}
	return "https://api.github.com/repos/" + url.PathEscape(updater.Owner) + "/" + url.PathEscape(updater.Repo) + "/releases/latest"
}

func (updater Updater) request(ctx context.Context, timeout time.Duration, address, accept string) ([]byte, error) {
	target, err := url.Parse(address)
	origin, originError := url.Parse(updater.endpoint())
	if err != nil || originError != nil || target.Scheme != origin.Scheme || target.Host != origin.Host || target.User != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, errors.New("invalid GitHub release URL")
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("could not construct GitHub request")
	}
	if updater.Token != "" {
		request.Header.Set("Authorization", "Bearer "+updater.Token)
	}
	request.Header.Set("Accept", accept)
	client := &http.Client{CheckRedirect: func(request *http.Request, previous []*http.Request) error {
		if len(previous) >= 10 {
			return errors.New("too many redirects")
		}
		if request.URL.User != nil || (request.URL.Scheme != "https" && request.URL.Scheme != "http") || (origin.Scheme == "https" && request.URL.Scheme != "https") {
			return errors.New("insecure redirect")
		}
		if request.URL.Host != origin.Host {
			request.Header.Del("Authorization")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("GitHub request failed; check connectivity and token configuration")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub request failed: HTTP %d", response.StatusCode)
	}
	return readResponse(response.Body, maxDownload)
}

func readResponse(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, errors.New("could not read GitHub response")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("GitHub release download exceeds size limit")
	}
	return body, nil
}

func (updater Updater) latest(ctx context.Context, timeout time.Duration) (release, error) {
	var latest release
	body, err := updater.request(ctx, timeout, updater.endpoint(), "application/vnd.github+json")
	if err != nil {
		return latest, err
	}
	if json.Unmarshal(body, &latest) != nil {
		return latest, errors.New("invalid GitHub release response")
	}
	if _, valid := parseVersion(strings.TrimPrefix(latest.Tag, "v")); !valid || !strings.HasPrefix(latest.Tag, "v") || latest.Draft || latest.Prerelease {
		return latest, errors.New("release must have a stable vX.Y.Z tag")
	}
	return latest, nil
}

func (updater Updater) asset(ctx context.Context, latest release, name string) ([]byte, error) {
	for _, asset := range latest.Assets {
		if asset.Name == name {
			return updater.request(ctx, 5*time.Minute, asset.URL, "application/octet-stream")
		}
	}
	return nil, fmt.Errorf("release %s has no %s", latest.Tag, name)
}

// Install installs the latest newer release at executablePath, resolving symlinks.
// It verifies SHA-256 and --version before an atomic rename. A checksum protects
// against corruption, not a compromised release publisher. Returns the installed
// version, or the current version if already up to date.
func (updater Updater) Install(ctx context.Context, executablePath string) (string, error) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", errors.New("self-update supports Linux amd64/arm64 only")
	}
	if updater.Name == "" || strings.ContainsAny(updater.Name, "/\\") {
		return "", errors.New("invalid binary name")
	}
	latest, err := updater.latest(ctx, 30*time.Second)
	if err != nil {
		return "", err
	}
	version := strings.TrimPrefix(latest.Tag, "v")
	if !Newer(version, updater.Version) {
		return updater.Version, nil
	}
	binaryName := updater.Name + "-linux-" + runtime.GOARCH
	checksums, err := updater.asset(ctx, latest, "checksums.txt")
	if err != nil {
		return "", err
	}
	binary, err := updater.asset(ctx, latest, binaryName)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(binary)
	matched := false
	for _, line := range strings.Split(string(checksums), "\n") {
		if line == hex.EncodeToString(digest[:])+"  "+binaryName {
			matched = true
		}
	}
	if !matched {
		return "", errors.New("downloaded binary does not match release checksum")
	}
	executablePath, err = filepath.EvalSymlinks(executablePath)
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(executablePath), "."+updater.Name+"-update-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.Write(binary)
	err = errors.Join(err, temporary.Chmod(0o755), temporary.Sync(), temporary.Close())
	if err != nil {
		return "", err
	}
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	versionCommand := exec.CommandContext(checkContext, temporary.Name(), "--version")
	// Stop waiting for output pipes held open by children after the binary exits.
	versionCommand.WaitDelay = time.Second
	reportedVersion, err := versionCommand.Output()
	if err != nil || strings.TrimSpace(string(reportedVersion)) != version {
		return "", errors.New("downloaded binary failed its version check")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(temporary.Name(), executablePath); err != nil {
		return "", err
	}
	return version, nil
}

// CachePath returns the OS cache path for this CLI, or empty if unavailable.
func (updater Updater) CachePath() string {
	folder, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(folder, updater.Name, "update-check.json")
}

// Check returns a newer version, or empty. Checks and failures are cached for
// 24 hours; errors are silent. Callers decide when a notice is appropriate.
// Separate concurrent processes may each perform a check on a cache miss.
func (updater Updater) Check(ctx context.Context, cachePath string, now time.Time) string {
	if _, valid := parseVersion(updater.Version); !valid || cachePath == "" {
		return ""
	}
	var cache struct {
		CheckedAt     time.Time `json:"checkedAt"`
		LatestVersion string    `json:"latestVersion"`
	}
	if content, err := os.ReadFile(cachePath); err != nil || json.Unmarshal(content, &cache) != nil || now.Before(cache.CheckedAt) || now.Sub(cache.CheckedAt) >= CheckInterval {
		latest, err := updater.latest(ctx, 3*time.Second)
		cache.CheckedAt, cache.LatestVersion = now, strings.TrimPrefix(latest.Tag, "v")
		if err != nil {
			cache.LatestVersion = ""
		}
		if os.MkdirAll(filepath.Dir(cachePath), 0o700) == nil {
			content, _ := json.Marshal(cache)
			_ = os.WriteFile(cachePath, content, 0o600)
		}
	}
	if !Newer(cache.LatestVersion, updater.Version) {
		return ""
	}
	return cache.LatestVersion
}
