// Custom core import: install a user-selected sing-box binary as the launcher's
// Data core.
//
// The frontend only chooses a file. Everything that decides what that file IS —
// whether it is a runnable core, whether it can serve the current config, where
// it is installed, whether it actually became the active core — is decided here,
// because a frontend that guessed any of it would be a second source of truth
// about the core.
//
// The transaction is deliberately ordered so the currently installed core is
// never damaged: validate, stage, probe, optionally check the config, and only
// then replace. Any failure before the rename leaves the old binary untouched.

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

const (
	// maxCoreFileBytes caps the import so a mis-selected multi-gigabyte file is
	// refused instead of copied. Generous: a custom sing-box build with extra
	// transport tags is legitimately much larger than a release binary.
	maxCoreFileBytes = 256 << 20 // 256 MiB

	// coreVersionProbeTimeout bounds `<candidate> version`.
	coreVersionProbeTimeout = 3 * time.Second
	// coreConfigCheckTimeout bounds `<candidate> check -c config.json`.
	coreConfigCheckTimeout = 5 * time.Second
	// probeOutputCap bounds collected probe output so a pathological candidate
	// cannot exhaust memory. Cores are chatty, not malicious.
	probeOutputCap = 64 << 10
)

// CoreImportResult is the structured outcome of a core import.
//
// The frontend renders these fields instead of parsing a message: which version
// arrived, whether the config still works with it, and whether the daemon's
// installed copy is now behind.
type CoreImportResult struct {
	// OldVersion is what was installed before, empty when there was none.
	OldVersion string `json:"old_version,omitempty"`
	// NewVersion is the version of the core now installed.
	NewVersion string `json:"new_version"`
	// InstalledPath is where the binary was placed (the canonical Data core).
	InstalledPath string `json:"installed_path"`
	// ActivePath is what ResolveCore() selected afterwards. It can differ from
	// InstalledPath when an environment override outranks the Data core.
	ActivePath string `json:"active_path"`
	// CoreSource is the resolution source: env / data / app / path.
	CoreSource string `json:"core_source"`
	// ConfigChecked is false when there was no config to check.
	ConfigChecked bool `json:"config_checked"`
	// ConfigCompatible reports whether the candidate accepted the current
	// config. Meaningless when ConfigChecked is false.
	ConfigCompatible bool `json:"config_compatible"`
	// DaemonUpdateRequired reports that the daemon service's installed copy no
	// longer matches this core. False when no daemon is installed.
	DaemonUpdateRequired bool `json:"daemon_update_required"`
	// Warning carries a non-fatal anomaly, e.g. the binary installed but a
	// higher-priority override is still the active core.
	Warning string `json:"warning,omitempty"`
	// Core is the authoritative state after the import.
	Core protocol.CoreState `json:"core"`
}

// ImportCoreFile installs the binary at path as the launcher's Data core.
//
// Every failure mode returns a structured code, and in every one of them the
// previously installed core is left exactly as it was.
func (b *Backend) ImportCoreFile(path string) (CoreImportResult, error) {
	if b.ac == nil || b.ac.FileService == nil {
		return CoreImportResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	if !CoreImportSupported() {
		return CoreImportResult{}, &protocol.Error{
			Code:        "unsupported",
			Message:     "installing a custom core is not supported on this platform",
			Recoverable: false,
		}
	}

	// §30: replacing a live core would leave the running process pointing at a
	// file that no longer matches it. The UI blocks this first; the backend
	// enforces it independently, because hiding a button is not a safety
	// boundary.
	//
	// THE LEASE IS TAKEN FIRST, and the check is a separate, earlier courtesy.
	// A check alone is a TOCTOU: it and the rename are two steps, so a start that
	// begins between them still races the import. The lease is the lifecycle's own
	// operation slot, so a start and a replacement compete for one resource and
	// whichever arrives second is refused.
	release, err := b.acquireCoreReplacementLease()
	if err != nil {
		return CoreImportResult{}, err
	}
	defer release()

	if !b.coreIsStoppedForReplacement() {
		return CoreImportResult{}, &protocol.Error{
			Code: "core_busy",
			Message: "the core is running (or is still shutting down). Stop it " +
				"before replacing the core binary.",
			Recoverable: true,
		}
	}

	src, err := b.validateCoreCandidate(path)
	if err != nil {
		return CoreImportResult{}, err
	}

	// §34: if an environment override is active, installing into the Data core
	// would not change the core that actually runs. Refuse rather than install a
	// binary the user believes is in use.
	if b.ac.FileService.CoreSource == string(platform.CoreSourceEnv) {
		return CoreImportResult{}, &protocol.Error{
			Code: "core_override_active",
			Message: "SINGBOX_LAUNCHER_CORE currently overrides the Data core. " +
				"Remove the environment override before replacing the core here.",
			Recoverable: false,
		}
	}

	target := b.ac.FileService.SingboxBundledPath
	if target == "" {
		return CoreImportResult{}, &protocol.Error{
			Code: "install_failed", Message: "the core install path is not resolved", Recoverable: true,
		}
	}

	// §16: the user may have selected the installed core itself. Comparing
	// canonical paths prevents the copy from truncating its own source.
	if same, cmpErr := samePath(src, target); cmpErr == nil && same {
		return CoreImportResult{}, &protocol.Error{
			Code:        "already_installed",
			Message:     "the selected core is already installed",
			Recoverable: false,
		}
	}

	oldVersion, _ := b.ac.GetInstalledCoreVersion()

	staged, cleanup, err := stageCoreBinary(src, target)
	if err != nil {
		return CoreImportResult{}, err
	}
	defer cleanup()

	// §21/§23: probe the STAGED file, not the user's original. The bytes that
	// were validated are then exactly the bytes installed, so the source cannot
	// change between the check and the copy.
	newVersion, err := probeStagedCoreVersion(staged)
	if err != nil {
		return CoreImportResult{}, err
	}

	configChecked, configCompatible, checkDetail := b.checkConfigWithCandidate(staged)

	// §26: a missing config is not a failure — a fresh install may import the
	// core first and add subscriptions afterwards. But an INCOMPATIBLE config is
	// fatal: installing a core that cannot run it would only surface at the next
	// Start, when the whole config fails.
	if configChecked && !configCompatible {
		return CoreImportResult{}, &protocol.Error{
			Code: "core_config_incompatible",
			Message: "this core cannot run the current configuration: " + checkDetail +
				". The installed core was left unchanged.",
			Recoverable: false,
		}
	}

	// §28: atomic replace. Same directory, so rename cannot cross a filesystem.
	if err := os.Rename(staged, target); err != nil {
		return CoreImportResult{}, &protocol.Error{
			Code:        "install_failed",
			Message:     "cannot install the core: " + err.Error(),
			Recoverable: true,
		}
	}

	// §37/§38: every cache derived from the old binary is now wrong, and the
	// active core has to be re-resolved before anything reports a version.
	b.ac.InvalidateCoreBinaryCaches()
	b.ac.FileService.ResolveCore()

	result := CoreImportResult{
		OldVersion:       oldVersion,
		NewVersion:       newVersion,
		InstalledPath:    target,
		ActivePath:       b.ac.FileService.SingboxPath,
		CoreSource:       b.ac.FileService.CoreSource,
		ConfigChecked:    configChecked,
		ConfigCompatible: configCompatible,
	}

	// §39: confirm the installed binary is the one that is actually active. A
	// discrepancy means something outranks the Data core, and reporting
	// "installed" without saying so would be a lie about the running core.
	if same, _ := samePath(b.ac.FileService.SingboxPath, target); !same {
		result.Warning = fmt.Sprintf(
			"the core was installed, but %s is still the active core (%s)",
			b.ac.FileService.SingboxPath, b.ac.FileService.CoreSource)
		debuglog.WarnLog("import_core_file: %s", result.Warning)
	}

	if installed, verr := b.ac.GetInstalledCoreVersion(); verr == nil && installed != "" {
		result.NewVersion = installed
	} else if verr != nil {
		debuglog.WarnLog("import_core_file: post-install version probe failed: %v", verr)
		result.Warning = strings.TrimSpace(result.Warning + " " +
			"the core was installed but its version could not be read back")
	}

	// §32: the daemon runs its own root-owned copy, so this core may now be
	// newer than what the service runs. Reported, never auto-fixed: updating the
	// service needs sudo and stays an explicit action on the Daemon screen.
	result.DaemonUpdateRequired = b.daemonCopyIsStale()

	debuglog.InfoLog("import_core_file: installed %s (was %q, source %s)",
		result.NewVersion, result.OldVersion, result.CoreSource)

	// §41/§93: publish the new truth before returning it, so the event and the
	// response cannot disagree.
	b.EmitCoreState()
	b.emit(protocol.EventDaemonChanged, nil)
	result.Core = b.coreState()
	return result, nil
}

// coreIsStoppedForReplacement reports whether the core is in a settled stopped
// state.
//
// Not "not running": a starting core is about to be running and a stopping one
// has not released its process yet. Replacing the binary underneath either would
// leave the two disagreeing.
func (b *Backend) coreIsStoppedForReplacement() bool {
	if b.ac == nil {
		return false
	}
	// THE LIFECYCLE DECIDES, NOT A PROCESS BOOLEAN.
	//
	// This checked `RunningState.IsRunning()` and the VPN button state, both of which
	// remain false while a start operation is in flight. The gap is not exotic: a start
	// that has been ACCEPTED but has not yet spawned leaves the running flag clear, and
	// an import arriving in that window was permitted to atomically rename the core
	// binary while the start goroutine was about to exec the old path. Version and
	// config were then validated against one file and the process ran another.
	//
	// The operation record is the authority: any start, stop or restart that is not
	// settled means the core file is in use, or is about to be.
	if b.ops.busyForMaintenance() {
		return false
	}
	// Daemon mode runs the core inside the system service, whose copy is not the file
	// being replaced; but a live daemon VPN still depends on the launcher core that
	// built its config, so a running core blocks either way.
	if b.ac.RunningState != nil && b.ac.RunningState.IsRunning() {
		return false
	}
	bs := b.ac.GetVPNButtonState()
	return !bs.IsRunning
}

// acquireCoreReplacementLease takes the exclusion that makes an import atomic against
// the lifecycle.
//
// The busy CHECK above and the rename are two separate steps, so a start beginning
// between them still races the import. The check is necessary and not sufficient: it
// narrows the window, and only mutual exclusion closes it.
//
// The lease is the lifecycle's own operation slot. Taking it through the same
// primitive the start/stop/restart paths use means the import cannot be the one
// component that forgot to participate — an import and a start compete for one lease,
// so whichever arrives second is refused rather than interleaved.
//
// Returns a release function, or an error describing why the core is busy.
func (b *Backend) acquireCoreReplacementLease() (func(), error) {
	if b.ac == nil {
		return nil, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	// The operation record is the single place that knows whether the lifecycle is
	// settled. A lease that lived beside it could be acquired while an operation is
	// running, which is the race this exists to prevent.
	release, ok := b.ops.beginMaintenance()
	if !ok {
		return nil, &protocol.Error{
			Code: "core_busy",
			Message: "the core is being started, stopped or restarted (or another core " +
				"replacement is in progress). Wait for it to settle before installing a " +
				"different core.",
			Recoverable: true,
		}
	}
	return release, nil
}

// validateCoreCandidate checks the path itself before any bytes are touched.
//
// Every rejection here happens before anything is opened for writing, so the
// installed core cannot be damaged by a bad selection.
func (b *Backend) validateCoreCandidate(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", &protocol.Error{
			Code: "bad_path", Message: "no file was selected", Recoverable: false,
		}
	}

	// Resolve symlinks first: every later check, including the self-copy
	// comparison, must be about the real file.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &protocol.Error{
				Code: "file_not_found", Message: "that file no longer exists", Recoverable: true,
			}
		}
		return "", &protocol.Error{
			Code: "bad_path", Message: "cannot resolve the selected path: " + err.Error(), Recoverable: false,
		}
	}

	st, err := os.Stat(real)
	if err != nil {
		return "", &protocol.Error{
			Code: "file_not_found", Message: "cannot read the selected file: " + err.Error(), Recoverable: true,
		}
	}
	if st.IsDir() {
		return "", &protocol.Error{
			Code: "not_regular_file", Message: "the selected item is a folder, not a core binary", Recoverable: false,
		}
	}
	if !st.Mode().IsRegular() {
		return "", &protocol.Error{
			Code: "not_regular_file", Message: "the selected item is not a regular file", Recoverable: false,
		}
	}
	if st.Size() == 0 {
		return "", &protocol.Error{
			Code: "invalid_core", Message: "the selected file is empty", Recoverable: false,
		}
	}
	if st.Size() > maxCoreFileBytes {
		return "", &protocol.Error{
			Code: "file_too_large",
			Message: fmt.Sprintf("the selected file is %d MB; the limit is %d MB",
				st.Size()>>20, maxCoreFileBytes>>20),
			Recoverable: false,
		}
	}

	f, err := os.Open(real)
	if err != nil {
		return "", &protocol.Error{
			Code: "bad_path", Message: "cannot open the selected file: " + err.Error(), Recoverable: false,
		}
	}
	_ = f.Close()

	// §18: architecture is checked on the real bytes rather than by shelling out
	// to `file`, which would mean parsing command output and passing a user path
	// through a shell.
	if err := checkCoreArchitecture(real); err != nil {
		return "", err
	}

	return real, nil
}

// stageCoreBinary copies the candidate next to the destination and makes it
// executable.
//
// The temp file lives in the destination directory on purpose: rename is only
// atomic within one filesystem, and the final step is a rename.
func stageCoreBinary(src, target string) (string, func(), error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, &protocol.Error{
			Code: "install_failed", Message: "cannot create the core directory: " + err.Error(), Recoverable: true,
		}
	}

	tmp, err := os.CreateTemp(dir, ".sing-box.import-*")
	if err != nil {
		return "", func() {}, &protocol.Error{
			Code: "install_failed", Message: "cannot stage the core: " + err.Error(), Recoverable: true,
		}
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	in, err := os.Open(src)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return "", func() {}, &protocol.Error{
			Code: "bad_path", Message: "cannot read the selected file: " + err.Error(), Recoverable: true,
		}
	}
	defer func() { _ = in.Close() }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", func() {}, &protocol.Error{
			Code: "install_failed", Message: "cannot copy the core: " + err.Error(), Recoverable: true,
		}
	}
	// Flush before the rename so a crash cannot leave a renamed-but-empty file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", func() {}, &protocol.Error{
			Code: "install_failed", Message: "cannot flush the core to disk: " + err.Error(), Recoverable: true,
		}
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", func() {}, &protocol.Error{
			Code: "install_failed", Message: "cannot finish staging the core: " + err.Error(), Recoverable: true,
		}
	}

	// §29: the installed file must be executable. 0755 is the mode the rest of
	// the project uses for the core.
	if err := os.Chmod(tmpName, 0o755); err != nil {
		cleanup()
		return "", func() {}, &protocol.Error{
			Code: "install_failed", Message: "cannot make the core executable: " + err.Error(), Recoverable: true,
		}
	}

	return tmpName, cleanup, nil
}

// probeStagedCoreVersion runs `<staged> version` with the project's existing
// version parser.
//
// Reusing the parser is the point: a second "read the version string" routine
// would drift from the one the rest of the app trusts.
func probeStagedCoreVersion(staged string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), coreVersionProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, staged, "version")
	platform.PrepareCommand(cmd)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", &protocol.Error{
			Code:        "invalid_core",
			Message:     "the selected file did not respond to `version` in time; it does not look like a sing-box core",
			Recoverable: false,
		}
	}
	if err != nil {
		return "", &protocol.Error{
			Code: "invalid_core",
			Message: "the selected file is not a runnable sing-box core: " +
				truncateProbeOutput(out, err),
			Recoverable: false,
		}
	}

	version := core.ParseCoreVersionOutput(string(out))
	if version == "" {
		return "", &protocol.Error{
			Code: "invalid_core",
			Message: "the selected file runs but does not report a sing-box version: " +
				truncateProbeOutput(out, nil),
			Recoverable: false,
		}
	}
	return version, nil
}

// checkConfigWithCandidate verifies the current config against the candidate.
//
// The invocation mirrors a real start — same working directory, same
// config-as-basename form — so the check exercises the same resolution the core
// will use when it actually runs. A check performed with different path
// semantics could pass while the real start fails.
func (b *Backend) checkConfigWithCandidate(staged string) (checked, compatible bool, detail string) {
	if b.ac == nil || b.ac.FileService == nil {
		return false, false, ""
	}
	configPath := b.ac.FileService.ConfigPath
	if _, err := os.Stat(configPath); err != nil {
		// §26: no config to check. A fresh install imports the core first.
		debuglog.InfoLog("import_core_file: no config.json — skipping the compatibility check")
		return false, false, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), coreConfigCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, staged, "check", "-c", filepath.Base(configPath))
	platform.PrepareCommand(cmd)
	// Same directory the real start uses, so relative paths resolve identically.
	cmd.Dir = b.ac.FileService.Layout.Data.Bin()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, true, ""
	}
	if ctx.Err() == context.DeadlineExceeded {
		return true, false, "the configuration check did not finish in time"
	}
	return true, false, truncateProbeOutput(out, err)
}

// probeOutputText renders bounded, human-readable probe output.
//
// Capped because a core can be verbose without limit, and the message is for a
// person, not a log file.
func truncateProbeOutput(out []byte, cause error) string {
	s := strings.TrimSpace(stripANSIString(string(out)))
	if len(s) > probeOutputCap {
		s = s[:probeOutputCap] + "…"
	}
	// sing-box writes its complaints to stderr, which CombinedOutput merges in;
	// the first line is the useful one.
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if s == "" && cause != nil {
		s = cause.Error()
	}
	if s == "" {
		s = "no output"
	}
	return s
}

// stripANSIString removes terminal colour codes, which sing-box emits even when
// its output is captured.
func stripANSIString(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			// Skip to the terminating letter of the escape sequence.
			for i < len(s) && !((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// daemonCopyIsStale reports whether the daemon service runs a different core
// than the one just installed.
//
// The existing classifier is asked rather than a new comparison written: it
// already knows what "the service runs our core" means on each platform.
func (b *Backend) daemonCopyIsStale() bool {
	if b.ac == nil || !core.DaemonEngineAvailable() {
		return false
	}
	snap := b.ac.DaemonStatusSnapshot()
	return snap.Service.State != core.DaemonServiceOK
}

// samePath reports whether two paths refer to the same file.
//
// Symlinks are resolved first, so selecting the installed core through a link
// still counts as the same file — otherwise the copy would read and truncate one
// path while writing the other.
func samePath(a, b string) (bool, error) {
	ra, erra := filepath.EvalSymlinks(a)
	if erra != nil {
		ra = filepath.Clean(a)
	}
	rb, errb := filepath.EvalSymlinks(b)
	if errb != nil {
		rb = filepath.Clean(b)
	}
	if ra == rb {
		return true, nil
	}
	// Fall back to identity when both exist: two different paths may still be
	// one file (hard links), and truncating it would destroy the source.
	sa, ea := os.Stat(ra)
	sb, eb := os.Stat(rb)
	if ea != nil || eb != nil {
		return false, nil
	}
	return os.SameFile(sa, sb), nil
}
