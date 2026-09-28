package locale

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"singbox-launcher/internal/atomicfile"
	"singbox-launcher/internal/platform"
)

// ErrSettingsCorrupt reports that settings.json exists but cannot be parsed.
//
// It is a distinct error because the correct response depends on the direction of the
// operation. A READER can degrade to defaults and carry on. A WRITER must NOT: saving
// defaults over an unparseable file destroys whatever is in it — the daemon pairing, the
// engine mode, the subscription identity, the hardware id — in exchange for one changed
// preference, and it does so silently, because the writer only ever saw defaults.
var ErrSettingsCorrupt = errors.New("settings.json is not valid JSON")

// settingsReadHookForTest runs immediately after settings.json is read and before the
// mutation is applied.
//
// It exists because the lost-update window is otherwise too narrow to hit reliably: the
// read and the write are both fast, so a concurrency test usually passes even with the
// lock removed — a test that cannot fail is not evidence. The hook widens the window
// deterministically, so the test measures the LOCK rather than the scheduler.
var settingsReadHookForTest func()

// settingsMu serialises every settings.json transaction in this process.
//
// It must cover the LOAD as well as the save. Locking only the save still lets a writer
// persist a snapshot it read before another writer's change landed, so one of two
// updates silently disappears while both report success.
var settingsMu sync.Mutex

// UpdateSettings applies mutations to settings.json as one serialised transaction.
//
// LOAD LATEST → MUTATE → ATOMIC REPLACE, under a lock. Every settings mutation in the
// app must go through here rather than loading, editing and saving for itself: those
// call sites are spread across enginet switching, daemon pairing, background maintenance
// and the first-run notices, and each one independently reintroduces the lost update.
//
// mutate is called with the CURRENT contents, so it may assume it is the only writer.
// It returns the error to abort with, if any.
func UpdateSettings(binDir string, mutate func(*Settings) error) error {
	if mutate == nil {
		return fmt.Errorf("locale: UpdateSettings called with no mutation")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()

	path := filepath.Join(binDir, "settings.json")
	st, err := loadSettingsLocked(path)
	if err != nil {
		// FAIL CLOSED. Continuing here would save defaults over a file that holds the
		// user's daemon pairing, engine mode and identity, and the only visible effect
		// would be one preference that did change — so the loss would be discovered
		// much later, in a different part of the app, with nothing pointing back here.
		if errors.Is(err, ErrSettingsCorrupt) {
			return fmt.Errorf("locale: refusing to update settings because the existing "+
				"file cannot be parsed (%w). It was left untouched; fix or remove %s to "+
				"continue", err, path)
		}
		// A missing file is not corruption: a first run legitimately has none.
		if !os.IsNotExist(err) {
			return fmt.Errorf("locale: read settings: %w", err)
		}
	}
	if settingsReadHookForTest != nil {
		settingsReadHookForTest()
	}
	if err := mutate(&st); err != nil {
		return err
	}
	return saveSettingsLocked(binDir, st)
}

// loadSettingsLocked reads settings.json without taking the lock.
//
// Distinguishes "absent" from "unparseable", which the public reader deliberately does
// not: a reader has nothing better to do than use defaults, while a writer must refuse.
func loadSettingsLocked(path string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Settings{Lang: "en"}, nil
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("%w: %v", ErrSettingsCorrupt, err)
	}
	if s.Lang == "" {
		s.Lang = "en"
	}
	return s, nil
}

// saveSettingsLocked writes settings.json through the atomic primitive.
//
// A UNIQUE sibling temp file, not `path + ".tmp"`. The fixed name was atomic only
// against a crash: two concurrent writers truncated the same staging file and raced to
// rename it, so the file that landed could be a mixture of both settings documents.
func saveSettingsLocked(binDir string, s Settings) error {
	path := filepath.Join(binDir, "settings.json")
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("locale: marshal settings: %w", err)
	}
	if err := atomicfile.Write(path, data, platform.DefaultFileMode); err != nil {
		return fmt.Errorf("locale: write settings: %w", err)
	}
	return nil
}

// SettingsCorrupt reports whether settings.json exists but cannot be parsed.
//
// The UI uses this to offer an explicit repair rather than silently overwriting: the user
// gets to decide whether the file is worth recovering.
func SettingsCorrupt(binDir string) bool {
	_, err := loadSettingsLocked(filepath.Join(binDir, "settings.json"))
	return errors.Is(err, ErrSettingsCorrupt)
}
