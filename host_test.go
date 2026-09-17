package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleUnit = `[Unit]
Description=App

[Service]
ExecStart=/opt/app/app serve
Restart=always
ReadWritePaths=/opt/app /var/lib/app

[Install]
WantedBy=multi-user.target
`

func unitUpdater(t *testing.T, expected, installed string) *Updater {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.service")

	if installed != "" {
		if err := os.WriteFile(path, []byte(installed), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	u, err := New(Config{
		Owner:        "acme",
		Repo:         "app",
		BinaryName:   "app",
		InstallDir:   dir,
		ExpectedUnit: expected,
		UnitPath:     path,
	})
	if err != nil {
		t.Fatal(err)
	}

	return u
}

func TestUnitStateMatching(t *testing.T) {
	if got := unitUpdater(t, sampleUnit, sampleUnit).UnitState(); got != UnitCurrent {
		t.Errorf("identical units = %v, want UnitCurrent", got)
	}

	// comments and blank lines are cosmetic
	reformatted := "# a note\n\n" + sampleUnit + "\n\n"
	if got := unitUpdater(t, sampleUnit, reformatted).UnitState(); got != UnitCurrent {
		t.Errorf("reformatted unit = %v, want UnitCurrent", got)
	}

	changed := sampleUnit + "ReadWritePaths=/etc/somewhere-new\n"
	if got := unitUpdater(t, changed, sampleUnit).UnitState(); got != UnitStale {
		t.Errorf("changed unit = %v, want UnitStale", got)
	}

	// nothing installed, or nothing expected: nothing to say
	if got := unitUpdater(t, sampleUnit, "").UnitState(); got != UnitUnknown {
		t.Errorf("no installed unit = %v, want UnitUnknown", got)
	}
	if got := unitUpdater(t, "", sampleUnit).UnitState(); got != UnitUnknown {
		t.Errorf("no expected unit = %v, want UnitUnknown", got)
	}
}

func TestStatusReportsProblems(t *testing.T) {
	u := unitUpdater(t, sampleUnit+"Extra=1\n", sampleUnit)

	status := u.Status()
	if !status.Writable {
		t.Errorf("a temp dir reported unwritable: %s", status.WriteProblem)
	}

	problems := status.Problems()
	if len(problems) == 0 {
		t.Fatal("no problems reported for a stale unit")
	}
	if problems[0] != StaleUnitProblem {
		t.Errorf("first problem = %q, want the stale unit notice", problems[0])
	}
}

func TestStatusOnMissingDir(t *testing.T) {
	u, err := New(Config{
		Owner: "acme", Repo: "app", BinaryName: "app",
		InstallDir: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err != nil {
		t.Fatal(err)
	}

	status := u.Status()
	if status.Writable {
		t.Error("a missing directory reported writable")
	}
	if status.CanInstall() {
		t.Error("CanInstall true with an unwritable directory")
	}
}
