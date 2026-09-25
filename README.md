# go-selfupdate

Go self-updates from GitHub releases. Standard library only; Go 1.23+.

## Standalone CLIs

```go
import "github.com/FiretailHosting/go-selfupdate/binary"

updater := binary.Updater{
    Owner: "FiretailHosting", Repo: "my-cli",
    Name: "my-cli", Version: version,
    Token: token, // optional for public repositories
}
installedVersion, err := updater.Install(ctx, executablePath)
```

Supports Linux amd64/arm64. Publish stable `vX.Y.Z` releases containing
`<name>-linux-<arch>` and `checksums.txt` in `sha256sum` format. The binary's
`--version` must print `X.Y.Z`.

Installs verify SHA-256 and version, then atomically replace the executable,
resolving symlinks. Downloads are bounded; credentials stay on the API origin.
Checksums do not protect against a compromised release publisher.

`updater.Check(ctx, updater.CachePath(), time.Now())` returns a newer version
or empty, silently caching checks and failures for 24 hours. Call it only when
a notice is appropriate; the caller handles output and token lookup.

## Service bundles

The root `selfupdate` package supports tarball-based Linux services:
`New(Config{...})`, `InstallLatest(ctx)`, and optional `Restart(delay)`.
Default assets are `<binary>_<tag>_<goos>_<goarch>.tar.gz`. `Dirs` configures
companion directories; `Status()` checks host wiring. It does not manage systemd
units or permissions. Bundle installs validate contents, not CLI checksums.

## Development

```sh
make check
```

MIT licensed.
