// Package swiftlogic runs the macOS frontend's PURE logic under the Go test
// suite.
//
// WHY THIS EXISTS. The build toolchain ships neither XCTest nor the Swift
// Testing macro plugin, so a Swift test target cannot be built here at all
// (verified: "external macro implementation type 'SwiftUIMacros.StateMacro'
// could not be found", and `swift test` fails to resolve the testing library).
// Every previous frontend invariant was therefore enforced by READING the Swift
// source and matching strings.
//
// That is a weak guarantee with a demonstrated failure mode. A source-shape test
// accepts any text that contains the right words, including text that does the
// opposite — this audit found a cancellation check that passed while every guard
// had been replaced by the no-op `_ = Task.isCancelled`, and a size-limit test
// that passed with the limit comparison disabled, because both matched a
// SUBSTRING of code that no longer ran.
//
// This package takes the other route: the Swift files that contain no SwiftUI
// (pure state machines, value types, validation) are COMPILED and EXECUTED by
// swiftc, with a generated main that reports one line per assertion. Go parses
// that output and turns it into real test failures. The assertions therefore run
// against the shipping implementation, not against a copy of its text.
//
// The helper is skipped when swiftc is unavailable (non-Darwin dev boxes), but
// it is NOT skipped on CI, which runs on macOS — see the guard in the test file,
// which fails rather than skips when the sources are present but the compiler is
// not.
package swiftlogic

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Source is a Swift file that participates in the logic harness, in the order it
// must be passed to swiftc. Only files that compile without SwiftUI belong here:
// importing SwiftUI pulls in the macro plugin this toolchain cannot load.
var Sources = []string{
	"macos/Sources/JiejieBox/Models/Protocol.swift",
	"macos/Sources/JiejieBox/Models/SubscriptionURLInput.swift",
	"macos/Sources/JiejieBox/Models/DaemonCommandLifetime.swift",
	"macos/Sources/JiejieBox/Models/NavigationStackModel.swift",
	"macos/Sources/JiejieBox/Models/RequestGeneration.swift",
	"macos/Sources/JiejieBox/Models/ActionPolicy.swift",
	"macos/Sources/JiejieBox/App/DraftStore.swift",
}

// repoRoot locates the checkout from this package's own path, so the harness
// works regardless of the test's working directory.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine caller path")
	}
	// <root>/internal/swiftlogic/swiftlogic.go -> <root>
	return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
}

// Available reports whether the Swift compiler can be used here.
func Available() bool {
	_, err := exec.LookPath("swiftc")
	return err == nil
}

// Program compiles a harness around the given assertion body and returns the
// executable path.
//
// Each call compiles, because the program IS the assertions: a cached binary
// would carry a different test's checks. The sources are small and swiftc is
// fast, and `Run` is called once per test file rather than once per assertion.
func Program(body string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "swiftlogic")
	if err != nil {
		return "", err
	}
	// The Localization stub is generated from the real enum, so a new key cannot
	// make the harness fail to compile.
	stub, err := buildLocalizationStub(root)
	if err != nil {
		return "", err
	}
	stubPath, err := writeLocalizationStubFile(dir, stub.Source)
	if err != nil {
		return "", err
	}

	main := filepath.Join(dir, "main.swift")
	var src strings.Builder
	// A tiny assertion vocabulary, so the generated main stays readable and the
	// Go side has one line format to parse.
	src.WriteString("import Foundation\n\n")
	src.WriteString("var swiftlogicFailures = 0\n")
	src.WriteString("func check(_ name: String, _ condition: Bool) {\n")
	src.WriteString("    if condition { print(\"PASS \\(name)\") } else { print(\"FAIL \\(name)\"); swiftlogicFailures += 1 }\n")
	src.WriteString("}\n\n")
	// The assertions run inside a @MainActor function because the shipping types
	// they exercise are main-actor isolated (DraftStore is). Running them on the
	// main actor is also what the real app does, so the harness cannot
	// accidentally pass by avoiding the isolation the app obeys.
	src.WriteString("@MainActor\nfunc swiftlogicRun() {\n")
	src.WriteString(body)
	src.WriteString("\n}\n\n")
	src.WriteString("MainActor.assumeIsolated { swiftlogicRun() }\n")
	src.WriteString("exit(swiftlogicFailures == 0 ? 0 : 1)\n")
	if err := os.WriteFile(main, []byte(src.String()), 0o600); err != nil {
		return "", err
	}

	args := []string{"-O"}
	// The stub comes first: the real sources reference `L`.
	args = append(args, stubPath)
	for _, rel := range Sources {
		args = append(args, filepath.Join(root, rel))
	}
	args = append(args, main, "-o", filepath.Join(dir, "harness"))
	cmd := exec.Command("swiftc", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swiftc failed: %v\n%s", err, out)
	}
	return filepath.Join(dir, "harness"), nil
}

// Result is one harness run.
type Result struct {
	// Passed and Failed hold the assertion names, in order.
	Passed []string
	Failed []string
	// Stderr is the program's stderr, for diagnosing a crash.
	Stderr string
}

// Run compiles and executes a Swift assertion body.
func Run(body string) (*Result, error) {
	bin, err := Program(body)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A non-zero exit is the expected way to report failures, so it is not an
	// error here; only a build failure or a missing binary is.
	_ = cmd.Run()

	res := &Result{Stderr: stderr.String()}
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "PASS "):
			res.Passed = append(res.Passed, strings.TrimPrefix(line, "PASS "))
		case strings.HasPrefix(line, "FAIL "):
			res.Failed = append(res.Failed, strings.TrimPrefix(line, "FAIL "))
		}
	}
	return res, nil
}
