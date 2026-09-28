package swiftlogic_test

import (
	"strings"
	"testing"
)

// TestStaleServiceExplainsItself covers the visible-with-a-reason requirement.
//
// `needs_install` is true in two different situations: nothing is installed, and
// the installed service no longer matches this app. They are different problems
// — one is routine setup, the other means the service that would receive the
// config is not the one this build expects — and a single generic subtitle hides
// the difference from the user.
//
// The backend already distinguishes them and sends `service_detail`. This checks
// the view surfaces it rather than re-deriving the reason, so what the user reads
// is what the launcher acted on.
func TestStaleServiceExplainsItself(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/DaemonView.swift"))

	// The install step must not hard-code the generic subtitle any more.
	if strings.Contains(code, "subtitle: L.installServiceSubtitle.tr(language)") {
		t.Error("the install step still shows a generic subtitle, so a STALE service " +
			"reads exactly like a first-time install")
	}
	if !strings.Contains(code, "installSubtitle(for:") {
		t.Error("the install step must derive its subtitle from the status")
	}

	// And the derivation must actually consult the backend's detail.
	idx := strings.Index(code, "func installSubtitle(")
	if idx < 0 {
		t.Fatal("installSubtitle is missing")
	}
	body := code[idx:]
	if end := strings.Index(body[1:], "\n    /// "); end > 0 {
		body = body[:end+1]
	} else if len(body) > 1200 {
		body = body[:1200]
	}
	if !strings.Contains(body, "service_detail") {
		t.Error("installSubtitle must use the backend's service_detail; " +
			"re-deriving the reason lets the shown text drift from the real one")
	}
	if !strings.Contains(body, "status.installed") {
		t.Error("installSubtitle must distinguish 'not installed' from 'installed but stale'")
	}
	if !strings.Contains(body, "serviceUpdateDetail") {
		t.Error("installSubtitle must use the localized stale-service explanation")
	}
}

// TestStaleServiceCopyExistsInBothLanguages — a new key with no translation is a
// regression the l10n guard cannot see, because it checks keys rather than
// meaning.
func TestStaleServiceCopyExistsInBothLanguages(t *testing.T) {
	loc := repoFile(t, "macos/Sources/JiejieBox/Models/Localization.swift")
	if !strings.Contains(loc, "case serviceUpdateDetail") {
		t.Fatal("the serviceUpdateDetail key is missing from the catalog")
	}
	if n := strings.Count(loc, "case .serviceUpdateDetail:"); n < 2 {
		t.Errorf("serviceUpdateDetail has %d translations; both EN and ZH are required", n)
	}
	// An English string left in the ZH branch is the usual way this goes wrong.
	for _, line := range strings.Split(loc, "\n") {
		if !strings.Contains(line, "case .serviceUpdateDetail:") {
			continue
		}
		if strings.Contains(line, `return "The installed service`) {
			continue // the English branch
		}
		if !strings.ContainsAny(line, "服务") {
			t.Errorf("the non-English branch does not look translated: %s", strings.TrimSpace(line))
		}
	}
}
