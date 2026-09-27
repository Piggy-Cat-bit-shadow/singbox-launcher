package locale

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// placeholderRe matches Go format verbs like %s, %d, %v, %f, %q, %x, %02d, etc.
var placeholderRe = regexp.MustCompile(`%[-+# 0]*[*]?[0-9]*[.*]?[0-9]*[vTtbcdoOqxXUeEfFgGsp%]`)

func findProjectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}
	dir := wd
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("Project root not found from %s", wd)
	return ""
}

func loadExternalLocalesForTest(t *testing.T) {
	t.Helper()
	root := findProjectRoot(t)
	localeDir := filepath.Join(root, "bin", "locale")
	LoadExternalLocales(localeDir)
}

func TestBuiltinEnglish(t *testing.T) {
	// Английский — псевдокаталог: только имя для селектора, ключи не нужны
	// (ключ и есть английский текст).
	en, ok := catalogs["en"]
	if !ok {
		t.Fatal("builtin en catalog not found")
	}
	if e, ok := en[displayNameKey]; !ok || e.Value.Text != "English" {
		t.Errorf("en._display_name = %q, want %q", e.Value.Text, "English")
	}
}

func TestExternalRussian(t *testing.T) {
	loadExternalLocalesForTest(t)

	ru, ok := catalogs["ru"]
	if !ok {
		t.Skip("ru.json not found in bin/locale/ — skipping")
	}
	if e, ok := ru[displayNameKey]; !ok || e.Value.Text != "Русский" {
		t.Errorf("ru._display_name = %q, want %q", e.Value.Text, "Русский")
	}
}

func TestNoEmptyValues(t *testing.T) {
	loadExternalLocalesForTest(t)

	for lang, msgs := range catalogs {
		for key, e := range msgs {
			if e.Value.IsZero() {
				t.Errorf("[%s] key %q has empty value", lang, key)
			}
		}
	}
}

func TestPlaceholderCount(t *testing.T) {
	loadExternalLocalesForTest(t)

	en := catalogs["en"]
	ru, ok := catalogs["ru"]
	if !ok {
		t.Skip("ru.json not found — skipping placeholder test")
	}

	for key, e := range ru {
		if key == displayNameKey {
			continue
		}
		// Эталон плейсхолдеров: значение легаси-ключа из en.json, а для
		// естественного ключа (нет в en) — сам ключ: он и есть английский
		// текст. Полная валидация — в l10n_check (SPEC 111, этап 4).
		ref := key
		if enEntry, ok := en[key]; ok && enEntry.Value.Text != "" {
			ref = enEntry.Value.Text
		}
		refCount := countPlaceholders(ref)
		for _, tmpl := range entryTemplates(e) {
			if got := countPlaceholders(tmpl); got != refCount {
				t.Errorf("key %q: reference has %d placeholder(s) (%q), translation has %d (%q)",
					key, refCount, ref, got, tmpl)
			}
		}
	}
}

func TestTFunction(t *testing.T) {
	loadExternalLocalesForTest(t)

	// Fixtures use keys the backend still ships. The previous fixtures
	// ("Start" and its special[1] form) belonged to the Fyne dashboard, which
	// was removed with the rest of the GUI, so the catalog no longer has them.
	const fixture = "Info"

	// Английский: ключ и есть текст.
	SetLang("en")
	if got := T(fixture); got != fixture {
		t.Errorf("T(%s) = %q, want %q", fixture, got, fixture)
	}

	if _, ok := catalogs["ru"]; ok {
		SetLang("ru")
		if got := T(fixture); got == fixture {
			t.Errorf("T(%s) = %q; the Russian catalog did not translate it", fixture, got)
		}
	}

	// Unknown key returns the key itself
	SetLang("en")
	if got := T("nonexistent.key"); got != "nonexistent.key" {
		t.Errorf("T(nonexistent.key) = %q, want %q", got, "nonexistent.key")
	}
}

func TestTfFunction(t *testing.T) {
	loadExternalLocalesForTest(t)
	SetLang("ru")
	defer SetLang("en")
	// Fixture is a live backend key with a format verb. The previous fixture
	// lived only in the removed Fyne UI and was pruned from the catalog.
	got := Tf("Sing-Box crashed, restarting... (attempt %d/%d)", 1, 3)
	if got == "" || got == "Sing-Box crashed, restarting... (attempt %d/%d)" {
		t.Errorf("Tf = %q; the Russian catalog did not format it", got)
	}
}

func TestLanguages(t *testing.T) {
	langs := Languages()
	if len(langs) < 1 {
		t.Errorf("expected at least 1 language, got %d", len(langs))
	}
	found := false
	for _, l := range langs {
		if l == "en" {
			found = true
		}
	}
	if !found {
		t.Error("'en' not in Languages()")
	}
}

func TestLangDisplayName(t *testing.T) {
	if got := LangDisplayName("en"); got != "English" {
		t.Errorf("LangDisplayName(en) = %q, want %q", got, "English")
	}
}

func TestLangDisplayNameFromExternal(t *testing.T) {
	loadExternalLocalesForTest(t)
	if _, ok := catalogs["ru"]; !ok {
		t.Skip("ru.json not found")
	}
	if got := LangDisplayName("ru"); got != "Русский" {
		t.Errorf("LangDisplayName(ru) = %q, want %q", got, "Русский")
	}
}

// entryTemplates collects every rendered template of an entry: the root
// value, each plural form and the same for every special form.
func entryTemplates(e Entry) []string {
	var out []string
	collect := func(v Value) {
		if v.Text != "" {
			out = append(out, v.Text)
		}
		for _, f := range v.Forms {
			out = append(out, f)
		}
	}
	collect(e.Value)
	for _, sp := range e.Special {
		collect(sp.Value)
	}
	return out
}

func countPlaceholders(s string) int {
	matches := placeholderRe.FindAllString(s, -1)
	count := 0
	for _, m := range matches {
		if m != "%%" {
			count++
		}
	}
	return count
}
