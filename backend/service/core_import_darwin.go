//go:build darwin

package service

import (
	"debug/macho"

	"singbox-launcher/backend/protocol"
)

// CoreImportSupported reports whether this build can install a user-selected
// core.
//
// Import needs a platform-specific "is this a runnable executable for THIS
// machine" check, and that check exists only where the format is known. The
// capability is reported rather than guessed so the frontend never offers an
// action the backend cannot perform.
func CoreImportSupported() bool { return true }

// checkCoreArchitecture refuses a Mach-O that cannot run on this machine.
//
// The shipped macOS build is arm64-only, so an x86_64-only binary would install
// cleanly and then fail to start. Catching it here means the user is told why
// instead of watching a core that never comes up.
//
// debug/macho is used rather than shelling out to `file`: no command parsing, no
// user-controlled path in a shell, and the answer comes from the file itself.
func checkCoreArchitecture(path string) error {
	// Universal binaries are checked slice by slice: macho.Open only reads the
	// first slice, so an arm64 slice could sit behind an x86_64 one.
	if fat, err := macho.OpenFat(path); err == nil {
		defer func() { _ = fat.Close() }()
		for _, arch := range fat.Arches {
			if arch.Cpu == macho.CpuArm64 {
				return nil
			}
		}
		return wrongArchError("no arm64 slice in this universal binary")
	}

	f, err := macho.Open(path)
	if err != nil {
		// Not a Mach-O at all — a script, a text file, or a Linux binary. The
		// version probe would reject it too, but this says why more directly.
		return &protocol.Error{
			Code: "wrong_architecture",
			Message: "the selected file is not a macOS executable. " +
				"Pick a sing-box build for macOS (arm64).",
			Recoverable: false,
		}
	}
	defer func() { _ = f.Close() }()

	if f.Cpu != macho.CpuArm64 {
		return wrongArchError("it is " + f.Cpu.String())
	}
	return nil
}

// wrongArchError keeps the two rejection paths saying the same thing.
func wrongArchError(detail string) error {
	return &protocol.Error{
		Code: "wrong_architecture",
		Message: "this core does not contain an arm64 executable (" + detail +
			"). Pick a sing-box build for Apple Silicon.",
		Recoverable: false,
	}
}
