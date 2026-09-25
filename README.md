# go-selfupdate

Self-updates for Linux Go CLIs from GitHub releases. Standard library only; Go 1.23+.

## Usage

```go
import selfupdate "github.com/FiretailHosting/go-selfupdate"

updater := selfupdate.Updater{
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

## Development

```sh
make check
```

MIT licensed.
