// Confirmed termination: the one primitive that stops an owned process, and the
// only thing entitled to report it as stopped.
//
// # THE BUG THIS REPLACES
//
// Stopping the privileged core did this:
//
//	send SIGTERM  →  return nil  →  RunningState.Set(false)
//
// Sending a signal is not the same as the process having exited. `kill -TERM`
// succeeds as soon as the signal is DELIVERED; a root sing-box in the middle of
// tearing down a TUN interface, or simply wedged, keeps running — with the TUN
// up and routes still pointed at it — while the launcher tells the user it is
// stopped. The UI then offers "Start", so the user starts a SECOND root core
// next to the one that never died.
//
// The same primitive is used by Stop and by Restart. Restart previously relied
// on the crash watcher noticing the exit and bringing the process back; when the
// signal was ignored, restart simply never happened, with no timeout and no
// error — the button appeared to do nothing, forever.
//
// THE CONTRACT
//
//	terminateConfirmed returns nil ONLY after the process is confirmed gone.
//
// Confirmation means the PID no longer exists, or no longer exists AS OUR
// PROCESS (identity check). Anything else — a signal that could not be sent, a
// process still alive after the forced kill, an identity that could not be
// established — is an error, and the caller must NOT report "stopped". A state
// that says stopped while a root core runs is worse than an error, because it is
// a silent lie the user acts on.
package core

import (
	"fmt"
	"strings"
	"time"

	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

const (
	// terminateGracePeriod is how long a process gets to exit after the graceful
	// signal before it is killed. Deliberately short: sing-box closes listeners
	// and routes quickly, and a user watching a spinner is not served by waiting
	// longer on a process that is not going to comply.
	terminateGracePeriod = 5 * time.Second

	// terminatePollInterval is the exit-check cadence. Small enough that the
	// common case (immediate exit) is not visibly delayed, large enough not to
	// spin.
	terminatePollInterval = 100 * time.Millisecond

	// terminateForceTimeout bounds the wait after the forced kill.
	terminateForceTimeout = 3 * time.Second
)

// terminateOutcome describes how a termination ended.
type terminateOutcome struct {
	// Graceful reports whether the process exited on the graceful signal.
	Graceful bool
	// Forced reports whether a forced kill was needed.
	Forced bool
	// Elapsed is how long the whole termination took.
	Elapsed time.Duration
}

// processChecker reports whether a PID exists and still matches an expected
// executable. Injected so tests are deterministic rather than dependent on
// spawning real processes and on machine timing.
type processChecker interface {
	// alive reports whether pid exists and its executable matches expected.
	// A PID that exists but runs something else is NOT alive for our purposes:
	// it is a recycled PID and must never be signalled.
	alive(pid int, expected string) (bool, error)
}

// platformChecker is the production implementation.
//
// Built on the project's existing identity machinery rather than a new process
// probe: pidMatchesCoreCopy/resolveForCompare already compare SYMLINK-RESOLVED
// paths, so a symlink pointing at our binary cannot be used to fake identity,
// and listProcessDetailsDarwin already returns full executable paths. Reusing
// them keeps one definition of "is this our process".
type platformChecker struct{}

func (platformChecker) alive(pid int, expected string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	if !processAlive(pid) {
		return false, nil
	}
	if expected == "" {
		// Caller chose PID-existence only (the privileged wrapper, which is a
		// shell rather than the core). Still not a licence to kill anything:
		// the privileged signal is scoped by PID from the launcher's own pidfile.
		return true, nil
	}
	procs, err := listProcessDetailsDarwin()
	if err != nil {
		// Cannot verify identity ⇒ report the error rather than assume. A
		// caller that swallowed this would force-kill whatever now holds the PID.
		if isUnsupportedPlatformErr(err) {
			// Windows/Linux stubs: fall back to PID existence, which is all
			// those platforms expose through this seam.
			return true, nil
		}
		return false, fmt.Errorf("cannot list processes: %w", err)
	}
	path, ok := pidExecutablePath(pid, procs)
	if !ok {
		// Alive but unidentifiable: NOT ours.
		debuglog.WarnLog("terminate: PID %d is alive but its executable path is unknown; not treating it as ours", pid)
		return false, nil
	}
	if !pidMatchesCoreCopy(path, expected) {
		debuglog.WarnLog("terminate: PID %d now runs %s, expected %s — PID reuse, treating as gone",
			pid, path, expected)
		return false, nil
	}
	return true, nil
}

// isUnsupportedPlatformErr reports whether a process listing is simply not
// available on this platform, as opposed to having failed.
func isUnsupportedPlatformErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "darwin only")
}

// terminateSignal sends the platform-appropriate graceful or forced signal.
type terminateSignal func(pid int, force bool) error

// terminateOwnedProcess stops one process and does not return until it is gone.
//
// This is the single implementation used by Stop, Restart, shutdown, and the
// replacement of a late-started orphan. Duplicating it is what allowed Restart
// to lack the watchdog Stop had.
func terminateOwnedProcess(id ProcessIdentity, reason string, checker processChecker, signal terminateSignal) (terminateOutcome, error) {
	var out terminateOutcome
	start := time.Now()

	// An identity we cannot verify must not be signalled. Returning an error
	// here is the point: guessing would risk killing an unrelated process.
	if id.PID <= 0 {
		return out, fmt.Errorf("%s: no process to stop", reason)
	}
	if id.Executable == "" {
		return out, fmt.Errorf("%s: refusing to signal PID %d without a verified executable path", reason, id.PID)
	}

	alive, err := checker.alive(id.PID, id.Executable)
	if err != nil {
		return out, fmt.Errorf("%s: cannot determine whether PID %d is alive: %w", reason, id.PID, err)
	}
	if !alive {
		// Already gone — the caller wanted it gone, and it is. Not an error.
		out.Elapsed = time.Since(start)
		debuglog.InfoLog("terminate: %s: PID %d already gone", reason, id.PID)
		return out, nil
	}

	if err := signal(id.PID, false); err != nil {
		debuglog.WarnLog("terminate: %s: graceful signal to PID %d failed: %v", reason, id.PID, err)
	} else {
		debuglog.InfoLog("terminate: %s: sent graceful signal to PID %d (%s)", reason, id.PID, id.Executable)
	}

	// Wait for the process to actually leave.
	if exited, err := waitForExit(id, checker, terminateGracePeriod); err != nil {
		return out, fmt.Errorf("%s: error while waiting for PID %d to exit: %w", reason, id.PID, err)
	} else if exited {
		out.Graceful = true
		out.Elapsed = time.Since(start)
		debuglog.InfoLog("terminate: %s: PID %d exited gracefully", reason, id.PID)
		return out, nil
	}

	// Still there. Escalate, and say so — a forced kill is worth knowing about,
	// because it means the process ignored the polite request.
	debuglog.WarnLog("terminate: %s: PID %d still alive after %s, forcing kill", reason, id.PID, terminateGracePeriod)
	if err := signal(id.PID, true); err != nil {
		return out, fmt.Errorf("%s: forced kill of PID %d failed: %w", reason, id.PID, err)
	}
	out.Forced = true

	if exited, err := waitForExit(id, checker, terminateForceTimeout); err != nil {
		return out, fmt.Errorf("%s: error while waiting for PID %d to die after kill: %w", reason, id.PID, err)
	} else if !exited {
		// The process survived SIGKILL. Report it rather than pretending: the
		// caller must not claim "stopped", and the user needs to know a root
		// process is still holding the interface.
		return out, fmt.Errorf("%s: PID %d is still running after a forced kill; it may be unkillable or root-owned", reason, id.PID)
	}
	out.Elapsed = time.Since(start)
	debuglog.InfoLog("terminate: %s: PID %d killed", reason, id.PID)
	return out, nil
}

// waitForExit polls until the process is gone or the timeout expires.
//
// Polling rather than `cmd.Wait` because the privileged and adopted paths have
// no child handle to wait on: on macOS the launcher is not the parent of the
// root core, so there is no waitable object. A process-existence check with
// executable verification is the mechanism that works for both, and it is the
// only one that can also detect a process we did not spawn.
func waitForExit(id ProcessIdentity, checker processChecker, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := checker.alive(id.PID, id.Executable)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(terminatePollInterval)
	}
}

// privilegedTerminateSignal sends signals to a privileged process through the
// authorized helper, since an unprivileged launcher cannot signal root.
//
// The wrapper and the core BOTH have to be signalled: the wrapper is a shell
// that will not necessarily propagate, and the core is what owns the TUN. The
// pidfile is passed so the helper can clean it up, but its removal is NOT taken
// as proof of exit — that was the original bug.
func privilegedTerminateSignal(scriptPID, singboxPID int, pidFile string) terminateSignal {
	return func(_ int, force bool) error {
		return platform.KillPrivilegedProcessForce(scriptPID, singboxPID, pidFile, force)
	}
}

// terminatePrivilegedOwned stops the owned privileged core, verifying BOTH PIDs.
//
// The identity used for confirmation is the core's, because the wrapper may
// legitimately exit first while the core keeps running.
func terminatePrivilegedOwned(scriptPID, singboxPID int, pidFile, exe, reason string, checker processChecker) (terminateOutcome, error) {
	var out terminateOutcome
	start := time.Now()

	if singboxPID <= 0 && scriptPID <= 0 {
		return out, fmt.Errorf("%s: no privileged PID to stop", reason)
	}
	if exe == "" {
		return out, fmt.Errorf("%s: refusing to stop privileged PIDs %d/%d without a verified executable path",
			reason, scriptPID, singboxPID)
	}

	coreAlive := false
	if singboxPID > 0 {
		var err error
		coreAlive, err = checker.alive(singboxPID, exe)
		if err != nil {
			return out, fmt.Errorf("%s: cannot verify PID %d: %w", reason, singboxPID, err)
		}
	}
	wrapperAlive := false
	if scriptPID > 0 {
		// The wrapper is a shell script, so its executable is the shell rather
		// than the core; verify by PID existence only.
		var err error
		wrapperAlive, err = checker.alive(scriptPID, "")
		if err != nil {
			return out, fmt.Errorf("%s: cannot verify PID %d: %w", reason, scriptPID, err)
		}
	}
	if !coreAlive && !wrapperAlive {
		out.Elapsed = time.Since(start)
		return out, nil
	}

	send := privilegedTerminateSignal(scriptPID, singboxPID, pidFile)
	if err := send(0, false); err != nil {
		// A failure to signal root is NOT proof of exit. Do not fall through to
		// "stopped".
		return out, fmt.Errorf("%s: could not signal privileged process: %w", reason, err)
	}

	id := ProcessIdentity{PID: singboxPID, Executable: exe}
	if singboxPID <= 0 {
		id = ProcessIdentity{PID: scriptPID, Executable: exe}
	}
	if exited, err := waitForExit(id, checker, terminateGracePeriod); err != nil {
		return out, fmt.Errorf("%s: error waiting for privileged exit: %w", reason, err)
	} else if exited {
		out.Graceful = true
		out.Elapsed = time.Since(start)
		return out, nil
	}

	debuglog.WarnLog("terminate: %s: privileged core still alive after %s, forcing kill", reason, terminateGracePeriod)
	if err := send(0, true); err != nil {
		return out, fmt.Errorf("%s: forced privileged kill failed: %w", reason, err)
	}
	out.Forced = true
	if exited, err := waitForExit(id, checker, terminateForceTimeout); err != nil {
		return out, fmt.Errorf("%s: error waiting after forced privileged kill: %w", reason, err)
	} else if !exited {
		return out, fmt.Errorf("%s: privileged PID %d survived a forced kill", reason, id.PID)
	}
	out.Elapsed = time.Since(start)
	return out, nil
}
