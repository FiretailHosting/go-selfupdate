package selfupdate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"
)

// Status is what the host allows right now. A UI can use it to explain why
// updating is unavailable instead of failing when the button is pressed.
type Status struct {
	InstallDir string
	// Writable is false when the install directory cannot be written.
	Writable bool
	// WriteProblem explains why, when it cannot.
	WriteProblem string
	// Supervised is true when systemd started this process, which is what makes
	// "exit and come back on the new build" work.
	Supervised bool
	// Unit reports whether the installed systemd unit matches this build, when
	// Config.ExpectedUnit is set.
	Unit UnitState
}

// Problems lists everything standing between this host and a working update.
func (s Status) Problems() []string {
	var out []string

	if s.Unit == UnitStale {
		out = append(out, StaleUnitProblem)
	}

	if !s.Writable {
		out = append(out, s.WriteProblem)
	}

	if !s.Supervised {
		out = append(out, "This process was not started by systemd, so it cannot restart itself after updating.")
	}

	return out
}

// CanInstall reports whether an update would work here.
func (s Status) CanInstall() bool { return s.Writable && s.Supervised }

// Status inspects the host.
func (u *Updater) Status() Status {
	writable, problem := writable(u.cfg.InstallDir)

	return Status{
		InstallDir:   u.cfg.InstallDir,
		Writable:     writable,
		WriteProblem: problem,
		Supervised:   Supervised(),
		Unit:         u.UnitState(),
	}
}

// writable reports whether a directory can be written, and why not.
//
// The reason matters: "permission denied" and "read-only file system" look the
// same to a user but have different fixes, and the second one is invisible from
// a shell because it comes from this service's own systemd mount namespace.
func writable(dir string) (bool, string) {
	file, err := os.CreateTemp(dir, ".selfupdate-probe-*")
	if err == nil {
		name := file.Name()
		file.Close()
		os.Remove(name)

		return true, ""
	}

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, fmt.Sprintf("%s does not exist.", dir)

	case errors.Is(err, syscall.EROFS):
		return false, fmt.Sprintf("%s is read-only for this service. Its systemd unit does not list "+
			"the path in ReadWritePaths; re-run the installer to update the unit.", dir)

	case errors.Is(err, fs.ErrPermission):
		return false, fmt.Sprintf("This service's user cannot write to %s. Re-run the installer "+
			"to grant access, or check the directory's group and mode.", dir)

	default:
		return false, fmt.Sprintf("%s cannot be written: %v", dir, err)
	}
}

// Supervised reports whether systemd started this process.
func Supervised() bool {
	return os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != ""
}

// Restart hands the process back to its supervisor after the given delay.
//
// It raises SIGTERM rather than exiting, so the program's own shutdown runs:
// for anything holding a database, skipping that risks leaving the write-ahead
// log unflushed. Call it after the response is on its way out.
//
// It returns immediately; the signal is raised from a goroutine.
func Restart(delay time.Duration) {
	go func() {
		time.Sleep(delay)

		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			// nothing left to try; the supervisor restarts us either way
			os.Exit(0)
		}
	}()
}

// UnitState says whether the installed systemd unit matches this build.
type UnitState int

const (
	// UnitUnknown means there was nothing to compare: no expected unit
	// configured, or no unit installed. This is the normal case in development.
	UnitUnknown UnitState = iota
	// UnitCurrent means the installed unit matches this build.
	UnitCurrent
	// UnitStale means this build expects a different unit from the installed one.
	UnitStale
)

// StaleUnitProblem is the advice to show when the installed unit is out of date.
const StaleUnitProblem = "This build expects a different systemd unit from the one installed. " +
	"Self-update replaces only the executable and its data directories, never the unit, " +
	"the sudoers rules or file permissions: re-run the installer from this release to apply them."

// UnitState compares the installed systemd unit with the one this build expects.
//
// Self-update cannot change the unit, because a service that could rewrite its
// own systemd configuration could grant itself anything. So a release needing
// new host wiring lands on a machine still running the old unit, and whatever
// it added silently does not work. Embedding the expected unit and comparing is
// how that gets noticed rather than debugged.
func (u *Updater) UnitState() UnitState {
	if u.cfg.ExpectedUnit == "" {
		return UnitUnknown
	}

	installed, err := os.ReadFile(u.unitPath())
	if err != nil {
		return UnitUnknown
	}

	if normaliseUnit(string(installed)) == normaliseUnit(u.cfg.ExpectedUnit) {
		return UnitCurrent
	}

	return UnitStale
}

func (u *Updater) unitPath() string {
	if u.cfg.UnitPath != "" {
		return u.cfg.UnitPath
	}

	return "/etc/systemd/system/" + u.cfg.BinaryName + ".service"
}

// normaliseUnit ignores comments and spacing, so reformatting the unit does not
// read as a functional change.
func normaliseUnit(unit string) string {
	var kept []string

	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		kept = append(kept, trimmed)
	}

	return strings.Join(kept, "\n")
}
