package locale

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentSettingsUpdatesDoNotLoseFields is statement 7 (§34 name).
//
// Every settings mutation in the app loads the whole file, changes one field and saves
// the whole struct back. Two of those running concurrently means the second save writes
// a snapshot taken before the first landed — so one user preference silently reverts,
// with both operations reporting success. The call sites are spread across engine
// switching, daemon pairing, background maintenance and first-run notices, so the race
// is not confined to one screen.
func TestConcurrentSettingsUpdatesDoNotLoseFields(t *testing.T) {
	dir := t.TempDir()

	// Six independent fields, each written by its own goroutine, many times.
	const rounds = 25
	type fieldUpdate struct {
		name  string
		apply func(*Settings)
		read  func(Settings) bool
	}
	updates := []fieldUpdate{
		{"AutoPingAfterConnectDisabled", func(s *Settings) { s.AutoPingAfterConnectDisabled = true },
			func(s Settings) bool { return s.AutoPingAfterConnectDisabled }},
		{"SubscriptionAutoUpdateDisabled", func(s *Settings) { s.SubscriptionAutoUpdateDisabled = true },
			func(s Settings) bool { return s.SubscriptionAutoUpdateDisabled }},
		{"CoreBackendMode", func(s *Settings) { s.CoreBackendMode = "daemon" },
			func(s Settings) bool { return s.CoreBackendMode == "daemon" }},
		{"DaemonAddress", func(s *Settings) { s.DaemonAddress = "10.0.0.5:443" },
			func(s Settings) bool { return s.DaemonAddress == "10.0.0.5:443" }},
		{"DaemonStopVPNOnExit", func(s *Settings) { s.DaemonStopVPNOnExit = true },
			func(s Settings) bool { return s.DaemonStopVPNOnExit }},
		{"DaemonSecret", func(s *Settings) { s.DaemonSecret = "s3cret" },
			func(s Settings) bool { return s.DaemonSecret == "s3cret" }},
	}

	var wg sync.WaitGroup
	for _, u := range updates {
		wg.Add(1)
		go func(u fieldUpdate) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := UpdateSettings(dir, func(s *Settings) error {
					u.apply(s)
					return nil
				}); err != nil {
					t.Errorf("update %s: %v", u.name, err)
					return
				}
			}
		}(u)
	}
	wg.Wait()

	final := LoadSettings(dir)
	for _, u := range updates {
		if !u.read(final) {
			t.Errorf("field %s was lost: after %d concurrent updates it is still at its "+
				"default, so a later writer overwrote an earlier one's change", u.name, rounds)
		}
	}
}

// TestSettingsParseFailureDoesNotOverwriteWithDefaults is statement 8 (§34 name).
//
// `LoadSettings` returns `Settings{Lang: "en"}` when the file cannot be parsed. A
// mutating path that then saves writes those defaults over the corrupt file — destroying
// the daemon pairing, engine mode, subscription identity and hardware id in exchange for
// one changed preference, silently, because the writer only ever saw defaults.
//
// A reader may degrade. A writer must refuse.
func TestSettingsParseFailureDoesNotOverwriteWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// A file that holds valuable settings but is not valid JSON (a truncated write from
	// an older build, or a user's edit gone wrong).
	corrupt := []byte(`{"lang":"ru","daemon_address":"10.0.0.9:443","core_backend_mode":"daemon",`)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := UpdateSettings(dir, func(s *Settings) error {
		s.AutoPingAfterConnectDisabled = true
		return nil
	})
	if err == nil {
		t.Fatal("a settings update against an unparseable file reported success; it " +
			"will have written defaults over the daemon pairing, engine mode and " +
			"identity that the file still contains")
	}

	// The file must be left EXACTLY as it was, so the user can recover it.
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read back: %v", rerr)
	}
	if string(after) != string(corrupt) {
		t.Errorf("the corrupt file was modified:\n got %q\nwant %q", after, corrupt)
	}
}

// TestSettingsCorruptIsDetectable — the UI needs to be able to offer a repair.
func TestSettingsCorruptIsDetectable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if SettingsCorrupt(dir) {
		t.Error("a missing settings file was reported as corrupt")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !SettingsCorrupt(dir) {
		t.Error("an unparseable settings file was not reported as corrupt, so the app " +
			"cannot offer the user a repair before something overwrites it")
	}
}

// TestSettingsMissingFileIsCreated — the positive case must keep working.
func TestSettingsMissingFileIsCreated(t *testing.T) {
	dir := t.TempDir()
	if err := UpdateSettings(dir, func(s *Settings) error {
		s.AutoPingAfterConnectDisabled = true
		s.Lang = "ru"
		return nil
	}); err != nil {
		t.Fatalf("first-run update failed: %v", err)
	}
	got := LoadSettings(dir)
	if !got.AutoPingAfterConnectDisabled || got.Lang != "ru" {
		t.Fatalf("first-run settings not persisted: %+v", got)
	}
}

// TestConcurrentSettingsFileStaysParseable — the file must never be left unparseable,
// which is what a shared staging path produces.
func TestConcurrentSettingsFileStaysParseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = UpdateSettings(dir, func(s *Settings) error {
					s.Lang = []string{"en", "ru", "de"}[n%3]
					return nil
				})
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("settings.json is not valid JSON after concurrent updates: %v\n%s", err, data)
	}
}

// TestSettingsLostUpdateWindowIsClosedByTheLock — the deterministic version.
//
// The natural race is too narrow to hit reliably (read and write are both fast), so a
// plain concurrency test passes even with the lock removed — a test that cannot fail is
// not evidence. This one widens the window with a hook and checks the property the lock
// exists to provide: while one writer is between its read and its write, no OTHER writer
// may be in the same region.
//
// The hook records how many writers are concurrently inside the critical section. With
// the lock that number is always 1; without it, the hook observes 2 and the test fails.
func TestSettingsLostUpdateWindowIsClosedByTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := UpdateSettings(dir, func(s *Settings) error { return nil }); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var inside int32
	var maxInside int32
	var mu sync.Mutex

	restore := settingsReadHookForTest
	settingsReadHookForTest = func() {
		n := atomic.AddInt32(&inside, 1)
		mu.Lock()
		if n > maxInside {
			maxInside = n
		}
		mu.Unlock()
		// Hold the window open so a second writer has every chance to enter it.
		time.Sleep(3 * time.Millisecond)
		atomic.AddInt32(&inside, -1)
	}
	defer func() { settingsReadHookForTest = restore }()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = UpdateSettings(dir, func(s *Settings) error {
					if n%2 == 0 {
						s.DaemonAddress = "10.0.0.1:443"
					} else {
						s.CoreBackendMode = "daemon"
					}
					return nil
				})
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	observed := maxInside
	mu.Unlock()

	if observed > 1 {
		t.Fatalf("%d writers were inside the settings read-modify-write at once; each "+
			"saves a snapshot taken before the others landed, so all but the last update "+
			"is silently lost", observed)
	}

	// Both field values must survive, which is the user-visible consequence.
	final := LoadSettings(dir)
	if final.DaemonAddress != "10.0.0.1:443" || final.CoreBackendMode != "daemon" {
		t.Fatalf("a settings update was lost (address=%q mode=%q)",
			final.DaemonAddress, final.CoreBackendMode)
	}
}
