package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name string
	body string
	mode int64
	dir  bool
	link string
}

func tarball(t *testing.T, entries []entry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		var header *tar.Header

		switch {
		case e.dir:
			header = &tar.Header{Name: e.name, Mode: 0o755, Typeflag: tar.TypeDir}
		case e.link != "":
			header = &tar.Header{Name: e.name, Linkname: e.link, Typeflag: tar.TypeSymlink}
		default:
			header = &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		}

		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}

		if !e.dir && e.link == "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func goodBundle(marker string) []entry {
	return []entry{
		{name: "app", body: "\x7fELF" + marker, mode: 0o755},
		{name: "frontend", dir: true},
		{name: "frontend/index.html", body: "<html>" + marker + "</html>", mode: 0o644},
		{name: "frontend/assets", dir: true},
		{name: "frontend/assets/app.js", body: "console.log('" + marker + "')", mode: 0o644},
	}
}

func testUpdater(t *testing.T, dir string) *Updater {
	t.Helper()

	u, err := New(Config{
		Owner:      "acme",
		Repo:       "app",
		BinaryName: "app",
		InstallDir: dir,
		Dirs:       map[string]string{"frontend": "index.html"},
	})
	if err != nil {
		t.Fatal(err)
	}

	return u
}

// liveInstall builds a directory shaped like a real install.
func liveInstall(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("\x7fELFold"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "frontend", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frontend", "index.html"), []byte("<html>old</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a file only the old build has, to prove the directory is replaced not merged
	if err := os.WriteFile(filepath.Join(dir, "frontend", "assets", "stale.js"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestApplyReplacesBinaryAndDirs(t *testing.T) {
	dir := liveInstall(t)
	u := testUpdater(t, dir)

	replaced, err := u.apply(bytes.NewReader(tarball(t, goodBundle("new"))))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(replaced) != 2 {
		t.Errorf("replaced = %v, want the binary and frontend", replaced)
	}

	binary, err := os.ReadFile(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if string(binary) != "\x7fELFnew" {
		t.Errorf("binary = %q, want the new build", binary)
	}

	index, err := os.ReadFile(filepath.Join(dir, "frontend", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "new") {
		t.Errorf("index.html = %q", index)
	}

	if _, err := os.Stat(filepath.Join(dir, "frontend", "assets", "stale.js")); !os.IsNotExist(err) {
		t.Error("a file from the previous frontend survived the swap")
	}

	assertNoScratch(t, dir)
}

func assertNoScratch(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".selfupdate") {
			t.Errorf("scratch directory %q was left behind", e.Name())
		}
	}
}

func TestApplyRejectsNonExecutable(t *testing.T) {
	dir := liveInstall(t)
	u := testUpdater(t, dir)

	bundle := goodBundle("new")
	bundle[0].body = `{"message":"Not Found"}` // what a bad download looks like

	_, err := u.apply(bytes.NewReader(tarball(t, bundle)))
	if err == nil || !strings.Contains(err.Error(), "not a Linux executable") {
		t.Fatalf("err = %v, want a rejection", err)
	}

	// and the running install must be untouched
	binary, _ := os.ReadFile(filepath.Join(dir, "app"))
	if string(binary) != "\x7fELFold" {
		t.Error("the previous binary was replaced by a rejected bundle")
	}
	assertNoScratch(t, dir)
}

func TestApplyRejectsMissingDir(t *testing.T) {
	dir := liveInstall(t)
	u := testUpdater(t, dir)

	bundle := []entry{{name: "app", body: "\x7fELFnew", mode: 0o755}}

	_, err := u.apply(bytes.NewReader(tarball(t, bundle)))
	if err == nil || !strings.Contains(err.Error(), "frontend/index.html") {
		t.Fatalf("err = %v, want a missing-frontend rejection", err)
	}
}

// A leftover directory where the executable belongs is moved aside like any
// other file, rather than wedging the swap.
func TestSwapHandlesOddExistingPaths(t *testing.T) {
	dir := liveInstall(t)
	u := testUpdater(t, dir)

	if err := os.Remove(filepath.Join(dir, "app")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "app", "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := u.apply(bytes.NewReader(tarball(t, goodBundle("new")))); err != nil {
		t.Fatalf("apply: %v", err)
	}

	binary, err := os.ReadFile(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if string(binary) != "\x7fELFnew" {
		t.Errorf("binary = %q", binary)
	}
	assertNoScratch(t, dir)
}

// rollback is what stands between a failed swap and a box with no working
// install, so it gets tested directly rather than through a contrived failure.
func TestRollbackRestoresEverything(t *testing.T) {
	dir := liveInstall(t)

	backup := filepath.Join(dir, ".selfupdate-backup-test")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}

	names := []string{"app", "frontend"}

	// mimic a swap that moved the old files aside, then installed new ones
	for _, name := range names {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(backup, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("\x7fELFhalf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "frontend"), 0o755); err != nil {
		t.Fatal(err)
	}

	rollback(dir, backup, names)

	binary, err := os.ReadFile(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatalf("the previous binary was not restored: %v", err)
	}
	if string(binary) != "\x7fELFold" {
		t.Errorf("binary = %q, want the previous build", binary)
	}

	index, err := os.ReadFile(filepath.Join(dir, "frontend", "index.html"))
	if err != nil {
		t.Fatalf("the previous frontend was not restored: %v", err)
	}
	if !strings.Contains(string(index), "old") {
		t.Errorf("index.html = %q", index)
	}
	if _, err := os.Stat(filepath.Join(dir, "frontend", "assets", "stale.js")); err != nil {
		t.Error("the restored frontend is missing files")
	}
}

// Rolling back a first install means removing what was put there, since there
// was nothing to put back.
func TestRollbackOnFirstInstall(t *testing.T) {
	dir := t.TempDir()

	backup := filepath.Join(dir, ".selfupdate-backup-test")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("\x7fELFnew"), 0o755); err != nil {
		t.Fatal(err)
	}

	rollback(dir, backup, []string{"app", "frontend"})

	// nothing was saved, so nothing is restored; the half-installed file stays
	// for the caller's error to explain rather than vanishing silently
	if _, err := os.Stat(filepath.Join(dir, "app")); err != nil {
		t.Errorf("rollback removed a file it never backed up: %v", err)
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../escaped", "../../etc/passwd", "/etc/passwd", "frontend/../../escaped"} {
		err := extract(bytes.NewReader(tarball(t, []entry{{name: name, body: "x", mode: 0o644}})), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "unsafe path") {
			t.Errorf("extract(%q) err = %v, want an unsafe-path rejection", name, err)
		}
	}
}

// A symlink in the archive is another way out of the staging directory.
func TestExtractSkipsSymlinks(t *testing.T) {
	dest := t.TempDir()

	err := extract(bytes.NewReader(tarball(t, []entry{
		{name: "app", body: "\x7fELFnew", mode: 0o755},
		{name: "escape", link: "/etc/passwd"},
	})), dest)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(dest, "escape")); !os.IsNotExist(err) {
		t.Error("a symlink was extracted")
	}
}

func TestExtractStripsSetuidAndFixesZeroMode(t *testing.T) {
	dest := t.TempDir()

	if err := extract(bytes.NewReader(tarball(t, []entry{
		{name: "suid", body: "x", mode: 0o4755},
		{name: "zero", body: "x", mode: 0},
	})), dest); err != nil {
		t.Fatal(err)
	}

	suid, err := os.Stat(filepath.Join(dest, "suid"))
	if err != nil {
		t.Fatal(err)
	}
	if suid.Mode()&os.ModeSetuid != 0 {
		t.Error("setuid survived extraction")
	}

	zero, err := os.Stat(filepath.Join(dest, "zero"))
	if err != nil {
		t.Fatal(err)
	}
	if zero.Mode().Perm() == 0 {
		t.Error("a zero mode produced an unreadable file")
	}
}

func TestExtractRejectsNonGzip(t *testing.T) {
	if err := extract(strings.NewReader("definitely not gzip"), t.TempDir()); err == nil {
		t.Error("extract accepted a non-gzip payload")
	}
}

func TestCheckBundle(t *testing.T) {
	u := testUpdater(t, t.TempDir())

	if err := u.CheckBundle(bytes.NewReader(tarball(t, goodBundle("new")))); err != nil {
		t.Errorf("CheckBundle rejected a good bundle: %v", err)
	}

	bad := goodBundle("new")
	bad[0].body = "<html>404</html>"

	if err := u.CheckBundle(bytes.NewReader(tarball(t, bad))); err == nil {
		t.Error("CheckBundle accepted a bundle whose binary is an error page")
	}
}
