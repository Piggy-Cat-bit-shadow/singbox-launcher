//go:build !darwin

package service

import "singbox-launcher/backend/protocol"

// CoreImportSupported is false off macOS: the architecture check below is
// written for Mach-O, and the menu bar product ships only for macOS. Reporting
// the capability honestly means the frontend does not offer an action whose
// validation the backend cannot perform.
func CoreImportSupported() bool { return false }

// checkCoreArchitecture is unreachable while CoreImportSupported is false; it
// exists so the import path compiles on every platform.
func checkCoreArchitecture(string) error {
	return &protocol.Error{
		Code:        "unsupported",
		Message:     "installing a custom core is not supported on this platform",
		Recoverable: false,
	}
}
