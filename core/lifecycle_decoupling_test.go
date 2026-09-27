//go:build darwin || (windows && !386)

package core

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"singbox-launcher/core/services"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/lxdclient"
	"singbox-launcher/internal/paths"
)

// SPEC 150: жизненный цикл GUI отвязан от жизненного цикла ядра VPN.
//
// Проверяется НАСТОЯЩИЙ DaemonBackend поверх httptest-«демона»: тесты
// считают фактические HTTP-запросы, поэтому утверждение «Stop не отправлен»
// означает, что запрос действительно не ушёл, а не что заглушка не позвалась.
//
// Регрессии, которые здесь закрыты:
//   - выход из лаунчера в daemon-режиме НЕ должен посылать /admin/stop
//     (раньше это делал OnAppExit по умолчанию и рвал живой туннель);
//   - Close() (смена движка, закрытие окна) не должен трогать ядро;
//   - повторный запуск лаунчера должен ПРИСОЕДИНЯТЬСЯ к живому ядру, а не
//     перезапускать его;
//   - classic-режим обязан остаться прежним: Stop при выходе + ожидание.

// fakeDaemon — минимальный admin-сервер демона: /admin/status, /admin/stop,
// /admin/start, /admin/apply. Считает вызовы, чтобы тест мог утверждать
// «остановки не было».
type fakeDaemon struct {
	srv *httptest.Server

	mu      sync.Mutex
	status  string
	stops   int32
	starts  int32
	applies int32
}

func newFakeDaemon(t *testing.T, status string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{status: status}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		switch r.URL.Path {
		case "/admin/status":
			st := d.status
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"` + st + `"}`))
			return
		case "/admin/stop":
			d.stops++
			d.status = "idle"
		case "/admin/start":
			d.starts++
			d.status = "started"
		case "/admin/apply":
			d.applies++
			d.status = "started"
		default:
			d.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) counts() (stops, starts, applies int32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stops, d.starts, d.applies
}

func (d *fakeDaemon) setStatus(s string) {
	d.mu.Lock()
	d.status = s
	d.mu.Unlock()
}

// newTestDaemonBackend собирает AppController с РЕАЛЬНОЙ раскладкой во
// временном каталоге (settings.json читается с диска — именно оттуда
// OnAppExit берёт DaemonStopVPNOnExit) и настоящий DaemonBackend,
// нацеленный на фейковый демон.
func newTestDaemonBackend(t *testing.T, d *fakeDaemon, stopVPNOnExit bool) (*AppController, *DaemonBackend) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	binDir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := locale.SaveSettings(binDir, locale.Settings{Lang: "en", DaemonStopVPNOnExit: stopVPNOnExit}); err != nil {
		t.Fatal(err)
	}

	ac := &AppController{
		FileService: &services.FileService{
			Layout: paths.Layout{
				Data: paths.DataDir(dataDir),
				App:  paths.AppDir(filepath.Join(root, "app")),
				Logs: paths.LogDir(filepath.Join(root, "logs")),
			},
		},
	}
	// RunningState — указатель; NewAppController создаёт его до UIService.
	// Здесь UI нет, поэтому инициализируем ровно то, что читает логика.
	ac.RunningState = &RunningState{controller: ac}
	ac.RunningState.Set(false)
	b := &DaemonBackend{ac: ac, admin: lxdclient.New(lxdclient.Config{Addr: d.srv.Listener.Addr().String()})}
	ac.backend = b
	installTestController(t, ac)
	return ac, b
}

// installTestController подменяет глобальный синглтон, через который
// пакетные обёртки (EnsureVPNRunning, StartSingBoxProcess) находят контроллер,
// и возвращает прежний на выходе из теста. Без этого пакетные обёртки
// работали бы с nil-контроллером и тест «ничего не отправил» проходил бы
// по неверной причине.
func installTestController(t *testing.T, ac *AppController) {
	t.Helper()
	prev := instance
	instance = ac
	t.Cleanup(func() { instance = prev })
}

// TestDaemonExitKeepsCoreRunningByDefault — SPEC 150 §2: при настройке по
// умолчанию (Keep VPN running after quit = On) выход из лаунчера не должен
// отправлять демону /admin/stop.
func TestDaemonExitKeepsCoreRunningByDefault(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, b := newTestDaemonBackend(t, d, false)
	// Сценарий: ядро работает (лаунчер к нему уже присоединился).
	ac.RunningState.Set(true)

	if !b.PersistsAfterAppExit() {
		t.Fatal("PersistsAfterAppExit() = false with DaemonStopVPNOnExit=false; the launcher would tear the tunnel down on quit")
	}
	if !ac.CorePersistsAfterAppExit() {
		t.Fatal("CorePersistsAfterAppExit() = false; UI would claim the VPN stops on exit")
	}

	waitForStop := b.OnAppExit()

	stops, starts, applies := d.counts()
	if stops != 0 {
		t.Fatalf("OnAppExit sent %d /admin/stop, want 0: quitting the GUI must not disconnect the VPN", stops)
	}
	if starts != 0 || applies != 0 {
		t.Fatalf("OnAppExit sent start=%d apply=%d, want 0/0: the running core must be left untouched", starts, applies)
	}
	if waitForStop {
		t.Fatal("OnAppExit() = true; the caller would block waiting for a core that is supposed to keep running")
	}
	if !ac.RunningState.IsRunning() {
		t.Fatal("RunningState was cleared on exit; the core is still running, the UI state lied")
	}
}

// TestDaemonExitStopsCoreWhenUserOptedIn — обратная сторона: пользователь
// явно попросил «Stop VPN when quitting», и тогда остановка обязана уйти
// демону ровно один раз.
func TestDaemonExitStopsCoreWhenUserOptedIn(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, b := newTestDaemonBackend(t, d, true)
	ac.RunningState.Set(true)

	if b.PersistsAfterAppExit() {
		t.Fatal("PersistsAfterAppExit() = true with DaemonStopVPNOnExit=true; the setting was ignored")
	}

	waitForStop := b.OnAppExit()

	stops, _, _ := d.counts()
	if stops != 1 {
		t.Fatalf("/admin/stop count = %d, want exactly 1", stops)
	}
	if !waitForStop {
		t.Fatal("OnAppExit() = false after an explicit stop; the caller would not wait for teardown")
	}
	if ac.RunningState.IsRunning() {
		t.Fatal("RunningState still true after the core was told to stop")
	}
}

// TestDaemonCloseNeverTouchesCore — SPEC 150 §4: Close() освобождает только
// клиентские ресурсы лаунчера (смена движка, закрытие окна). Ядро при этом
// трогать нельзя ни при какой настройке.
func TestDaemonCloseNeverTouchesCore(t *testing.T) {
	for _, stopOnExit := range []bool{false, true} {
		d := newFakeDaemon(t, "started")
		_, b := newTestDaemonBackend(t, d, stopOnExit)

		b.Close()

		// Close() останавливает супервизоры; даём им шанс добежать, чтобы
		// запоздавший запрос не спрятался за гонкой.
		time.Sleep(50 * time.Millisecond)

		stops, starts, applies := d.counts()
		if stops != 0 || starts != 0 || applies != 0 {
			t.Fatalf("Close() (stopOnExit=%v) sent stop=%d start=%d apply=%d, want all 0", stopOnExit, stops, starts, applies)
		}
	}
}

// TestEnsureVPNRunningAttachesToLiveCore — SPEC 150 §7: `-start` при уже
// работающем демоне обязан быть no-op. Это закрывает гонку, из-за которой
// первый кадр статуса не успевал прийти за autoStartDelay и лаунчер
// перезапускал живое ядро.
func TestEnsureVPNRunningAttachesToLiveCore(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, _ := newTestDaemonBackend(t, d, false)

	// Локальное состояние ещё «не работает» — как сразу после старта GUI.
	ac.RunningState.Set(false)

	if EnsureVPNRunning() {
		t.Fatal("EnsureVPNRunning() = true; the launcher would restart a core that is already serving traffic")
	}
	stops, starts, applies := d.counts()
	if stops != 0 || starts != 0 || applies != 0 {
		t.Fatalf("attach path sent stop=%d start=%d apply=%d, want all 0", stops, starts, applies)
	}
}

// TestEnsureVPNRunningStartsWhenCoreIsDown — если демон действительно пуст
// (idle), автостарт обязан сработать: иначе decoupling превратился бы в
// «VPN больше никогда не поднимается сам».
//
// Проверяется именно решение (true = запуск инициирован), а не доехавший до
// демона apply: apply уходит в горутине и собирает config.json, что к
// предмету теста не относится.
func TestEnsureVPNRunningStartsWhenCoreIsDown(t *testing.T) {
	d := newFakeDaemon(t, "idle")
	ac, _ := newTestDaemonBackend(t, d, false)
	ac.RunningState.Set(false)

	if !EnsureVPNRunning() {
		t.Fatal("EnsureVPNRunning() = false while the daemon is idle; auto-start would be dead")
	}
}

// TestEnsureVPNRunningTreatsUnreachableDaemonAsNotRunning — недоступный демон
// не должен блокировать автостарт: «неизвестно» — это не «работает».
// Иначе один неотвечающий сокет навсегда отключал бы автоподъём VPN.
func TestEnsureVPNRunningTreatsUnreachableDaemonAsNotRunning(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, _ := newTestDaemonBackend(t, d, false)
	ac.RunningState.Set(false)
	d.srv.Close() // демон «пропал»

	if !EnsureVPNRunning() {
		t.Fatal("EnsureVPNRunning() = false with an unreachable daemon; a dead socket must not veto auto-start")
	}
}

// TestEnsureVPNRunningSkipsWhenAlreadyRunningLocally — если состояние уже
// известно как «работает», лишняя проба демона не нужна и старт не шлётся.
func TestEnsureVPNRunningSkipsWhenAlreadyRunningLocally(t *testing.T) {
	d := newFakeDaemon(t, "idle")
	ac, _ := newTestDaemonBackend(t, d, false)
	ac.RunningState.Set(true)

	if EnsureVPNRunning() {
		t.Fatal("EnsureVPNRunning() = true while RunningState is already true")
	}
	_, starts, applies := d.counts()
	if starts != 0 || applies != 0 {
		t.Fatalf("start=%d apply=%d, want 0/0", starts, applies)
	}
}

// TestCoreRunningOnDaemonMapsStatuses — проба демона: STARTED/STARTING/
// STOPPING считаются «ядро есть» (reload не должен выглядеть как падение),
// а ошибка связи — «неизвестно», чтобы вызывающий не принимал недоступный
// демон за выключенный VPN.
func TestCoreRunningOnDaemonMapsStatuses(t *testing.T) {
	for _, tc := range []struct {
		status  string
		running bool
		known   bool
	}{
		{"started", true, true},
		{"starting", true, true},
		{"stopping", true, true},
		{"idle", false, true},
	} {
		d := newFakeDaemon(t, tc.status)
		_, b := newTestDaemonBackend(t, d, false)

		running, known := b.CoreRunningOnDaemon()
		if running != tc.running || known != tc.known {
			t.Fatalf("status %q: CoreRunningOnDaemon() = (%v, %v), want (%v, %v)", tc.status, running, known, tc.running, tc.known)
		}
	}

	// Демон недоступен: known=false, чтобы вызывающий не делал выводов.
	d := newFakeDaemon(t, "started")
	_, b := newTestDaemonBackend(t, d, false)
	d.srv.Close()
	if running, known := b.CoreRunningOnDaemon(); known {
		t.Fatalf("unreachable daemon reported as known (running=%v); a dead socket would be read as 'VPN is off'", running)
	}
}

// TestLegacyExitStillStopsCoreAndWaits — SPEC 150 §23: classic-режим не
// изменился. Ядро принадлежит лаунчеру, поэтому выход обязан его погасить и
// попросить вызывающего подождать.
//
// Идём через настоящий ProcessService: LegacyBackend.OnAppExit делегирует
// именно ему, а не шву legacyOps (тот покрывает fire-and-forget Start/Stop
// из UI). Шов stopOverrideForTest не даёт тесту тронуть живой VPN на машине.
func TestLegacyExitStillStopsCoreAndWaits(t *testing.T) {
	var stopped atomic.Int32
	ac := &AppController{}
	ac.RunningState = &RunningState{controller: ac}
	ac.RunningState.Set(false)
	ps := NewProcessService(ac)
	ps.stopOverrideForTest = func() { stopped.Add(1) }
	ac.ProcessService = ps
	b := &LegacyBackend{ac: ac}

	if b.PersistsAfterAppExit() {
		t.Fatal("PersistsAfterAppExit() = true for the classic backend; it would leak an orphan sing-box on quit")
	}
	if ac.CorePersistsAfterAppExit() {
		t.Fatal("CorePersistsAfterAppExit() = true for the classic backend")
	}

	if waitForStop := b.OnAppExit(); !waitForStop {
		t.Fatal("LegacyBackend.OnAppExit() = false; the caller would not wait for the child core to die")
	}
	if !waitFor(2*time.Second, func() bool { return stopped.Load() == 1 }) {
		t.Fatalf("classic stop count = %d, want exactly 1: quitting must stop the launcher-owned core", stopped.Load())
	}
}

// TestLegacyExitWithoutProcessServiceDoesNotWait — страховка от «повисшего»
// выхода: если ProcessService не построен, ждать нечего, и OnAppExit обязан
// сказать об этом честно (false), а не заставить вызывающего ждать впустую.
func TestLegacyExitWithoutProcessServiceDoesNotWait(t *testing.T) {
	ac := &AppController{}
	ac.RunningState = &RunningState{controller: ac}
	b := &LegacyBackend{ac: ac}

	if b.OnAppExit() {
		t.Fatal("LegacyBackend.OnAppExit() = true without a ProcessService; the caller would wait for nothing")
	}
}

// TestPersistentCoreArmedDrivesWatchdogWording — SPEC 150 §14: принудительный
// выход в daemon-режиме не орёт про «ядро осталось», потому что ядро и
// ДОЛЖНО остаться; в classic — обязан предупредить.
func TestPersistentCoreArmedDrivesWatchdogWording(t *testing.T) {
	acDaemon, _ := newTestDaemonBackend(t, newFakeDaemon(t, "started"), false)
	if !acDaemon.persistentCoreArmed() {
		t.Fatal("persistentCoreArmed() = false in daemon mode with keep-running on; the watchdog would warn about a normal state")
	}

	acDaemonStop, _ := newTestDaemonBackend(t, newFakeDaemon(t, "started"), true)
	if acDaemonStop.persistentCoreArmed() {
		t.Fatal("persistentCoreArmed() = true while the user asked to stop the VPN on exit")
	}

	var warned, informed atomic.Int32
	restore := forceExitHook
	forceExitHook = func(ac *AppController, phase string, _ time.Duration) {
		if ac.persistentCoreArmed() {
			informed.Add(1)
		} else {
			warned.Add(1)
		}
	}
	t.Cleanup(func() { forceExitHook = restore })

	acDaemon.forceExitHook("teardown", time.Second)
	if informed.Load() != 1 || warned.Load() != 0 {
		t.Fatalf("daemon watchdog: informed=%d warned=%d, want 1/0", informed.Load(), warned.Load())
	}

	legacyAC := &AppController{}
	legacyAC.backend = &LegacyBackend{ac: legacyAC}
	legacyAC.forceExitHook("teardown", time.Second)
	if warned.Load() != 1 {
		t.Fatalf("classic watchdog: warned=%d, want 1 (a forced exit there really does orphan the core)", warned.Load())
	}
}

// TestKeepRunningSettingInvertsToStoredField — SPEC 150 §10: UI показывает
// ПОЛОЖИТЕЛЬНЫЙ чекбокс, а на диск по-прежнему пишется DaemonStopVPNOnExit.
// Инверсия — единственное место, где легко ошибиться на единицу, поэтому
// она закреплена здесь.
func TestKeepRunningSettingInvertsToStoredField(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Чекбокс включён → ядро остаётся → Stop-on-exit выключен.
	if err := locale.SaveSettings(binDir, locale.Settings{Lang: "en", DaemonStopVPNOnExit: false}); err != nil {
		t.Fatal(err)
	}
	st := locale.LoadSettings(binDir)
	if keepRunning := !st.DaemonStopVPNOnExit; !keepRunning {
		t.Fatal("stored DaemonStopVPNOnExit=false must render the checkbox CHECKED")
	}

	// Чекбокс выключен → ядро гасится → Stop-on-exit включён.
	if err := locale.SaveSettings(binDir, locale.Settings{Lang: "en", DaemonStopVPNOnExit: true}); err != nil {
		t.Fatal(err)
	}
	st = locale.LoadSettings(binDir)
	if keepRunning := !st.DaemonStopVPNOnExit; keepRunning {
		t.Fatal("stored DaemonStopVPNOnExit=true must render the checkbox UNCHECKED")
	}
}

// TestGracefulExitDaemonDoesNotWaitNorStop — сквозная проверка выхода в
// daemon-режиме: GracefulExit не должен ни посылать stop, ни ждать.
func TestGracefulExitDaemonDoesNotWaitNorStop(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, _ := newTestDaemonBackend(t, d, false)

	done := make(chan struct{})
	go func() {
		ac.GracefulExit()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("GracefulExit blocked in daemon keep-running mode; the GUI would hang on quit")
	}

	stops, _, _ := d.counts()
	if stops != 0 {
		t.Fatalf("GracefulExit sent %d /admin/stop, want 0", stops)
	}

	// Идемпотентность (exitOnce): второй вызов ничего не делает.
	ac.GracefulExit()
	stops, _, _ = d.counts()
	if stops != 0 {
		t.Fatalf("second GracefulExit sent %d /admin/stop, want 0", stops)
	}
}
