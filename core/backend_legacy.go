package core

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

// legacyOps — три операции ProcessService, которые бэкенд уводит с потока UI.
type legacyOps struct {
	start   func(skipRunningCheck bool)
	stop    func()
	restart func()
}

// opsOrDefault возвращает действующие операции бэкенда.
func (b *LegacyBackend) opsOrDefault() legacyOps {
	if b.ops != nil {
		return *b.ops
	}
	svc := b.ac.ProcessService
	return legacyOps{
		start:   func(skip bool) { svc.Start(skip) },
		stop:    func() { svc.Stop() },
		restart: func() { svc.KillForRestart() },
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

// Close implements CoreBackend. Nothing to release: ProcessService живёт в
// контроллере и не имеет фоновых ресурсов, привязанных к режиму.
func (b *LegacyBackend) Close() {}
