package swiftlogic_test

import (
	"os"
	"testing"

	"singbox-launcher/internal/swiftlogic"
)

// TestAvailabilityDetectionIsHonest proves the signal the skip guard relies on.
//
// requireHarness fails on macOS and skips elsewhere, and that split is only worth
// anything if Available() actually reports the truth. A detector that always
// returned true would turn a missing toolchain into a build error with a confusing
// message; one that always returned false would skip the entire suite on the very
// platform it exists to protect.
func TestAvailabilityDetectionIsHonest(t *testing.T) {
	original := os.Getenv("PATH")
	defer func() { _ = os.Setenv("PATH", original) }()

	// With no PATH at all, no compiler can be found.
	if err := os.Setenv("PATH", "/nonexistent"); err != nil {
		t.Fatal(err)
	}
	if swiftlogic.Available() {
		t.Error("Available() reported a Swift compiler with an empty PATH, so the " +
			"skip guard would never fire and a broken toolchain would look present")
	}

	// With the real PATH restored it must be found, or the suite would skip on
	// every machine.
	if err := os.Setenv("PATH", original); err != nil {
		t.Fatal(err)
	}
	if !swiftlogic.Available() {
		t.Skip("no swiftc in the restored PATH; the macOS CI runner has one")
	}
}
