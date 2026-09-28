package swiftlogic_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestIconOnlyButtonsCarryAnAccessibleName covers dimension 15 for the controls
// that need it MOST.
//
// A button labelled with `Text` gets its accessible name for free — SwiftUI reads
// the string. A button labelled with only `Image(systemName:)` gets NOTHING: read
// aloud it is an unnamed button, so a VoiceOver user cannot tell what it does, and
// on a dismiss control cannot tell that it is dismissible at all. Those are the
// controls where the omission is a genuine barrier rather than a nicety.
//
// The rule: every `Button` whose label contains an Image and NO Text must carry an
// explicit `accessibilityLabel`. Checked across every view, so a new icon button
// cannot quietly ship without one.
func TestIconOnlyButtonsCarryAnAccessibleName(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	views, err := filepath.Glob(filepath.Join(root, "macos/Sources/JiejieBox/Views/*.swift"))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) == 0 {
		t.Fatal("no view files found; the glob is wrong and this test would pass vacuously")
	}

	// A button's label body, captured by BALANCING BRACES rather than by matching
	// to the first closing brace at some indentation.
	//
	// The naive form was wrong in the direction that matters: `MenuToggleRow`
	// wraps its Text in a nested VStack, so a non-balancing match stopped before
	// reaching it and the row was reported as icon-only when it is not. A checker
	// that invents offenders gets muted, and a muted checker protects nothing.
	buttonStart := regexp.MustCompile(`Button\s*\{`)
	labelOpen := regexp.MustCompile(`\}\s*label:\s*\{`)

	var offenders []string
	checked := 0
	for _, path := range views {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code := swiftCode(string(raw))
		for _, loc := range buttonStart.FindAllStringIndex(code, -1) {
			// Find the `} label: {` that belongs to THIS Button, then take the
			// balanced body that follows it.
			rest := code[loc[1]:]
			lab := labelOpen.FindStringIndex(rest)
			if lab == nil {
				continue
			}
			// lab is an index into rest, which itself starts at loc[1].
			bodyOpen := loc[1] + lab[1] - 1 // the `{` itself
			body, bodyEnd := balancedBlock(code, bodyOpen)
			if body == "" {
				continue
			}
			if !strings.Contains(body, "Image(systemName") || strings.Contains(body, "Text(") {
				continue
			}
			// The modifiers follow the label's closing brace.
			tail := code[bodyEnd:]
			if len(tail) > 600 {
				tail = tail[:600]
			}
			checked++
			if !strings.Contains(tail, "accessibilityLabel") {
				line := strings.Count(code[:loc[0]], "\n") + 1
				offenders = append(offenders,
					filepath.Base(path)+":"+itoa(line))
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no icon-only buttons to check, which cannot be right: the " +
			"dismiss controls in HomeView and PanelScaffold are icon-only")
	}
	if len(offenders) > 0 {
		t.Errorf("icon-only buttons with no accessible name (read aloud as an "+
			"unnamed button): %s", strings.Join(offenders, ", "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// balancedBlock returns the text inside the brace that opens at `open` and the
// index just past its closing brace.
//
// Brace counting rather than a regex, because the bodies that matter here nest
// HStacks and VStacks, and a non-balancing match stops at the first `}`.
func balancedBlock(src string, open int) (string, int) {
	if open >= len(src) || src[open] != '{' {
		return "", 0
	}
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open+1 : i], i + 1
			}
		}
	}
	return "", 0
}
