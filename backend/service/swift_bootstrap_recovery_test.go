package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFailedBootstrapReleasesTheHelper is statement 25 (§34 name).
//
// `BackendClient.start` begins with `guard process == nil else { return }`. That guard exists
// so two concurrent bootstraps cannot launch two helpers — but it keys on the PROCESS, not
// on whether the previous attempt got as far as being usable.
//
// A bootstrap that fails AFTER the process launched (the handshake throws, the event stream
// cannot be opened, the snapshot request fails) leaves a running helper behind. The UI shows
// `.failed` with a Retry button; Retry calls `start()`, which hits `guard process == nil`,
// returns without doing anything, and the UI reports the same failure again. The app is
// wedged: the only way out is to quit and relaunch, and nothing on screen says so.
//
// The failure path must release the client, so Retry genuinely starts over.
func TestFailedBootstrapReleasesTheHelper(t *testing.T) {
	root := repoRootForTest(t)
	data, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/App/AppModel.swift"))
	if err != nil {
		t.Fatalf("read AppModel.swift: %v", err)
	}
	src := stripSwiftComments(string(data))

	body := swiftFunctionBody(src, "private func performStart()")
	if body == "" {
		t.Fatal("could not find performStart()")
	}

	// Both catch blocks — cancellation and failure — must release the helper.
	// Count them: a bootstrap can fail on the handshake, on the stream, or on the snapshot,
	// and every one of those paths lands in the generic catch.
	// `AppModel.stop()` is the model's teardown; it calls `client.shutdown()` and clears
	// the session state. Either name is acceptable — what matters is that the failure path
	// RELEASES the helper rather than leaving it running.
	if !strings.Contains(body, "await stop()") && !strings.Contains(body, "client.shutdown") {
		t.Error("a failed bootstrap does not stop the client, so the helper process stays " +
			"alive. `BackendClient.start` guards on `process == nil`, so the Retry button " +
			"then calls start(), does nothing at all, and reports the same failure — the " +
			"app is wedged until it is quit and relaunched")
	}

	// The client must also forget the process, or stopping is not enough.
	client, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/Services/BackendClient.swift"))
	if err != nil {
		t.Fatalf("read BackendClient.swift: %v", err)
	}
	csrc := stripSwiftComments(string(client))
	// `process` is released by `cleanupOwnedState`, which `shutdown()` reaches through
	// `cleanupAfterExit` — so the check is that the release exists on the teardown path,
	// not that it sits in one particular function.
	owner := swiftFunctionBody(csrc, "private func cleanupOwnedState(")
	if owner == "" {
		t.Fatal("could not find BackendClient.cleanupOwnedState()")
	}
	if !strings.Contains(owner, "process = nil") {
		t.Error("the teardown path never clears `process`, so the `guard process == nil` in " +
			"start() still refuses to launch a replacement after a failed bootstrap")
	}
	// And shutdown must reach it, or clearing the process is unreachable from stop().
	shutdown := swiftFunctionBody(csrc, "func shutdown() async throws")
	if shutdown == "" {
		t.Fatal("could not find BackendClient.shutdown()")
	}
	if !strings.Contains(shutdown, "cleanupAfterExit(") {
		t.Error("shutdown() does not reach cleanupAfterExit(), so it never releases the " +
			"process and a retry after a failed bootstrap cannot start a new helper")
	}
}

// TestBootstrapFailureIsRetryable — the state the UI needs to offer a working retry.
func TestBootstrapFailureIsRetryable(t *testing.T) {
	root := repoRootForTest(t)
	data, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/App/AppModel.swift"))
	if err != nil {
		t.Fatalf("read AppModel.swift: %v", err)
	}
	src := stripSwiftComments(string(data))

	body := swiftFunctionBody(src, "private func performStart()")
	// The failure path must land in `.failed`, which is what the UI keys the Retry button
	// on — a retry offered for a state that was never set is a dead button.
	if !strings.Contains(body, "connection = .failed(") {
		t.Error("a failed bootstrap does not report .failed, so the UI cannot offer a retry")
	}
	// And the barrier must be disarmed, or buffers accumulate forever.
	if !strings.Contains(body, "disarmBaselineBarrier()") {
		t.Error("a failed bootstrap leaves the baseline barrier armed")
	}
}
