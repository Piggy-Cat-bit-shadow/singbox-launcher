package swiftlogic_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoFile reads a file from the checkout.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// swiftCode strips comments so a match cannot be satisfied by prose.
//
// This is the lesson from the source-shape tests this suite replaces: a check that
// greps raw text passes when the pattern appears in a COMMENT describing the
// behaviour, including a comment describing behaviour that was removed. Every
// source-level assertion below runs against code only.
func swiftCode(src string) string {
	// Block comments, then line comments. Strings are not stripped: the patterns
	// checked here are structural and do not appear inside literals.
	block := regexp.MustCompile(`(?s)/\*.*?\*/`)
	src = block.ReplaceAllString(src, "")
	line := regexp.MustCompile(`(?m)//.*$`)
	return line.ReplaceAllString(src, "")
}

// TestActionRowHoverOnlyForEnabledAction covers the hover/hit-target dimension.
//
// A row that highlights on hover is promising the user that clicking will do
// something. An action row whose action is disabled but which still lights up is
// how a user concludes the app is ignoring them. The hover state must be gated on
// `isEnabled` in CODE, not merely described.
func TestActionRowHoverOnlyForEnabledAction(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/ActionRow.swift"))

	// The hover assignment must combine the hover flag with the enabled state.
	// Matched structurally so a reformat cannot silently defeat it.
	pattern := regexp.MustCompile(`isHovering\s*=\s*\$0\s*&&\s*isEnabled`)
	if !pattern.MatchString(code) {
		t.Error("ActionRow's hover state is not gated on isEnabled, so a disabled " +
			"action still highlights and promises a click that will not happen")
	}
	// And the guard must not be a bare assignment of the hover flag.
	bare := regexp.MustCompile(`isHovering\s*=\s*\$0\s*\n`)
	if bare.MatchString(code) {
		t.Error("ActionRow assigns hover without consulting isEnabled")
	}
}

// TestLatencyButtonHasAccessibilityLabel covers the accessibility dimension.
//
// A latency button renders a bare number — "42 ms" or an em dash. Read aloud that
// is meaningless: the user cannot tell which node it belongs to or what the number
// measures. The label must name the node and the action, so the control is usable
// without sight.
func TestLatencyButtonHasAccessibilityLabel(t *testing.T) {
	src := repoFile(t, "macos/Sources/JiejieBox/Views/ProxiesView.swift")
	code := swiftCode(src)

	// The label is built by a model helper, which must exist and must mention the
	// node and the measurement.
	if !strings.Contains(code, "delayAccessibilityLabel") {
		t.Error("the latency action passes no accessibility label, so a bare " +
			"number is announced with no node and no unit")
	}

	proto := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Models/Protocol.swift"))
	helper := regexp.MustCompile(`func delayAccessibilityLabel`)
	if !helper.MatchString(proto) {
		t.Fatal("delayAccessibilityLabel is referenced but not defined")
	}
	// The helper must interpolate the node's name and the latency key, so the
	// spoken label identifies both WHAT and HOW MUCH.
	// Read the function BODY, up to the next declaration, so the assertion cannot
	// be satisfied by an unrelated mention elsewhere in the file.
	idx := helper.FindStringIndex(proto)
	body := proto[idx[0]:]
	if end := regexp.MustCompile(`\n\}`).FindStringIndex(body); end != nil {
		body = body[:end[1]]
	}
	// The spoken label must identify WHICH node (the node's own label) and WHAT is
	// measured (the latency key), or it announces a bare number.
	if !strings.Contains(body, "label") {
		t.Error("the latency accessibility label does not name the node, so a bare " +
			"number is announced with no indication of which node it belongs to")
	}
	if !strings.Contains(body, "measureLatencyFor") {
		t.Error("the latency accessibility label does not state what the number " +
			"measures, so it cannot be understood without sight")
	}
}

// TestUnknownEngineDoesNotPretendClassicActive covers the engine label.
//
// The active engine was read as `activeEngine ?? "classic"`, so an engine the
// frontend does not recognise — a newer build, a renamed protocol value — was
// displayed as if Classic were active. The user then sees a mode selected that
// they are not running, and any decision they make from that screen starts from a
// false premise.
func TestUnknownEngineDoesNotPretendClassicActive(t *testing.T) {
	src := swiftCode(repoFile(t, "macos/Sources/JiejieBox/App/AppModel.swift"))

	// The defaulting expression must be gone.
	if regexp.MustCompile(`activeEngine\s*\?\?\s*"classic"`).MatchString(src) {
		t.Error("the active engine still defaults to \"classic\", so an unknown " +
			"engine is displayed as Classic being active")
	}
	// The engine must be optional, so "unknown" is representable at all.
	if !regexp.MustCompile(`var activeEngine:\s*String\?`).MatchString(src) {
		t.Error("activeEngine is not optional, so an unrecognised engine cannot be " +
			"represented and must be mislabelled")
	}

	// And the screen must SAY it is unknown rather than showing nothing selected.
	mode := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/CoreModeView.swift"))
	if !strings.Contains(mode, "engineUnknown") {
		t.Error("the core mode screen does not tell the user the running engine is " +
			"unrecognised, so it shows a selection that is not running")
	}
}

// TestCustomSubscriptionNameCanBeReset covers the name-clearing path.
//
// A custom name could be set but never removed: clearing the field saved an empty
// name, and the backend re-derived a display name, so the user's custom name came
// back. The reset has to travel as an explicit instruction, because an absent name
// and "remove the name" are different requests.
func TestCustomSubscriptionNameCanBeReset(t *testing.T) {
	// The DTO must expose whether the current name is custom, or the UI cannot
	// know when offering a reset is meaningful.
	proto := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Models/Protocol.swift"))
	if !strings.Contains(proto, "hasCustomName") {
		t.Error("the subscription DTO does not report whether its name is custom, " +
			"so the UI cannot offer to reset one")
	}

	// The view must translate "the field is empty AND a custom name exists" into
	// the explicit clear instruction.
	edit := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/EditSubscriptionView.swift"))
	if !regexp.MustCompile(`clearName:`).MatchString(edit) {
		t.Error("EditSubscriptionView never requests a name reset, so an emptied " +
			"name is saved as blank and the derived name reappears")
	}
	if !strings.Contains(edit, "hasCustomName") {
		t.Error("EditSubscriptionView does not consult hasCustomName, so it cannot " +
			"distinguish 'leave the name alone' from 'remove the custom name'")
	}

	// The client must carry it, and the backend must accept it.
	client := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Services/BackendClient.swift"))
	if !strings.Contains(client, "clear_name") {
		t.Error("the client never sends clear_name, so the reset cannot reach the backend")
	}
}

// TestDeleteLastSubscriptionStillShowsReload covers UI-31.
//
// The reload prompt used to live INSIDE the "there is at least one subscription"
// branch. Deleting the LAST one therefore took it off screen at exactly the moment
// it mattered most: the state now holds no subscriptions while the config on disk —
// and the core's running config — may still contain every node they contributed.
// The screen said "No Subscriptions" and offered no way to rebuild, which reads as
// "nothing to do" about a config that is now wrong.
//
// The prompt describes the CONFIG, not the list, so its visibility must not depend
// on how many sources remain. That is a question of NESTING, not of the guard's
// text: the same `config_stale` guard placed one level deeper is exactly the defect.
// So the assertion measures the brace depth at which the prompt is emitted and
// requires it to be no deeper than the empty-state check's own depth.
func TestDeleteLastSubscriptionStillShowsReload(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/SubscriptionsView.swift"))

	emptyIdx := strings.Index(code, "model.subscriptions.isEmpty")
	if emptyIdx < 0 {
		t.Fatal("cannot find the empty-subscriptions check; the view was restructured")
	}
	promptIdx := strings.Index(code, "reloadPrompt\n")
	if promptIdx < 0 {
		t.Fatal("the view never emits reloadPrompt, so a stale config is never announced")
	}
	if promptIdx < emptyIdx {
		t.Fatal("the prompt is emitted before the subscription list is even considered")
	}

	// Brace depth at a position, counting the view builder's own braces. Both
	// positions are measured the same way, so the comparison is what matters.
	depthAt := func(pos int) int {
		depth := 0
		for i := 0; i < pos && i < len(code); i++ {
			switch code[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		return depth
	}
	// The GUARD that decides whether the prompt is shown — not the emission line,
	// which is one level deeper by construction, since the statement sits inside
	// its own `if`.
	region := code[emptyIdx:promptIdx]
	guardStart := strings.LastIndex(region, "if ")
	if guardStart < 0 {
		t.Fatal("the prompt is emitted with no guard at all")
	}
	guardAbs := emptyIdx + guardStart

	emptyDepth := depthAt(emptyIdx)
	guardDepth := depthAt(guardAbs)

	// THE PROPERTY. The guard must sit at the same level as the empty-state check
	// or SHALLOWER — i.e. in the branch that runs in BOTH cases. Nesting it inside
	// the non-empty branch puts it strictly deeper, which is the defect.
	if guardDepth > emptyDepth {
		t.Errorf("UI-31 the reload prompt's guard is nested %d level(s) deeper than the "+
			"empty-state check (%d vs %d), so it disappears when the last subscription "+
			"is deleted while the config is still stale",
			guardDepth-emptyDepth, guardDepth, emptyDepth)
	}

	guard := region[guardStart:]
	if strings.Contains(guard, "subscriptions.isEmpty") ||
		strings.Contains(guard, "subscriptions.count") {
		t.Error("UI-31 the reload prompt is gated on the subscription LIST, so deleting " +
			"the last source removes the prompt while the config is still stale")
	}
	if !strings.Contains(guard, "config_stale") {
		t.Errorf("UI-31 the reload prompt is not gated on the config being stale; "+
			"guard was: %q", strings.TrimSpace(guard))
	}

	if n := strings.Count(code, "private var reloadPrompt"); n != 1 {
		t.Errorf("reloadPrompt is declared %d times; a single definition is what keeps "+
			"its availability independent of the branch that renders", n)
	}
}
