package core

import (
	"context"

	"singbox-launcher/internal/debuglog"
)

// LegacyBackend — классический движок: делегирует в ProcessService ровно те
// же вызовы, которые раньше делали package-level обёртки. Поведение старого
// режима не меняется ни на байт: вся логика (диалоги, привилегированный
// запуск, Monitor, авто-перезапуск) остаётся внутри ProcessService.
//
// Отличие от прежней версии — ПОТОК (SPEC 148): пользовательские Start/Stop/
// Restart уходят с потока Fyne. Контракт CoreBackend прямо требует
// «fire-and-forget from the UI thread», и daemon-бэкенд давно так и делает
// (`go b.applyCurrentConfig(...)`), а classic вызывал ProcessService
// синхронно. На TUN-старте это означало, что кнопка Start вешает главный
// поток на всё время пути: rebuild конфига, поиск процессов, диалог
// авторизации macOS и запуск root-процесса. macOS видит замерший main thread
// и показывает «приложение не отвечает».
//
// Синхронным остаётся только OnAppExit: выходу из приложения нужен
// завершённый Stop (GracefulExit ждёт `waitForStop`), и он вызывается не из
// UI-колбэка, а из пути завершения.
type LegacyBackend struct {
	ac *AppController
	// ops — шов для тестов: методы ProcessService, вызываемые асинхронно.
	// nil означает «идти в ac.ProcessService». Продакшн его не задаёт.
	ops *legacyOps
}

// legacyOps — операции ProcessService, которые бэкенд уводит с потока UI.
//
// startContext/restartContext are the CONTEXTUAL forms, used by the IPC path
// that must be able to await a real outcome. They are the same code as start/
// restart — ProcessService.StartContext is what Start itself delegates to — so
// there is one implementation of starting a core, not two that can drift.
type legacyOps struct {
	start          func(skipRunningCheck bool)
	startContext   func(ctx context.Context, skipRunningCheck bool) error
	stop           func()
	restart        func()
	restartContext func(ctx context.Context) error
}

// opsOrDefault возвращает действующие операции бэкенда.
func (b *LegacyBackend) opsOrDefault() legacyOps {
	if b.ops != nil {
		return *b.ops
	}
	svc := b.ac.ProcessService
	return legacyOps{
		start:          func(skip bool) { svc.Start(skip) },
		startContext:   func(ctx context.Context, skip bool) error { return svc.StartContext(ctx, skip) },
		stop:           func() { svc.Stop() },
		restart:        func() { svc.KillForRestart() },
		restartContext: func(ctx context.Context) error { return svc.RestartContext(ctx) },
	}
}

// NewLegacyBackend constructs the classic spawn backend.
func NewLegacyBackend(ac *AppController) *LegacyBackend {
	return &LegacyBackend{ac: ac}
}

// Mode implements CoreBackend.
func (b *LegacyBackend) Mode() BackendMode { return BackendClassic }

// StartVPN implements CoreBackend via ProcessService.Start.
//
// Асинхронно и fire-and-forget: возврат управления не означает, что ядро уже
// поднялось — состояние придёт через RunningState/UI, как и у daemon-бэкенда.
// Защита от двойного старта живёт в ProcessService.Start (CmdMutex +
// проверка RunningState под ним), поэтому два быстрых нажатия по-прежнему
// поднимают ровно один экземпляр.
func (b *LegacyBackend) StartVPN(skipRunningCheck ...bool) {
	if b.ac == nil || (b.ops == nil && b.ac.ProcessService == nil) {
		return
	}
	// Копируем вариадический аргумент: он читается в другой горутине.
	skip := len(skipRunningCheck) > 0 && skipRunningCheck[0]
	op := b.opsOrDefault().start
	go op(skip)
}

// StartVPNContext implements contextualCoreBackend: it starts the classic core
// and does not return until the start has actually committed.
//
// WITHOUT THIS, THE HEADLESS PATH SILENTLY FELL BACK TO FIRE-AND-FORGET.
//
// `AppController.StartVPNContext` calls the contextual method when the engine has
// one and otherwise calls `StartVPN` and returns nil immediately. LegacyBackend
// had no contextual method, so on classic — the DEFAULT engine — the IPC start
// returned nil the moment the goroutine was spawned. The caller then cleared its
// pending operation and published the state, which at that instant was still
// `stopped`, because nothing had started yet. The result was the exact symptom
// the contextual interface was introduced to fix, still present on the engine
// most users run: a successful Start click that appears to revert instantly, and
// a client told the operation finished before any work had happened.
//
// So the classic engine now awaits its own commit point like the daemon does.
func (b *LegacyBackend) StartVPNContext(ctx context.Context) error {
	if b.ac == nil || (b.ops == nil && b.ac.ProcessService == nil) {
		return nil
	}
	return b.opsOrDefault().startContext(ctx, false)
}

// RestartVPNContext implements contextualCoreBackend: restart and return the
// real outcome rather than "a goroutine was started".
func (b *LegacyBackend) RestartVPNContext(ctx context.Context) error {
	if b.ac == nil || (b.ops == nil && b.ac.ProcessService == nil) {
		return nil
	}
	return b.opsOrDefault().restartContext(ctx)
}

// StopVPNContext implements contextualCoreBackend.
//
// Classic's Stop already confirms termination internally (TERM → wait → KILL →
// verify by executable identity), so the honest contextual statement is "the
// stop ran to completion and was confirmed". The fire-and-forget StopVPN stays
// for the GUI; this is the path the IPC handler uses.
func (b *LegacyBackend) StopVPNContext(ctx context.Context) error {
	if b.ac == nil || (b.ops == nil && b.ac.ProcessService == nil) {
		return nil
	}
	// The teardown is started UNCONDITIONALLY, even if the context is already
	// cancelled.
	//
	// A stop is not a request that can be declined: the user asked for the tunnel
	// to come down, and the core's fate must not depend on whether some caller is
	// still listening. Checking the context first would skip the teardown
	// entirely and leave a running core behind while telling the caller "you
	// cancelled" — turning a UI timeout into a live tunnel.
	//
	// So: run it, and report the cancellation separately, as what it is.
	done := make(chan struct{})
	go func() {
		b.opsOrDefault().stop()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// The teardown must still run to completion — abandoning it halfway
		// would leave the core in whatever state the half-finished teardown
		// produced — but the CALLER stops waiting and is told why.
		return ctx.Err()
	}
}

// StopVPN implements CoreBackend via ProcessService.Stop.
//
// Тоже асинхронно: Stop ждёт завершения процессов и снятия TUN, а это не
// должно задерживать отрисовку. Панель показывает «остановка…» через
// beginPendingOp, состояние придёт из RunningState.
func (b *LegacyBackend) StopVPN() {
	if b.ac == nil || (b.ops == nil && b.ac.ProcessService == nil) {
		return
	}
	op := b.opsOrDefault().stop
	go op()
}

// RestartVPN implements CoreBackend via ProcessService.KillForRestart —
// watcher (Monitor / onPrivilegedScriptExited) поднимет процесс обратно.
// Асинхронно по той же причине, что Start/Stop.
func (b *LegacyBackend) RestartVPN() {
	if b.ac == nil || (b.ops == nil && b.ac.ProcessService == nil) {
		return
	}
	op := b.opsOrDefault().restart
	go op()
}

// PersistsAfterAppExit implements persistentCoreBackend (SPEC 150): classic
// НИКОГДА не переживает выход лаунчера.
//
// Ядро classic — дочерний процесс лаунчера, и на этом держится вся его
// модель: ProcessService владеет процессом, Monitor следит за падением и
// перезапускает, privileged-путь ждёт обёртку, завершение снимает TUN.
// Отпустить процесс «пусть живёт» значило бы оставить orphan sing-box с
// поднятыми маршрутами и без владельца — поэтому здесь всегда false, а
// удержание VPN после выхода доступно только в daemon-режиме.
func (b *LegacyBackend) PersistsAfterAppExit() bool { return false }

// OnAppExit implements CoreBackend: classic всегда останавливает ядро при
// выходе из лаунчера (как делал GracefulExit → StopSingBoxProcess).
//
// СИНХРОННО намеренно: GracefulExit ждёт завершения Stop (`waitForStop`),
// прежде чем выйти, и вызывается не из UI-колбэка. Сделать его
// fire-and-forget значило бы выйти из приложения, оставив root-ядро и TUN
// висеть.
func (b *LegacyBackend) OnAppExit() bool {
	if b.ac == nil || b.ac.ProcessService == nil {
		return false
	}
	b.ac.ProcessService.Stop()
	return true
}

// Close implements CoreBackend: it abandons this engine's runtime so that any
// work still in flight becomes a no-op.
//
// This used to be empty, on the reasoning that ProcessService lives on the
// controller and owns no mode-scoped resources. That was true of MEMORY and
// false of WORK: Start/Stop/Restart are fire-and-forget goroutines, and a start
// can be sitting in a rebuild or an authorization dialog for minutes. With no
// Close, nothing told that work it had been abandoned, so it finished and
// spawned a core for an engine the user had already left.
//
// What Close does NOT do is kill the running core: switching engines is refused
// while anything is in flight (see SwitchBackendMode) and while the VPN is up, so
// reaching Close means there is nothing to stop. Invalidating the generation is
// the complete action — it makes every outstanding callback stale, which is
// precisely the guarantee that was missing.
func (b *LegacyBackend) Close() {
	if b == nil || b.ac == nil {
		return
	}
	gen := b.ac.classic.currentGeneration()

	// AN ENGINE MUST NOT RELEASE A PROCESS IT STILL OWNS.
	//
	// Renewing the generation only invalidates the bookkeeping; it does nothing
	// to a process that is still alive. That was safe while the engine switch
	// required `!RunningState.IsRunning()` — but a CRASH already clears that flag
	// while the core may still be running (the crash path clears it as soon as
	// the exit is observed, and the process can outlive that observation). So the
	// user could switch engines and leave a live classic core holding the TUN
	// while the daemon's core started alongside it: two engines, one VPN, which is
	// the exact situation the runtime's ownership model exists to prevent.
	//
	// Stopping here is unconditional and synchronous: `Close` is called while the
	// engine is being replaced, so there is no later moment at which anyone would
	// notice the orphan.
	if owned, hasOwned, _ := b.ac.classic.ownedProcess(); hasOwned && owned.PID > 0 {
		debuglog.WarnLog("classic backend closing with a live owned core (pid=%d); stopping it "+
			"before the engine is replaced", owned.PID)
		if b.ac.ProcessService != nil {
			if !b.ac.ProcessService.ForceStopOwnedCore() {
				// Reported rather than swallowed: a core that survived the stop
				// will keep the TUN, and the daemon about to start will fight it.
				b.ac.RecordLifecycleError(LifecycleErrStopFailed, "engine_switch",
					"the core could not be stopped while switching engines",
					"a core left running will keep the tunnel while the new engine starts", false)
			}
		}
	}

	// Renew AFTER the stop: a start that was mid-flight sampled the generation at
	// entry, so bumping it now makes that start's commit point fail and its
	// just-spawned process get stopped instead of adopted.
	newGen := b.ac.classic.renewGeneration()
	debuglog.InfoLog("classic backend closed: generation %d abandoned, %d is now current", gen, newGen)
}
