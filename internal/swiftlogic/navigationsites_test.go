package swiftlogic_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAsyncCompletionsNeverUseUnconditionalGoBack covers dimension 11 (no wrong
// navigation after an async step) across EVERY view, not just the four that were
// fixed.
//
// The rule has two halves, and both must hold:
//
//   - A USER-INITIATED Back is synchronous and SHOULD be `goBack()`: the user is
//     looking at the screen they are leaving, so there is nothing to check.
//
//   - AN ASYNC COMPLETION must name the screen it expects to leave, via
//     `popIfCurrent`. If the user pressed Back while the request was in flight,
//     an unconditional pop moves them a SECOND level, so one action throws them
//     two screens from where they were.
//
// Checking the split mechanically is the point: four call sites were fixed by
// hand, and a fifth added later would reintroduce the defect silently.
func TestAsyncCompletionsNeverUseUnconditionalGoBack(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	views, err := filepath.Glob(filepath.Join(root, "macos/Sources/JiejieBox/Views/*.swift"))
	if err != nil || len(views) == 0 {
		t.Fatal("no view files found; this test would pass vacuously")
	}

	// A `Task { ... }` or `do { ... }` region is where an async completion lives.
	asyncRe := regexp.MustCompile(`(?s)(Task\s*\{|do\s*\{)`)

	unowned := 0
	owned := 0
	for _, path := range views {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code := swiftCode(string(raw))
		for _, m := range asyncRe.FindAllStringSubmatchIndex(code, -1) {
			body, _ := balancedBlock(code, m[1]-1)
			if body == "" {
				continue
			}
			// A pop INSIDE an async body must be the owning form.
			if strings.Contains(body, "model.goBack()") {
				line := strings.Count(code[:m[0]], "\n") + 1
				t.Errorf("%s:%d pops with goBack() inside an async block; an async "+
					"completion must use popIfCurrent(<screen>) so it cannot pop a "+
					"screen the user already left",
					filepath.Base(path), line)
				unowned++
			}
			if strings.Contains(body, "popIfCurrent(") {
				owned++
			}
		}
	}

	// The guard must find the owning call sites, or it is checking nothing.
	if owned == 0 {
		t.Fatal("found no popIfCurrent inside an async block anywhere, which cannot " +
			"be right: Add, Edit Save/Delete, Pair and Generate Invite all navigate " +
			"on completion")
	}
	t.Logf("owning async pops: %d, unowned: %d", owned, unowned)
}

// TestOnChangeDoesNotNavigate covers the OTHER mechanism the same defect used.
//
// `DaemonPairView` navigated from `.onChange(of: model.daemon?.paired)`, so the
// screen moved whenever the VALUE changed — including when the change came from a
// background status refresh the user did not cause. That is navigation with no
// owner at all: it cannot know whether the user is still on the screen, because it
// is not reacting to the user's action.
func TestOnChangeDoesNotNavigate(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	views, err := filepath.Glob(filepath.Join(root, "macos/Sources/JiejieBox/Views/*.swift"))
	if err != nil || len(views) == 0 {
		t.Fatal("no view files found")
	}

	onChangeRe := regexp.MustCompile(`\.onChange\(of:[^)]*\)\s*\{`)
	found := 0
	for _, path := range views {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code := swiftCode(string(raw))
		for _, loc := range onChangeRe.FindAllStringIndex(code, -1) {
			body, _ := balancedBlock(code, loc[1]-1)
			found++
			if strings.Contains(body, "goBack()") || strings.Contains(body, "popIfCurrent(") ||
				strings.Contains(body, "path.append") {
				line := strings.Count(code[:loc[0]], "\n") + 1
				t.Errorf("%s:%d navigates from onChange; that reacts to a VALUE change "+
					"rather than to a user action or an owned completion, so it cannot "+
					"know whether the user is still on the screen",
					filepath.Base(path), line)
			}
		}
	}
	if found == 0 {
		t.Fatal("found no onChange handlers at all; the check would pass vacuously")
	}
}
