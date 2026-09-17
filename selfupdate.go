// Package selfupdate installs a newer build of the running program from a
// GitHub release, then hands the process back to its supervisor.
//
// It is built for a service that ships as a tarball containing an executable
// and some directories beside it (a built frontend, say), installed under a
// systemd unit with Restart=always. Installing swaps the files atomically and
// raises SIGTERM; systemd starts the new version.
//
// The package deliberately does not manage the systemd unit, sudoers rules or
// directory permissions. A service has no business granting itself privileges,
// so those stay with whatever installer put the service there. UnitState exists
// to notice when that wiring has fallen behind the running build.
package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Config describes the program being updated and where its releases live.
type Config struct {
	// Owner and Repo identify the GitHub repository holding the releases.
	Owner string
	Repo  string
	// Token authenticates against a private repository. Public repos need none.
	Token string

	// BinaryName is the executable's name, both inside the release tarball and
	// in the install directory.
	BinaryName string

	// CurrentVersion is the running build's version, normally stamped in with
	// -ldflags. "dev" or empty counts as older than any release.
	CurrentVersion string

	// Dirs are directories in the tarball that replace their counterparts in the
	// install directory wholesale. Each value names a file that must exist
	// inside, as a sanity check against a half-built bundle:
	//
	//	Dirs: map[string]string{"frontend": "index.html"}
	//
	// Replacing rather than merging matters: a merge leaves the previous
	// release's hashed assets behind forever.
	Dirs map[string]string

	// AssetName builds the release asset's file name from a tag. It defaults to
	// DefaultAssetName, which matches <binary>_<tag>_<goos>_<goarch>.tar.gz.
	AssetName func(tag string) string

	// InstallDir defaults to the directory holding the running executable.
	InstallDir string

	// MaxAssetBytes caps the download. Defaults to 512 MiB.
	MaxAssetBytes int64

	// ExpectedUnit is the systemd unit this build ships, normally embedded with
	// go:embed. When set, Status reports whether the installed unit matches.
	ExpectedUnit string

	// UnitPath defaults to /etc/systemd/system/<BinaryName>.service.
	UnitPath string

	// APIBase overrides the GitHub API root, for tests.
	APIBase string

	// UserAgent is sent with every request. Defaults to the binary name.
	UserAgent string

	// APITimeout and DownloadTimeout default to 30 seconds and 30 minutes.
	APITimeout      time.Duration
	DownloadTimeout time.Duration

	// HTTPTransport is shared by both clients when set.
	HTTPTransport http.RoundTripper
}

// Updater installs releases of one program.
type Updater struct {
	cfg Config
}

// New validates the configuration and returns an Updater.
func New(cfg Config) (*Updater, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, fmt.Errorf("selfupdate: Owner and Repo are required")
	}

	if cfg.BinaryName == "" {
		return nil, fmt.Errorf("selfupdate: BinaryName is required")
	}

	if cfg.InstallDir == "" {
		dir, err := InstallDir()
		if err != nil {
			return nil, err
		}
		cfg.InstallDir = dir
	}

	return &Updater{cfg: cfg}, nil
}

// InstallDir returns the directory holding the running executable, with
// symlinks resolved so an update replaces the real file rather than a link.
func InstallDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("selfupdate: cannot locate the running executable: %w", err)
	}

	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	return filepath.Dir(exe), nil
}

// DefaultAssetName matches the convention `make package` produces:
// <binary>_<tag>_<goos>_<goarch>.tar.gz.
func DefaultAssetName(binary, tag string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", binary, tag, runtime.GOOS, runtime.GOARCH)
}

// AssetNameFor is the asset this build looks for in a given release.
func (u *Updater) AssetNameFor(tag string) string {
	if u.cfg.AssetName != nil {
		return u.cfg.AssetName(tag)
	}

	return DefaultAssetName(u.cfg.BinaryName, tag)
}

// Available reports the latest release and whether it is newer than this build.
func (u *Updater) Available(ctx context.Context) (Release, bool, error) {
	release, err := u.Latest(ctx)
	if err != nil {
		return Release{}, false, err
	}

	return release, Newer(release.TagName, u.cfg.CurrentVersion), nil
}

// Result describes a completed install.
type Result struct {
	// Version is the tag that was installed.
	Version string
	// Asset is the release asset it came from.
	Asset string
	// Replaced lists the paths swapped in the install directory.
	Replaced []string
}

// InstallLatest downloads the newest release and swaps it into place.
//
// It does not restart anything: the caller answers its HTTP request or logs a
// line first, then calls Restart. Nothing is changed on disk unless the whole
// bundle verifies, and a failure part-way through restores the previous files.
func (u *Updater) InstallLatest(ctx context.Context) (Result, error) {
	release, newer, err := u.Available(ctx)
	if err != nil {
		return Result{}, err
	}

	if !newer {
		return Result{}, fmt.Errorf("release %s is not newer than the running version %s",
			release.TagName, u.version())
	}

	return u.Install(ctx, release)
}

// Install downloads a specific release and swaps it into place.
func (u *Updater) Install(ctx context.Context, release Release) (Result, error) {
	name := u.AssetNameFor(release.TagName)

	asset, found := release.FindAsset(name)
	if !found {
		return Result{}, fmt.Errorf("release %s has no asset named %q (it has %s)",
			release.TagName, name, assetNames(release))
	}

	body, err := u.download(ctx, asset)
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	replaced, err := u.apply(body)
	if err != nil {
		return Result{}, err
	}

	return Result{Version: release.TagName, Asset: asset.Name, Replaced: replaced}, nil
}

func assetNames(release Release) string {
	if len(release.Assets) == 0 {
		return "no assets"
	}

	names := make([]string, 0, len(release.Assets))
	for _, asset := range release.Assets {
		names = append(names, asset.Name)
	}

	return fmt.Sprintf("%v", names)
}

func (u *Updater) version() string {
	if u.cfg.CurrentVersion == "" {
		return "dev"
	}

	return u.cfg.CurrentVersion
}

func (u *Updater) apiBase() string {
	if u.cfg.APIBase != "" {
		return u.cfg.APIBase
	}

	return defaultAPIBase
}

func (u *Updater) userAgent() string {
	if u.cfg.UserAgent != "" {
		return u.cfg.UserAgent
	}

	return u.cfg.BinaryName + "-selfupdate"
}

func (u *Updater) maxAssetBytes() int64 {
	if u.cfg.MaxAssetBytes > 0 {
		return u.cfg.MaxAssetBytes
	}

	return 512 << 20
}

func (u *Updater) apiClient() *http.Client {
	timeout := u.cfg.APITimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &http.Client{Timeout: timeout, Transport: u.cfg.HTTPTransport}
}

func (u *Updater) downloadClient() *http.Client {
	timeout := u.cfg.DownloadTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	return &http.Client{
		Timeout:       timeout,
		Transport:     u.cfg.HTTPTransport,
		CheckRedirect: stripAuthOnHostChange,
	}
}

// stripAuthOnHostChange drops the token when a redirect crosses hosts.
//
// GitHub answers an asset request with a redirect to signed storage, which
// needs no token and should never see one. net/http already does something
// like this, but only compares hostnames, so a same-host different-port hop
// keeps the header. Comparing host and port makes the guarantee ours rather
// than inherited.
func stripAuthOnHostChange(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}

	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}

	return nil
}
