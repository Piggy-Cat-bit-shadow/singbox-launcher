package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Localization invariants for the macOS frontend.
//
// These live in the Go suite for the same reason as TestSwiftTimeoutBudgets:
// this toolchain ships neither XCTest nor the Swift Testing macro plugin, so
// `swift test` cannot build at all, while the Go suite does run. A `switch` over
// the localization enum is exhaustive at COMPILE time, so a missing translation
// is already a build error — but several other things can silently degrade, and
// those are what is checked here.
//
// The properties, in order of how badly they would fail in front of a user:
//
//  1. No screen draws a hardcoded user-facing string. This is the one that rots:
//     a new row is added, someone types the English inline, it compiles, and
//     only a Chinese user ever notices.
//  2. The Chinese table is actually Chinese. A copy-paste that duplicates an
//     English value into the zh switch is invisible to the compiler and produces
//     a half-translated interface.
//  3. Typography goes through the token scale, so the panel does not drift back
//     into per-call-site font sizes.
//  4. Language choice is never inferred by inspecting rendered text.

// swiftSource reads a file from the Swift frontend.
func swiftSource(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "macos", "Sources", "JiejieBox"}, parts...)...)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("Swift sources not present: %v", err)
	}
	return string(src)
}

// swiftViewSources returns every view file's contents, keyed by base name.
func swiftViewSources(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "..", "macos", "Sources", "JiejieBox", "Views")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("Swift views not present: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".swift") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("cannot read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Skip("no Swift view sources found")
	}
	return out
}

// TestSwiftViewsHaveNoHardcodedUserText — every user-facing label must come from
// the localization table.
//
// Deliberately narrow: it matches a string literal passed directly to a
// text-drawing initializer, which is the shape a new hardcoded label actually
// takes. A broader "any English literal anywhere" rule would flag help text,
// accessibility labels and log messages, and a guard that cries wolf gets
// disabled.
func TestSwiftViewsHaveNoHardcodedUserText(t *testing.T) {
	// The one allowed literal: the product name with its version. "sing-box" is
	// a proper noun and the version is data, so neither is translatable. Keyed by
	// the literal text, because the surrounding call shape is not the point.
	allowed := map[string]bool{
		`sing-box \(core)`: true,
	}

	re := regexp.MustCompile(
		`(?:Text|MenuRow|Button|Label|DetailLine|PendingRow|confirmationDialog)\(\s*"([A-Za-z][^"]*)"`)
	for name, src := range swiftViewSources(t) {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// Localized call sites pass a key, never a literal.
			if strings.Contains(line, ".tr(language)") || strings.Contains(line, "L.") {
				continue
			}
			if allowed[m[1]] {
				continue
			}
			t.Errorf("%s draws a hardcoded string %q; route it through the L table "+
				"so it can be translated", name, m[1])
		}
	}
}

// TestSwiftChineseTableIsChinese — the zh table must contain CJK text.
//
// The compiler cannot catch a value pasted from the English table: both are
// valid `String`s. A handful of entries are legitimately not Chinese (a brand
// name, an em-dash, a URL placeholder), so those are listed explicitly rather
// than by guessing.
func TestSwiftChineseTableIsChinese(t *testing.T) {
	src := swiftSource(t, "Models", "Localization.swift")

	start := strings.Index(src, "private static func zhHans(")
	if start < 0 {
		t.Fatal("cannot find the zhHans table in Localization.swift")
	}
	zh := src[start:]

	// Entries that are the same in every language, by design.
	nonChineseOK := map[string]bool{
		"appName":        true, // product name
		"notMeasured":    true, // "—"
		"unmeasured":     true, // "—"
		"urlPlaceholder": true, // "https://…"
		"github":         true, // brand
		"telegram":       true, // brand
	}

	re := regexp.MustCompile(`case \.(\w+): return "([^"]*)"`)
	seen := 0
	for _, m := range re.FindAllStringSubmatch(zh, -1) {
		key, value := m[1], m[2]
		seen++
		if nonChineseOK[key] {
			continue
		}
		if !containsCJK(value) {
			t.Errorf("zh translation for %q is %q, which contains no Chinese "+
				"characters: an English value pasted into the zh table leaves a "+
				"half-translated interface", key, value)
		}
	}
	if seen == 0 {
		t.Fatal("parsed no entries from the zhHans table; the parser and the " +
			"table shape have drifted apart")
	}
}

// containsCJK reports whether s has any Han character.
func containsCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// TestSwiftEnumsCarryBothTables — the two tables must cover the same keys.
//
// A key present in one and missing from the other is a compile error today,
// because the switch is exhaustive. This guard exists for the case where someone
// "fixes" that by adding a default branch: the compiler would go quiet and the
// missing key would fall through to whatever the default says.
func TestSwiftEnumsCarryBothTables(t *testing.T) {
	src := swiftSource(t, "Models", "Localization.swift")

	enAt := strings.Index(src, "private static func en(")
	zhAt := strings.Index(src, "private static func zhHans(")
	if enAt < 0 || zhAt < 0 || zhAt < enAt {
		t.Fatal("cannot locate both localization tables")
	}
	en, zh := src[enAt:zhAt], src[zhAt:]

	re := regexp.MustCompile(`case \.(\w+): return`)
	keys := func(body string) map[string]bool {
		set := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			set[m[1]] = true
		}
		return set
	}
	enKeys, zhKeys := keys(en), keys(zh)

	for k := range enKeys {
		if !zhKeys[k] {
			t.Errorf("key %q is translated in English but not in Chinese", k)
		}
	}
	for k := range zhKeys {
		if !enKeys[k] {
			t.Errorf("key %q is translated in Chinese but not in English", k)
		}
	}

	// A default branch would defeat the exhaustiveness that makes this table
	// safe, so its presence is itself a failure.
	for name, body := range map[string]string{"en": en, "zhHans": zh} {
		if regexp.MustCompile(`(?m)^\s*default:\s*$`).MatchString(body) {
			t.Errorf("the %s table has a default branch: a switch over the key "+
				"enum must stay exhaustive so a missing translation is a build error", name)
		}
	}
}

// TestSwiftViewsUseTypographyTokens — text goes through the token scale.
//
// The panel previously chose font sizes per call site, which is why rows that
// should have matched differed by a point. A semantic font (`.caption`,
// `.callout`) or a raw point size in a view reintroduces exactly that drift.
func TestSwiftViewsUseTypographyTokens(t *testing.T) {
	// Raw sizes are allowed only for decorative glyphs: an icon is sized to the
	// icon grid, not to the text scale.
	dimRe := regexp.MustCompile(`\.font\(\.system\(size: \d+`)
	iconLineRe := regexp.MustCompile(`Image\(systemName:`)
	semanticRe := regexp.MustCompile(`\.font\(\.(caption|caption2|callout|footnote|headline|title|title2|title3|body|subheadline|largeTitle)\b`)

	for name, src := range swiftViewSources(t) {
		if name == "Typography.swift" {
			continue
		}
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if semanticRe.MatchString(line) {
				t.Errorf("%s:%d uses a semantic font (%s); use a Typography token "+
					"so the scale stays reviewable in one place",
					name, i+1, strings.TrimSpace(line))
			}
			if dimRe.MatchString(line) && !iconLineRe.MatchString(line) {
				// A raw size is fine on the same line as an Image(systemName:),
				// and in the ProgressView/control sizing cases, which are not
				// text at all.
				if strings.Contains(line, "ProgressView") || strings.Contains(line, "controlSize") {
					continue
				}
				t.Errorf("%s:%d sets a raw font size (%s); use a Typography token",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestSwiftLocalizationTableIsTotal — every key resolves in both languages, and
// the enum is enumerable.
//
// The companion to the CJK check above: that one proves the Chinese values are
// Chinese, this one proves no key is keyed to an empty string in either table.
// An empty value renders as a blank label, which looks like a layout bug rather
// than a missing translation.
//
// The enum must stay CaseIterable so the table can be walked as a whole; a
// second hand-maintained key list would drift from the enum it describes.
func TestSwiftLocalizationTableIsTotal(t *testing.T) {
	src := swiftSource(t, "Models", "Localization.swift")

	if !strings.Contains(src, "enum L: CaseIterable") {
		t.Error("L is no longer CaseIterable: the table cannot be enumerated as a whole")
	}

	enAt := strings.Index(src, "private static func en(")
	zhAt := strings.Index(src, "private static func zhHans(")
	if enAt < 0 || zhAt < 0 || zhAt < enAt {
		t.Fatal("cannot locate both localization tables")
	}
	re := regexp.MustCompile(`case \.(\w+): return "([^"]*)"`)
	check := func(name, body string) {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if strings.TrimSpace(m[2]) == "" {
				t.Errorf("%s translation for %q is empty: it would render as a "+
					"blank label", name, m[1])
			}
		}
	}
	check("en", src[enAt:zhAt])
	check("zhHans", src[zhAt:])
}

// TestSwiftSpacingRhythmIsMonotonic — the three grouping gaps must stay
// distinct and correctly ordered.
//
// The whole page rhythm rests on three steps:
//
//	rowGap (within a group) < headerToRowGap (header to its own rows)
//	                        < groupSpacing (between groups)
//
// If two of them become equal the grouping stops being legible, and if the
// order inverts the header visually detaches from the rows it labels and joins
// the section above it. Neither failure throws or logs — the page just looks
// slightly wrong — which is exactly why it needs a guard.
func TestSwiftSpacingRhythmIsMonotonic(t *testing.T) {
	src := swiftSource(t, "Views", "Typography.swift")

	// The gaps live in Typography.swift; rowHeight still lives in MenuRow.swift.
	values := map[string]float64{}
	for _, name := range []string{"headerToRowGap", "groupSpacing", "contentTopPadding", "contentBottomPadding"} {
		re := regexp.MustCompile(name + `(?:: CGFloat)? = (\d+)`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Errorf("cannot find Metrics.%s in Typography.swift", name)
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(m[1], "%f", &v); err == nil {
			values[name] = v
		}
	}
	rowSrc := swiftSource(t, "Views", "MenuRow.swift")
	if m := regexp.MustCompile(`rowGap(?:: CGFloat)? = (\d+)`).FindStringSubmatch(src); m != nil {
		var v float64
		_, _ = fmt.Sscanf(m[1], "%f", &v)
		values["rowGap"] = v
	}
	if m := regexp.MustCompile(`rowHeight: CGFloat = (\d+)`).FindStringSubmatch(rowSrc); m != nil {
		var v float64
		_, _ = fmt.Sscanf(m[1], "%f", &v)
		values["rowHeight"] = v
	}

	need := []string{"rowGap", "headerToRowGap", "groupSpacing", "rowHeight"}
	for _, n := range need {
		if _, ok := values[n]; !ok {
			t.Fatalf("cannot determine Metrics.%s; the spacing tokens have been renamed", n)
		}
	}

	if !(values["rowGap"] < values["headerToRowGap"]) {
		t.Errorf("rowGap (%.0f) must be smaller than headerToRowGap (%.0f)",
			values["rowGap"], values["headerToRowGap"])
	}
	if !(values["headerToRowGap"] < values["groupSpacing"]) {
		t.Errorf("headerToRowGap (%.0f) must be smaller than groupSpacing (%.0f), "+
			"or a header sits as far from its own rows as from the previous section",
			values["headerToRowGap"], values["groupSpacing"])
	}
	// A row must stay a comfortable target even after compaction.
	if values["rowHeight"] < 30 {
		t.Errorf("rowHeight = %.0f, below the 30pt floor: compaction must not "+
			"shrink the click target", values["rowHeight"])
	}
}

// TestSwiftPagesUseTheSpacingTokens — a PAGE must not invent its own rhythm.
//
// The pages are the views that sit directly under PanelScaffold and stack
// MenuSections. Each one has exactly one such stack, and it must use
// Metrics.groupSpacing: that single value is what makes section-to-section
// distance identical on every screen. When pages chose their own (8 here, 14
// there, 10 elsewhere) the panel read as a set of unrelated screens.
//
// Deliberately NOT a blanket check on every VStack: a title-over-subtitle pair
// is legitimately spacing 1, and flagging those would bury the real signal in
// noise — the failure mode that gets a guard disabled.
func TestSwiftPagesUseTheSpacingTokens(t *testing.T) {
	pages := []string{
		"MoreView.swift", "HomeView.swift", "SubscriptionsView.swift",
		"ProxiesView.swift", "CoreDetailsView.swift", "CoreModeView.swift",
		"DaemonView.swift", "DaemonPairView.swift", "AddSubscriptionView.swift",
		"EditSubscriptionView.swift", "AboutView.swift",
	}
	sources := swiftViewSources(t)
	// A page stack is the one that directly contains MenuSection calls.
	reStack := regexp.MustCompile(`VStack\(alignment: \.leading, spacing: (\d+)\)`)

	for _, name := range pages {
		src, ok := sources[name]
		if !ok {
			t.Errorf("page %s not found", name)
			continue
		}
		for i, line := range strings.Split(src, "\n") {
			if !reStack.MatchString(line) {
				continue
			}
			// Only a stack whose own body opens a MenuSection is a page stack.
			rest := strings.Join(strings.Split(src, "\n")[i:min(i+6, len(strings.Split(src, "\n")))], "\n")
			if !strings.Contains(rest, "MenuSection(") {
				continue
			}
			if !strings.Contains(line, "Metrics.groupSpacing") {
				t.Errorf("%s:%d page section stack uses a hardcoded spacing (%s); "+
					"use Metrics.groupSpacing so every page shares one rhythm",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestSwiftLanguageIsNotInferredFromText — the language must come from the
// stored preference, never from inspecting a rendered string.
//
// Comparing against an English label is the failure mode this prevents: it works
// in English and silently picks the wrong branch in every other language. Two
// real instances were fixed during the redesign (the core-mode rows compared
// against "Daemon"/"Classic", and the daemon status row against "Active").
func TestSwiftLanguageIsNotInferredFromText(t *testing.T) {
	// Rendered English labels that must never appear in a comparison.
	forbidden := []string{`== "Daemon"`, `== "Classic"`, `== "Active"`,
		`== "Ready"`, `== "Not paired"`, `"Daemon" ==`, `"Classic" ==`}

	for name, src := range swiftViewSources(t) {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, f := range forbidden {
				if strings.Contains(line, f) {
					t.Errorf("%s:%d compares against a rendered label (%s); "+
						"branch on a protocol identifier instead",
						name, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestSwiftSectionHeaderAlignsToTitleColumn — the section header must sit on the
// ROW TITLE column, and that column must be defined exactly once.
//
// The reported defect was a header parked over the icon column instead. It was
// wrong for a reason a reviewer cannot see from a single screenshot: the reader
// compares a header with the WORDS under it, so a header aligned to the icon
// starts one icon-plus-gap too far left and reads as a caption belonging to
// nothing. Worse, the row types had drifted to two different icon-to-text gaps
// (8pt in MenuRow/MenuToggleRow, 10pt in MenuRow's other variants), so "the
// title column" was not one column at all and no single header inset could match
// every row.
//
// So this asserts both halves: the derived formula, and that every icon+text row
// shares one spacing token. A raw `HStack(spacing: 10)` in a row would silently
// re-split the column and re-break the alignment.
func TestSwiftSectionHeaderAlignsToTitleColumn(t *testing.T) {
	src := swiftSource(t, "Views", "Typography.swift")

	// The inset must be DERIVED from the three row tokens, not a literal: a
	// hardcoded number stops matching the moment any row's icon geometry moves.
	m := regexp.MustCompile(`sectionHeaderInset: CGFloat \{([^}]*)\}`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("Metrics.sectionHeaderInset is no longer a computed property; " +
			"a literal cannot stay in step with the row geometry")
	}
	expr := strings.Join(strings.Fields(m[1]), " ")
	for _, token := range []string{"rowPaddingH", "iconColumn", "iconToTextSpacing"} {
		if !strings.Contains(expr, token) {
			t.Errorf("sectionHeaderInset = {%s} does not use %s; the title column "+
				"must be derived from the tokens the rows themselves use", expr, token)
		}
	}

	// The single spacing token must exist...
	sp := regexp.MustCompile(`iconToTextSpacing: CGFloat = (\d+)`).FindStringSubmatch(src)
	if sp == nil {
		t.Fatal("Metrics.iconToTextSpacing is missing")
	}

	// ...and every icon+text row must use it, with the icon in the shared column.
	rowFiles := []string{"MenuRow.swift", "MenuToggleRow.swift", "ActionRow.swift"}
	for _, name := range rowFiles {
		rowSrc := swiftSource(t, "Views", name)
		if !strings.Contains(rowSrc, "Metrics.iconToTextSpacing") {
			t.Errorf("%s does not use Metrics.iconToTextSpacing: its title column "+
				"can drift from the header and from the other rows", name)
		}
		if !strings.Contains(rowSrc, "Metrics.iconColumn") {
			t.Errorf("%s does not size its icon with Metrics.iconColumn", name)
		}
		// A raw spacing on the ICON+TEXT stack is the drift this test exists to
		// catch. Scoped to stacks that actually contain the icon: a container
		// stacking sibling buttons (HStack(spacing: 0)) is not a title column,
		// and flagging it would bury the real signal.
		lines := strings.Split(rowSrc, "\n")
		for i, line := range lines {
			if !regexp.MustCompile(`HStack\(spacing: \d+\)`).MatchString(line) {
				continue
			}
			// Look ahead a few lines for the icon that makes this a title column.
			window := ""
			for j := i + 1; j < len(lines) && j <= i+4; j++ {
				window += lines[j]
			}
			if !strings.Contains(window, "systemImage") && !strings.Contains(window, "Metrics.iconColumn") {
				continue
			}
			t.Errorf("%s:%d uses a raw HStack spacing (%s); icon rows must share "+
				"Metrics.iconToTextSpacing or the title column splits in two",
				name, i+1, strings.TrimSpace(line))
		}
	}
}

// TestSwiftNeverInfersOwnershipFromRebuildable — the provenance copy guard.
//
// `config_rebuildable == false` means "do not rebuild", which is NOT the same
// statement as "another tool owns this file". Reading one as the other is what
// told users that a config their own JiejieBox had written was foreign, because
// a config written before provenance markers existed has no marker.
//
// The UI must therefore branch on configOwnership, and the external sentence
// must be reachable only from the `.external` case.
func TestSwiftNeverInfersOwnershipFromRebuildable(t *testing.T) {
	home := swiftSource(t, "Views", "HomeView.swift")

	// The ownership switch must be the thing that selects the message.
	if !strings.Contains(home, "model.configOwnership") {
		t.Error("HomeView does not consult model.configOwnership; ownership must " +
			"come from the explicit field, never from a permission flag")
	}
	if !strings.Contains(home, "configChangedUnknown") {
		t.Error("HomeView has no UNKNOWN branch, so an unverified config would be " +
			"reported as external again")
	}

	// The external copy must not be selected by `configRebuildable`.
	if regexp.MustCompile(`configRebuildable\s*\?\s*L\.configChanged\b`).MatchString(home) {
		t.Error("HomeView still chooses its message from configRebuildable")
	}
	for i, line := range strings.Split(home, "\n") {
		if strings.Contains(line, "configChangedExternal") &&
			strings.Contains(line, "configRebuildable") {
			t.Errorf("HomeView:%d ties the external message to configRebuildable", i+1)
		}
	}

	// The model's ownership accessor must default to UNKNOWN, not external: an
	// older backend that omits the field must not be read as an accusation.
	model := swiftSource(t, "App", "AppModel.swift")
	if regexp.MustCompile(`config_ownership[^\n]*\?\?\s*\.external`).MatchString(model) {
		t.Error("an absent config_ownership falls back to .external; the safe " +
			"default is .unknown")
	}
}
