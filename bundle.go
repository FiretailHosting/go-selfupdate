package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// elfMagic starts every ELF file. It is the cheapest way to tell a real binary
// from an HTML error page or a build for the wrong platform, and installing
// either of those under Restart=always produces an endless crash loop.
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// apply extracts a bundle, checks it, and swaps it into the install directory.
func (u *Updater) apply(bundle io.Reader) ([]string, error) {
	dir := u.cfg.InstallDir

	staging, err := os.MkdirTemp(dir, ".selfupdate-stage-*")
	if err != nil {
		return nil, fmt.Errorf("cannot stage an update in %s: %w", dir, err)
	}
	defer os.RemoveAll(staging)

	if err := extract(io.LimitReader(bundle, u.maxAssetBytes()), staging); err != nil {
		return nil, err
	}

	if err := u.verify(staging); err != nil {
		return nil, err
	}

	return u.swap(staging)
}

// verify checks the staged payload before anything on disk is touched.
func (u *Updater) verify(staging string) error {
	binary := filepath.Join(staging, u.cfg.BinaryName)

	info, err := os.Stat(binary)
	if err != nil {
		return fmt.Errorf("the release bundle has no %q executable", u.cfg.BinaryName)
	}

	if info.Size() == 0 {
		return fmt.Errorf("the %q executable in the release bundle is empty", u.cfg.BinaryName)
	}

	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer file.Close()

	magic := make([]byte, len(elfMagic))
	if _, err := io.ReadFull(file, magic); err != nil || !bytes.Equal(magic, elfMagic) {
		return fmt.Errorf("the %q file in the release bundle is not a Linux executable", u.cfg.BinaryName)
	}

	if err := os.Chmod(binary, 0o755); err != nil {
		return err
	}

	for name, sentinel := range u.cfg.Dirs {
		if _, err := os.Stat(filepath.Join(staging, name, sentinel)); err != nil {
			return fmt.Errorf("the release bundle has no %s/%s", name, sentinel)
		}
	}

	return nil
}

// swap moves the staged files into place, restoring the previous ones if any
// step fails. Staging and backup both live inside the install directory, so
// every move is a same-filesystem rename: nobody ever sees half a file.
func (u *Updater) swap(staging string) ([]string, error) {
	dir := u.cfg.InstallDir

	backup, err := os.MkdirTemp(dir, ".selfupdate-backup-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(backup)

	names := append([]string{u.cfg.BinaryName}, dirNames(u.cfg.Dirs)...)

	// set the old files aside first, so a rollback has something to put back
	for _, name := range names {
		live := filepath.Join(dir, name)

		if _, err := os.Lstat(live); err != nil {
			continue
		}

		if err := os.Rename(live, filepath.Join(backup, name)); err != nil {
			rollback(dir, backup, names)
			return nil, fmt.Errorf("cannot set %s aside: %w", name, err)
		}
	}

	for _, name := range names {
		if err := os.Rename(filepath.Join(staging, name), filepath.Join(dir, name)); err != nil {
			rollback(dir, backup, names)
			return nil, fmt.Errorf("cannot install %s: %w", name, err)
		}
	}

	return names, nil
}

// rollback puts the previous files back after a failed swap.
func rollback(dir, backup string, names []string) {
	for _, name := range names {
		live := filepath.Join(dir, name)
		saved := filepath.Join(backup, name)

		if _, err := os.Lstat(saved); err != nil {
			continue
		}

		os.RemoveAll(live)
		os.Rename(saved, live)
	}
}

func dirNames(dirs map[string]string) []string {
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}

	// stable order keeps failures reproducible
	sort.Strings(names)

	return names
}

// extract unpacks a gzipped tarball into dest.
func extract(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the release asset is not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot read the release archive: %w", err)
		}

		target, err := safeJoin(dest, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}

		case tar.TypeReg:
			if err := writeEntry(tr, target, header.Mode); err != nil {
				return err
			}

		default:
			// symlinks, hard links and devices have no business in a release
			// bundle, and each is a way out of the staging directory
			continue
		}
	}
}

func writeEntry(src io.Reader, target string, rawMode int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	// .Perm() drops setuid and setgid bits riding along in the header; a zero
	// mode would otherwise produce a file nothing can read
	mode := os.FileMode(rawMode).Perm()
	if mode == 0 {
		mode = 0o644
	}

	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}

	return out.Close()
}

// safeJoin resolves a tar entry inside dest, refusing anything that climbs out.
func safeJoin(dest, name string) (string, error) {
	cleaned := filepath.Clean(filepath.ToSlash(name))

	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("release archive contains an unsafe path: %q", name)
	}

	target := filepath.Join(dest, cleaned)

	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("release archive contains an unsafe path: %q", name)
	}

	return target, nil
}

// CheckBundle verifies a release bundle without installing anything.
//
// Worth running in CI against whatever `make package` produced: it is the only
// thing that catches the packaging and the updater drifting apart, which
// otherwise shows up for the first time on a device, mid-update.
func (u *Updater) CheckBundle(r io.Reader) error {
	staging, err := os.MkdirTemp("", "selfupdate-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if err := extract(io.LimitReader(r, u.maxAssetBytes()), staging); err != nil {
		return err
	}

	return u.verify(staging)
}
