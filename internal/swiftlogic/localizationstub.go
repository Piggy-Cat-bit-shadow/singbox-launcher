package swiftlogic

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// localizationStub generates a compilable stand-in for Localization.swift.
//
// WHY A STUB RATHER THAN THE REAL FILE. `Localization.swift` imports SwiftUI for
// its `EnvironmentKey`, and importing SwiftUI in this toolchain fails outright:
// the macro plugin `SwiftUIMacros` is not present, so the file cannot be
// compiled outside the app target. The logic under test does not depend on the
// translations themselves, only on the fact that a key EXISTS and returns a
// String.
//
// DERIVED, NOT HAND-WRITTEN. The key list and the associated-value arity are
// parsed out of the real enum, so adding a key to Localization.swift cannot make
// this stub silently stale — a hand-maintained duplicate would drift within a
// week and then fail to compile for reasons that have nothing to do with the
// test.
type localizationStub struct {
	// Source is the generated Swift text.
	Source string
	// Keys are the enum case names it defines.
	Keys []string
}

var (
	lCaseRe = regexp.MustCompile(`(?m)^    case (\w+)(\(([^)]*)\))?`)
	lEnumRe = regexp.MustCompile(`(?s)\nenum L: CaseIterable \{(.*?)\n\}`)
	lStart  = "\nenum L: CaseIterable {"
)

// buildLocalizationStub parses Localization.swift and returns a stub declaring
// the same enum.
func buildLocalizationStub(root string) (*localizationStub, error) {
	path := filepath.Join(root, "macos/Sources/JiejieBox/Models/Localization.swift")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := string(raw)
	loc := strings.Index(src, lStart)
	if loc < 0 {
		return nil, fmt.Errorf("Localization.swift: cannot find the L enum")
	}
	// The enum runs to the end of the file's top-level declaration. Matching to
	// the last closing brace at column 0 is exact for this file's layout and is
	// checked below by requiring the parsed key count to be non-trivial.
	rest := src[loc:]
	end := strings.LastIndex(rest, "\n}")
	if end < 0 {
		return nil, fmt.Errorf("Localization.swift: cannot find the end of the L enum")
	}
	body := rest[:end]

	var (
		cases []string
		out   strings.Builder
	)
	out.WriteString("// GENERATED from Localization.swift by internal/swiftlogic. Do not edit.\n")
	out.WriteString("enum Localization: String { case zhHans, en }\n")
	out.WriteString("enum L {\n")
	for _, m := range lCaseRe.FindAllStringSubmatch(body, -1) {
		name := m[1]
		params := m[3]
		cases = append(cases, name)
		if params == "" {
			out.WriteString("    case " + name + "\n")
		} else {
			out.WriteString("    case " + name + "(" + params + ")\n")
		}
	}
	if len(cases) < 100 {
		return nil, fmt.Errorf("Localization.swift: parsed only %d keys, which is too few to be right", len(cases))
	}
	out.WriteString("\n")
	// One `tr` overload per arity, so both the plain and the formatted calls
	// used across the model layer resolve. The values are not the point of the
	// harness; only that a key exists and yields a String.
	out.WriteString("    func tr(_ language: Localization) -> String {")
	out.WriteString(` switch self { case .` + cases[0] + `: return "x"`)
	out.WriteString(" default: return \"stub\" } }\n")
	out.WriteString("    func tr(_ language: Localization, _ args: CVarArg...) -> String { return \"stub\" }\n")
	out.WriteString("}\n")
	return &localizationStub{Source: out.String(), Keys: cases}, nil
}

// writeLocalizationStubFile writes the stub next to the other harness sources.
func writeLocalizationStubFile(dir, src string) (string, error) {
	path := filepath.Join(dir, "LocalizationStub.swift")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
