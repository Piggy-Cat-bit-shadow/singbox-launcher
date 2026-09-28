package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEventReducerDoesNotBlockTheStreamOnSlowIO is statement 26 (§34 name).
//
// The Swift client consumes events with `for await event in stream { await
// self?.apply(event) }`, and `apply` performs network round-trips for several event kinds —
// `loadGroups`, `loadProxies`, `loadSubscriptions`, `loadDaemonStatus`. Awaiting the loop
// body means the stream advances only as fast as the slowest request.
//
// The consequence is a stall that looks like a lost event: a `traffic_rate` tick arriving
// while a `proxies_changed` handler is waiting on a slow `/proxies` is not delivered until
// that request returns. Traffic stops updating for as long as the request takes, and every
// queued event arrives in a burst afterwards. Worse, the events that matter most for
// liveness — core state, shutdown — sit behind the same queue, so a wedged request delays
// the notification that the backend is going away.
//
// The reducer must HAND OFF the reload rather than perform it inline: record the intent, let
// the stream continue, and coalesce so N events of the same kind cause one reload rather
// than N.
//
// This is checked from Go because the Swift client has no test target in this toolchain
// (neither XCTest nor Swift Testing links), and a rule about the shape of the event loop is
// exactly the kind of thing that gets reintroduced by a well-meaning edit.
func TestEventReducerDoesNotBlockTheStreamOnSlowIO(t *testing.T) {
	root := repoRootForTest(t)
	data, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/App/AppModel.swift"))
	if err != nil {
		t.Fatalf("read AppModel.swift: %v", err)
	}
	src := stripSwiftComments(string(data))

	// The event loop.
	loopIdx := strings.Index(src, "for await event in stream")
	if loopIdx < 0 {
		t.Fatal("could not find the event consumption loop")
	}
	loopEnd := strings.Index(src[loopIdx:], "}")
	loop := src[loopIdx : loopIdx+loopEnd+1]

	// The loop body must not await the reducer. If it does, the reducer's own awaits
	// back-pressure the stream, which is the defect.
	if strings.Contains(loop, "await self?.apply(") || strings.Contains(loop, "await apply(") {
		t.Error("the event loop AWAITS the reducer, so a slow reload inside one event " +
			"handler back-pressures the whole stream: traffic ticks stall, and the " +
			"shutdown notification queues behind a request that may never return")
	}

	// And the reducer itself must not perform the reloads inline.
	body := swiftFunctionBody(src, "private func apply(_ event: BackendEvent)")
	if body == "" {
		t.Fatal("could not find the apply(_:) reducer")
	}
	for _, call := range []string{"await loadGroups()", "await loadProxies(", "await loadSubscriptions()",
		"await loadDaemonStatus()"} {
		if strings.Contains(body, call) {
			t.Errorf("the event reducer performs %q inline. Every event behind it waits for "+
				"that round-trip, so a slow request stalls the stream and delays the "+
				"events that report liveness", call)
		}
	}

	// The coalescing mechanism must exist, or "hand it off" becomes "reload per event".
	if !strings.Contains(src, "pendingReloads") && !strings.Contains(src, "dirty") &&
		!strings.Contains(src, "coalesc") {
		t.Error("there is no coalescing state, so either the reducer still blocks or it " +
			"issues one reload per event")
	}
}

// TestEventLoopKeepsDrainingWhileAReloadRuns — the property in the other direction.
//
// A reducer that hands work off but never runs it is silent breakage of a different kind:
// the UI simply stops updating. The hand-off must reach a task that actually performs it.
func TestEventLoopKeepsDrainingWhileAReloadRuns(t *testing.T) {
	root := repoRootForTest(t)
	data, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/App/AppModel.swift"))
	if err != nil {
		t.Fatalf("read AppModel.swift: %v", err)
	}
	src := stripSwiftComments(string(data))

	if !strings.Contains(src, "Task {") && !strings.Contains(src, "Task.detached") {
		t.Error("no task performs the deferred work, so handing it off would mean never " +
			"doing it")
	}
	if !regexp.MustCompile(`func (drain|run)Pending|flushPending`).MatchString(src) {
		t.Error("no drain entry point exists for the coalesced work")
	}
}

// stripSwiftComments removes // and /* */ comments, respecting string literals.
//
// Needed for the same reason the Go version is: a comment that mentions a call must not
// satisfy an assertion about the code.
func stripSwiftComments(src string) string {
	var out strings.Builder
	lines := strings.Split(src, "\n")
	inBlock := false
	for _, line := range lines {
		if inBlock {
			if idx := strings.Index(line, "*/"); idx >= 0 {
				inBlock = false
				line = line[idx+2:]
			} else {
				continue
			}
		}
		var b strings.Builder
		inStr := false
		i := 0
		for i < len(line) {
			c := line[i]
			if inStr {
				if c == '\\' && i+1 < len(line) {
					b.WriteByte(c)
					b.WriteByte(line[i+1])
					i += 2
					continue
				}
				if c == '"' {
					inStr = false
				}
				b.WriteByte(c)
				i++
				continue
			}
			if c == '"' {
				inStr = true
				b.WriteByte(c)
				i++
				continue
			}
			if c == '/' && i+1 < len(line) {
				if line[i+1] == '/' {
					break
				}
				if line[i+1] == '*' {
					if close := strings.Index(line[i+2:], "*/"); close >= 0 {
						i += 2 + close + 2
						continue
					}
					inBlock = true
					break
				}
			}
			b.WriteByte(c)
			i++
		}
		out.WriteString(b.String())
		out.WriteString("\n")
	}
	return out.String()
}

// swiftFunctionBody returns one Swift function's body by counting braces.
func swiftFunctionBody(src, signature string) string {
	idx := strings.Index(src, signature)
	if idx < 0 {
		return ""
	}
	open := strings.Index(src[idx:], "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := idx + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[idx : i+1]
			}
		}
	}
	// UNBALANCED BRACES ARE A FAILURE, NOT A WIDER RESULT.
	//
	// This returned `src[idx:]` — the rest of the FILE — when the braces never balanced. Every
	// caller then asserted `strings.Contains` against a body that extended to the end of the
	// file, so a scan that failed silently turned every assertion into "does this appear
	// ANYWHERE below this point", which is nearly always true. The failure mode is a test that
	// passes while measuring nothing, and it is invisible in the test's own output.
	//
	// An empty result makes the caller's existing `if body == ""` guard fire, which is the
	// honest outcome: the function could not be located.
	return ""
}

// TestDeferredReloadsAreBoundToTheirSession — the teardown half of the coalescing fix.
//
// Introducing deferred work introduced a new thing that outlives a session. Every other piece
// of session state is cleared carefully on teardown, with comments explaining that leaving it
// "would apply a dead process's view of the world" — but the reload set and its task were
// added without being added to that list.
//
// Two concrete consequences: a drain already in flight keeps issuing /groups, /proxies,
// /subscriptions and daemon-status requests against a backend that is shutting down; and a
// batch still queued survives the stop/restart boundary and fires against the NEXT helper,
// applying one session's intent to another's data.
func TestDeferredReloadsAreBoundToTheirSession(t *testing.T) {
	root := repoRootForTest(t)
	data, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/App/AppModel.swift"))
	if err != nil {
		t.Fatalf("read AppModel.swift: %v", err)
	}
	src := stripSwiftComments(string(data))

	for _, teardown := range []string{
		"func stop() async",
		"func quit() async",
	} {
		body := swiftFunctionBody(src, teardown)
		if body == "" {
			t.Fatalf("could not find %s", teardown)
		}
		if !strings.Contains(body, "cancelPendingReloads()") {
			t.Errorf("%s does not cancel the deferred reloads, so work requested by one "+
				"session runs against the next one — or against a backend that is "+
				"shutting down", teardown)
		}
	}

	// And the cancel must actually cancel, not merely forget: a task already inside a
	// reload would otherwise keep going.
	cancel := swiftFunctionBody(src, "private func cancelPendingReloads()")
	if cancel == "" {
		t.Fatal("could not find cancelPendingReloads()")
	}
	if !strings.Contains(cancel, ".cancel()") {
		t.Error("cancelPendingReloads() forgets the task without cancelling it, so a " +
			"reload already in flight keeps running")
	}
	if !strings.Contains(cancel, "removeAll()") {
		t.Error("cancelPendingReloads() leaves the queued batch in place")
	}

	// The drain must not re-arm itself after a cancellation, or a stopped session
	// restarts its own work.
	drain := swiftFunctionBody(src, "private func runPendingReloads() async")
	if drain == "" {
		t.Fatal("could not find runPendingReloads()")
	}
	// AND THE CHECK MUST FOLLOW EVERY AWAIT.
	//
	// The top-of-loop check alone is not enough, and this is the assertion the previous version
	// was missing: `stop()` cancels the task while it is SUSPENDED in an `await`, and the loop
	// head is not reached again until the whole batch has run. So a stop arriving during
	// `loadGroups()` still let `.proxies`, `.subscriptions` and `.daemonStatus` fire against a
	// backend the user has just stopped.
	//
	// A bare `Contains(drain, "Task.isCancelled")` cannot see this: one occurrence anywhere
	// satisfies it, including one inside a comment (comments are stripped here, but the check
	// was still a single substring test over the whole body). Every suspension point has to be
	// followed by a re-check, so that is what is asserted.
	lines := strings.Split(drain, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "await ") {
			continue
		}
		// THE RE-CHECK MUST BE A GUARD, NOT A MENTION OF THE TOKEN.
		//
		// The first version of this only required the next statement to CONTAIN
		// `Task.isCancelled`. Verified: replacing all five real guards with
		// `_ = Task.isCancelled` — a no-op that compiles, `swift build` exit 0 — left the test
		// PASSING with every request in the batch still firing against a stopped backend. A
		// substring check tests spelling, not behaviour, and it is the same mistake this test
		// was rewritten to remove, one level down.
		//
		// A guard has to TEST the token and LEAVE on it. Both parts are required: `if
		// Task.isCancelled { }` with an empty body reads the flag and continues, which is the
		// same defect with more ceremony.
		//
		// The re-check may not be literally the next line — the awaited call can be the last
		// statement of a branch, so closing braces and comments intervene. Look past those and
		// examine the next statement, then its block.
		stmt := -1
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || next == "}" || strings.HasPrefix(next, "//") {
				continue
			}
			stmt = j
			break
		}

		found := false
		if stmt >= 0 {
			head := strings.TrimSpace(lines[stmt])
			// A guard TESTS the flag. `if Task.isCancelled {` and
			// `guard !Task.isCancelled else` are the two forms the codebase uses.
			testsIt := strings.HasPrefix(head, "if Task.isCancelled") ||
				strings.HasPrefix(head, "guard !Task.isCancelled")
			// And it must LEAVE. The body is the lines up to the matching closing brace; a
			// body with no `return` reads the flag and carries on.
			if testsIt {
				depth := 0
				started := false
				for j := stmt; j < len(lines); j++ {
					line := lines[j]
					if !started {
						if idx := strings.Index(line, "{"); idx >= 0 {
							started = true
							depth = 1
							if strings.Contains(line[idx:], "return") {
								found = true
								break
							}
						}
						// `guard ... else { return }` on one line.
						if strings.Contains(line, "else") && strings.Contains(line, "return") {
							found = true
							break
						}
						continue
					}
					if strings.Contains(line, "return") {
						found = true
						break
					}
					depth += strings.Count(line, "{") - strings.Count(line, "}")
					if depth <= 0 {
						break
					}
				}
			}
		}
		if !found {
			t.Errorf("runPendingReloads() awaits %q without re-checking cancellation "+
				"afterwards (line %d of its body). A stop() arriving while that call is "+
				"suspended lets every remaining request in the batch run against a backend "+
				"the user has stopped — and a late one can land after a NEWER session has "+
				"already loaded, overwriting fresh state with the old session's",
				trimmed, i+1)
		}
	}

	// Guard the guard: the scan must have found the awaits at all, or the loop above is
	// vacuous in exactly the way `swiftFunctionBody` used to make possible.
	if !strings.Contains(drain, "await ") {
		t.Fatal("the drain body contains no `await` at all, so the per-await cancellation " +
			"check above proved nothing — the function was probably not located correctly")
	}
}
