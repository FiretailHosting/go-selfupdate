# go-selfupdate

Install a newer build of the running program from a GitHub release, then hand
the process back to systemd.

Built for services that ship as a tarball holding an executable and some
directories beside it (a built frontend, say), running under a unit with
`Restart=always`. Installing swaps the files atomically and raises `SIGTERM`;
systemd starts the new version.

```go
updater, err := selfupdate.New(selfupdate.Config{
    Owner:          "FiretailHosting",
    Repo:           "my-service",
    Token:          token, // only needed for a private repo
    BinaryName:     "my-service",
    CurrentVersion: version.Version, // stamped with -ldflags
    Dirs:           map[string]string{"frontend": "index.html"},
    ExpectedUnit:   embeddedUnit, // optional, see "Host wiring"
})

result, err := updater.InstallLatest(ctx)
if err != nil {
    return err
}

// answer the request first, then go
selfupdate.Restart(time.Second)
```

## What it does

`InstallLatest` fetches the newest release, refuses anything not newer than
`CurrentVersion`, downloads the asset for this platform, and swaps it in.

Nothing on disk changes unless the whole bundle verifies first:

- the executable exists, is non-empty, and starts with the ELF magic number —
  a 404 page or a wrong-architecture build installed under `Restart=always`
  is an endless crash loop, and this is the cheapest way to catch one
- every directory in `Dirs` contains its named sentinel file

The swap then moves the old files aside and renames the new ones in. Staging
and backup both live inside the install directory, so every move is a
same-filesystem rename and nobody ever observes half a file. If any step fails,
the previous files go back.

Directories are **replaced, not merged**. Merging leaves the last release's
hashed assets lying around forever.

## Safety

- Archive entries that climb out of the staging directory are rejected, and
  symlinks, hard links and devices are skipped rather than written.
- `setuid`/`setgid` bits in tar headers are stripped; a zero mode becomes 0644.
- Downloads are capped (512 MiB by default) and both HTTP clients have timeouts.
- The token is removed when a redirect crosses hosts. GitHub answers an asset
  request with a redirect to signed storage that needs no token and should
  never see one. `net/http` does something similar but only compares
  hostnames, so a same-host different-port hop keeps the header.

## Restarting

`Restart` raises `SIGTERM` rather than calling `os.Exit`, so the program's own
shutdown runs. For anything holding a database, skipping that risks leaving the
write-ahead log unflushed. Call it after the response is on its way out.

`Supervised()` reports whether systemd started the process, which is what makes
"exit and come back on the new build" work at all.

## Host wiring

Self-update replaces the executable and the directories in `Dirs`. It does not
touch the systemd unit, sudoers rules or file permissions — a service that
could rewrite its own systemd configuration could grant itself anything.

So a release that needs new host wiring lands on a machine still running the
old unit, and whatever it added silently does not work. Embed the unit you ship
and pass it as `ExpectedUnit`:

```go
//go:embed my-service.service
var embeddedUnit string
```

`Status()` then reports `UnitStale`, and `Status().Problems()` returns the
message to show: re-run the installer from this release.

`Status()` also reports whether the install directory is writable, and why not
when it isn't. "Permission denied" and "read-only file system" look the same to
an operator but have different fixes, and the second is invisible from a shell
because it comes from the service's own systemd mount namespace.

## Asset naming

The default is `<binary>_<tag>_<goos>_<goarch>.tar.gz`, matching what a
`make package` target usually produces. Override `Config.AssetName` for a
different convention.

## Requirements

Linux with systemd, and a release whose asset is a gzipped tarball with the
executable and the `Dirs` at its root. Go 1.23+.

## License

MIT.
