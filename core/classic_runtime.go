// Classic runtime: generation-owned lifecycle state.
//
// # WHY A GENERATION, AND WHY NOW
//
// The classic engine's state used to be a set of loose fields on AppController —
// RunningState, SingboxCmd, SingboxPrivilegedMode, SingboxPrivilegedPID,
// SingboxPrivilegedSingboxPID, StoppedByUser, RestartRequestedByUser,
// ConsecutiveCrashAttempts, a PID file — written from dozens of places, with
// asynchronous callbacks (the crash Monitor, the privileged-exit waiter, the
// restart delay, the stability timer) deciding what to do by reading them. Two
// concrete failures came out of that, both reproducible:
//
//  1. A crash monitor sleeping in its 2-second restart delay would wake AFTER the
//     user switched to the daemon engine and call `svc.Start(true)`, bringing a
//     classic core back up next to the daemon's. Two engines, one VPN.
//
//  2. A start still in progress (rebuild, authorization, spawn) is not "running"
//     yet, so the engine could be switched underneath it, and the classic core
//     it eventually spawned belonged to nobody.
//
// Neither is fixable by adding another flag, because the defect is that an
// asynchronous callback had no way to ask "am I still the current owner?".
// A monotonically increasing GENERATION answers exactly that question, and every
// callback captures the generation it was created in and refuses to act for any
// other. That is the invariant this file exists to enforce:
//
//	AN ASYNC CALLBACK FROM GENERATION N MAY NOT ACT ON BEHALF OF GENERATION M.
//
// Ownership is expressed as one struct rather than several parallel fields so
// that "which process do I own" cannot be half-updated: the identity, the
// generation it belongs to, and whether it is privileged are committed together.
package core

import (
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// ClassicPhase is the lifecycle phase of the classic engine.
//
// These are the phases the WIRE protocol already exposes ("starting",
// "running", "stopping") and which the runtime previously could not produce:
// coreState() derived everything from a single boolean, so "starting" and
// "stopping" existed in the protocol and in the SwiftUI client while never
// occurring in reality. A state the UI can render but the backend never emits is
// worse than a missing one — it invites the client to build logic on a
// transition that never fires.
type ClassicPhase string

const (
	// ClassicStopped — no owned process, nothing in flight.
	ClassicStopped ClassicPhase = "stopped"
	// ClassicStarting — a start has been accepted and is in progress. The core
	// is NOT yet usable.
	ClassicStarting ClassicPhase = "starting"
	// ClassicRunning — the owned process is up and past the early-exit window.
	ClassicRunning ClassicPhase = "running"
	// ClassicStopping — a stop is in progress; the process may still exist.
	ClassicStopping ClassicPhase = "stopping"
	// ClassicRestarting — a restart is in progress (stop, then start).
	ClassicRestarting ClassicPhase = "restarting"
	// ClassicFailed — the last operation failed. The process may or may not
	// still exist, which is why phase alone is never used to decide whether it
	// is safe to kill: ownership is.
	ClassicFailed ClassicPhase = "failed"
)

// isSettled reports whether the runtime has no operation in flight.
//
// Only a settled runtime may be handed to another engine. This is the single
// predicate the mode switch consults, so "starting" cannot be mistaken for
// "stopped just because RunningState is false" — the mistake that allowed a
// classic core to appear after the switch to daemon.
func (p ClassicPhase) isSettled() bool {
	return p == ClassicStopped || p == ClassicFailed
}

// ProcessIdentity identifies a process well enough to kill it safely.
//
// PID alone is NOT enough: PIDs are reused, and killing a recycled PID means
// killing an unrelated process — potentially another user's VPN. The executable
// path is checked before any signal is sent, everywhere.
type ProcessIdentity struct {
	PID int
	// Executable is the path we expect this PID to be running. An empty
	// Executable means "identity not established", and no signal may be sent.
	Executable string
	// StartedAt is when this runtime spawned the process. Used to distinguish
	// a live process from a recycled PID that happens to share the path.
	StartedAt time.Time
}

// valid reports whether this identity is safe to act on.
func (id ProcessIdentity) valid() bool {
	return id.PID > 0 && id.Executable != ""
}

// String renders the identity for logs.
func (id ProcessIdentity) String() string {
	return fmt.Sprintf("pid=%d exe=%s", id.PID, id.Executable)
}

// classicRuntime is the classic engine's lifecycle owner.
//
// All phase transitions and all process-identity changes go through the methods
// on this type. Code elsewhere reads state through snapshots; it does not write
// the fields directly. That concentration is the point — with the state written
// from twenty functions, no invariant about it can be enforced.
type classicRuntime struct {
	mu sync.Mutex

	// generation increments every time ownership of the classic engine is
	// (re)established or abandoned. Async work captures it at creation and
	// compares before acting.
	generation uint64

	phase ClassicPhase

	// cmd is the owned child process, for the non-privileged path.
	cmd *exec.Cmd
	// privileged is the owned root process, for the TUN path.
	privileged bool
	// scriptPID is the PID of the privileged wrapper; singboxPID is the core.
	// Two PIDs because killing only the wrapper would leave the root core.
	scriptPID  int
	singboxPID int
	pidFile    string
	// identity is the process this runtime believes it owns, privileged or not.
	identity ProcessIdentity

	// stoppedByUser / restartRequested are operation-scoped INTENT, not sticky
	// state: they belong to the current operation and are cleared when it
	// settles. They used to survive a failed restart, so an unrelated later exit
	// would be read as "the user asked for a restart" and start a core nobody
	// requested.
	stoppedByUser    bool
	restartRequested bool

	// crashAttempts counts consecutive crash restarts in the CURRENT generation
	// chain. Reset when the core stays up long enough to be considered stable,
	// and on any deliberate stop.
	crashAttempts int

	// opID identifies the in-flight operation, for logs and for late results to
	// report against.
	opID uint64
}

// classicSnapshot is a consistent read of the runtime, taken under the lock.
type classicSnapshot struct {
	Generation       uint64
	Phase            ClassicPhase
	Privileged       bool
	ScriptPID        int
	SingboxPID       int
	PIDFile          string
	Identity         ProcessIdentity
	StoppedByUser    bool
	RestartRequested bool
	CrashAttempts    int
	OpID             uint64
	// HasCmd reports whether a non-privileged child is held.
	HasCmd bool
}

// snapshot returns a consistent copy of the runtime state.
func (r *classicRuntime) snapshot() classicSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return classicSnapshot{
		Generation:       r.generation,
		Phase:            r.phase,
		Privileged:       r.privileged,
		ScriptPID:        r.scriptPID,
		SingboxPID:       r.singboxPID,
		PIDFile:          r.pidFile,
		Identity:         r.identity,
		StoppedByUser:    r.stoppedByUser,
		RestartRequested: r.restartRequested,
		CrashAttempts:    r.crashAttempts,
		OpID:             r.opID,
		HasCmd:           r.cmd != nil,
	}
}

// beginOperation claims the runtime for a new operation.
//
// Returns the generation and operation id the caller must present when it later
// commits a result, or ok=false when the request is refused. Refusal happens
// when an operation is already in flight: two concurrent starts must not both
// believe they own the engine.
//
// A start is the one case that MAY proceed while the phase is settled-only, so
// the caller states its intent. Everything else requires a settled runtime.
func (r *classicRuntime) beginOperation(phase ClassicPhase, allowWhileBusy bool) (gen uint64, opID uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !allowWhileBusy && !r.phase.isSettled() {
		return 0, 0, false
	}
	r.opID++
	r.phase = phase
	// Intent is per-operation: carrying it over from a previous operation is
	// exactly how a stale "user wants a restart" leaked into an unrelated exit.
	r.stoppedByUser = false
	r.restartRequested = false
	return r.generation, r.opID, true
}

// renewGeneration abandons the current generation and starts a new one.
//
// Called when ownership changes hands — engine switch, shutdown, or a fresh
// start after the previous runtime was closed. Any async callback still holding
// the old generation becomes a no-op, which is what stops a stale crash monitor
// from resurrecting a core after the user moved to the daemon engine.
//
// Returns the new generation.
func (r *classicRuntime) renewGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	// A new generation owns nothing yet.
	r.phase = ClassicStopped
	r.cmd = nil
	r.privileged = false
	r.scriptPID = 0
	r.singboxPID = 0
	r.pidFile = ""
	r.identity = ProcessIdentity{}
	r.stoppedByUser = false
	r.restartRequested = false
	r.crashAttempts = 0
	return r.generation
}

// ClassicPhase reports the current classic runtime phase.
func (ac *AppController) ClassicPhase() ClassicPhase {
	if ac == nil {
		return ClassicStopped
	}
	return ac.classic.currentPhase()
}

// currentPhase reads the current phase.
func (r *classicRuntime) currentPhase() ClassicPhase {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase
}

// ClassicSettled reports whether the classic runtime has no operation in flight,
// which is the precondition for handing the engine to another backend.
func (ac *AppController) ClassicSettled() bool {
	if ac == nil {
		return true
	}
	return ac.classic.currentPhase().isSettled()
}

// ClassicGeneration exposes the live generation, for tests.
func (ac *AppController) ClassicGeneration() uint64 {
	if ac == nil {
		return 0
	}
	return ac.classic.currentGeneration()
}

// currentGeneration returns the live generation.
func (r *classicRuntime) currentGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

// isCurrent reports whether gen is still the live generation.
//
// This is THE ownership test every asynchronous callback must pass before it
// touches state or starts a process.
func (r *classicRuntime) isCurrent(gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation == gen
}

// setPhase commits a phase transition for a specific generation.
//
// A stale generation's transition is dropped: this is what prevents an old
// monitor from setting the state of a new runtime, the failure mode where a
// generation-10 crash callback reported "stopped" over a healthy generation-11
// core.
func (r *classicRuntime) setPhase(gen uint64, phase ClassicPhase) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false
	}
	r.phase = phase
	return true
}

// commitChild records an owned non-privileged child process.
func (r *classicRuntime) commitChild(gen uint64, cmd *exec.Cmd, exe string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false
	}
	r.cmd = cmd
	r.privileged = false
	r.scriptPID = 0
	r.singboxPID = 0
	r.pidFile = ""
	if cmd != nil && cmd.Process != nil {
		r.identity = ProcessIdentity{PID: cmd.Process.Pid, Executable: exe, StartedAt: time.Now()}
	}
	return true
}

// commitPrivileged records an owned root process.
func (r *classicRuntime) commitPrivileged(gen uint64, scriptPID, singboxPID int, pidFile, exe string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false
	}
	r.cmd = nil
	r.privileged = true
	r.scriptPID = scriptPID
	r.singboxPID = singboxPID
	r.pidFile = pidFile
	// The CORE pid is the identity that matters: the wrapper is a shell that may
	// exit while the root core keeps running, and the core is what holds the TUN.
	pid := singboxPID
	if pid <= 0 {
		pid = scriptPID
	}
	r.identity = ProcessIdentity{PID: pid, Executable: exe, StartedAt: time.Now()}
	return true
}

// clearOwnership drops the owned process for a generation.
//
// Only the owning generation may clear ownership: otherwise a stale monitor
// could erase the identity of a live process, leaving a running root core that
// nothing can stop because the launcher no longer knows it exists.
func (r *classicRuntime) clearOwnership(gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false
	}
	r.cmd = nil
	r.privileged = false
	r.scriptPID = 0
	r.singboxPID = 0
	r.pidFile = ""
	r.identity = ProcessIdentity{}
	return true
}

// ownedProcess returns the identity of the process this runtime owns, if any.
//
// Reads ownership rather than phase: "is the phase stopped?" is a statement
// about intent, while this answers "is there a process I am responsible for?".
// Stop must use THIS, because a logical state that says stopped while a root
// core is still up is precisely the desynchronisation that leaves an orphan.
func (r *classicRuntime) ownedProcess() (ProcessIdentity, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.identity.valid() {
		return ProcessIdentity{}, false, r.privileged
	}
	return r.identity, true, r.privileged
}

// setIntent records the user's intent for the current operation.
func (r *classicRuntime) setIntent(gen uint64, stoppedByUser, restartRequested bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false
	}
	r.stoppedByUser = stoppedByUser
	r.restartRequested = restartRequested
	return true
}

// takeRestartIntent consumes the restart intent, returning whether it was set.
//
// Taken rather than read, so the intent cannot be acted on twice: a restart flag
// that survives its own handling is how one user restart becomes a restart loop.
func (r *classicRuntime) takeRestartIntent(gen uint64) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false, false
	}
	had := r.restartRequested
	r.restartRequested = false
	return had, true
}

// clearIntent drops any recorded intent for a generation.
func (r *classicRuntime) clearIntent(gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return
	}
	r.stoppedByUser = false
	r.restartRequested = false
}

// noteCrashAttempt increments the crash counter, returning the new value.
func (r *classicRuntime) noteCrashAttempt(gen uint64) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return 0, false
	}
	r.crashAttempts++
	return r.crashAttempts, true
}

// resetCrashAttempts clears the crash counter after a stable run.
func (r *classicRuntime) resetCrashAttempts(gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return false
	}
	r.crashAttempts = 0
	return true
}

// crashAttemptsFor reads the counter for a generation.
func (r *classicRuntime) crashAttemptsFor(gen uint64) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != gen {
		return 0, false
	}
	return r.crashAttempts, true
}

// adoptExisting records ownership of a core this launcher started in a PREVIOUS
// session and has just re-identified.
//
// Adoption is the case the old design handled worst: the process exists, so the
// UI must show it as running and Stop must work, but there is no `exec.Cmd` to
// Wait on because the launcher is not its parent. Recording the identity (with
// the executable path, so a recycled PID is never signalled) is what makes Stop
// and the existence watcher possible without pretending to own a child we do
// not have.
func (r *classicRuntime) adoptExisting(exe string, pid int, privileged bool) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	r.cmd = nil
	r.privileged = privileged
	r.scriptPID = 0
	r.singboxPID = pid
	r.pidFile = ""
	r.identity = ProcessIdentity{PID: pid, Executable: exe, StartedAt: time.Now()}
	r.phase = ClassicRunning
	r.stoppedByUser = false
	r.restartRequested = false
	r.crashAttempts = 0
	return r.generation
}

// phaseForState maps the runtime phase onto the wire state.
//
// Kept next to the phases so the two lists cannot drift: a phase the protocol
// cannot express would be reported as "stopped", which is the false reassurance
// this whole exercise is meant to remove.
func (p ClassicPhase) wireState() string {
	switch p {
	case ClassicStarting:
		return "starting"
	case ClassicRunning:
		return "running"
	case ClassicStopping, ClassicRestarting:
		return "stopping"
	case ClassicFailed:
		return "error"
	}
	return "stopped"
}
