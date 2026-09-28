package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"singbox-launcher/core/config"
	"singbox-launcher/internal/ctxutil"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/platform"
	"singbox-launcher/internal/process"

	"singbox-launcher/core/events"
)

// Длинные тексты локализации: ключ = английский текст (SPEC 111).
const (
	stopPrivilegedFailedText = "Could not stop sing-box with administrator rights (dialog cancelled or error). The core may still be running — try Stop again or quit sing-box in Activity Monitor."
)

const (
	// restartAttempts is the maximum number of consecutive crash restart attempts
	restartAttempts = 3

	// stabilityThreshold is the duration a process must run without crashing
	// before the crash counter is reset
	stabilityThreshold = 180 * time.Second

	// gracefulShutdownTimeout is the maximum time to wait for graceful shutdown
	// before forcing kill
	gracefulShutdownTimeout = 2 * time.Second

	// ghostTunCleanupDelay — пауза после exit sing-box перед SetupAPI cleanup.
	// На Win7 после taskkill драйверу / PnP нужен короткий момент, чтобы
	// отпустить device node. На non-Win7 — no-op внутри cleanup.
	ghostTunCleanupDelay = 500 * time.Millisecond
)

// runGhostTunCleanup выполняет SPEC 065 cleanup (Win7, aggressive mode).
// Вызывать только когда sing-box подтверждённо не работает.
//
// sync=false — фоновая goroutine (Stop, не блокирует UI).
// sync=true  — inline (Restart: cleanup до Start, чтобы не снести новый адаптер).
func runGhostTunCleanup(sync bool) {
	// SPEC 139 §6 п. 4: без прав ядро TUN не поднимало (гейт в Start), а
	// очистка пишет в HKLM и SetupAPI — пропуск молча.
	if windowsNotElevated() {
		debuglog.DebugLog("runGhostTunCleanup: skipped, not elevated")
		return
	}
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				debuglog.WarnLog("runGhostTunCleanup: recovered from panic: %v", r)
			}
		}()
		time.Sleep(ghostTunCleanupDelay)
		removed, err := platform.CleanupGhostSingboxTunAdapters(platform.GhostTunCleanupAggressive)
		if err != nil {
			debuglog.WarnLog("runGhostTunCleanup: cleanup returned error: %v", err)
			return
		}
		if removed > 0 {
			debuglog.WarnLog("runGhostTunCleanup: removed %d stale adapter(s)", removed)
		}
	}
	if sync {
		run()
		return
	}
	go run()
}

// triggerGhostTunCleanup — async runGhostTunCleanup (не блокирует caller).
func triggerGhostTunCleanup() {
	runGhostTunCleanup(false)
}

// CleanupStaleTunAtStart удаляет накопившиеся singbox-tun WinTun-адаптеры при
// старте лаунчера (Win7, aggressive). Нужно после обновления с версии без
// auto-cleanup или если остались ghost'ы с прошлых сессий / reboot.
//
// Пропускаем, если sing-box уже запущен — иначе снесём активный адаптер.
// Не блокирует UI (фоновая goroutine, без задержки — stale уже давно мёртвые).
func (svc *ProcessService) CleanupStaleTunAtStart() {
	if found, pid := svc.isSingBoxProcessRunning(); found {
		debuglog.WarnLog("CleanupStaleTunAtStart: skipped (sing-box already running PID=%d)", pid)
		return
	}
	if svc.ac.RunningState.IsRunning() {
		debuglog.WarnLog("CleanupStaleTunAtStart: skipped (launcher thinks VPN is running)")
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				debuglog.WarnLog("CleanupStaleTunAtStart: recovered from panic: %v", r)
			}
		}()
		debuglog.WarnLog("CleanupStaleTunAtStart: scanning for stale singbox-tun adapters")
		removed, err := platform.CleanupGhostSingboxTunAdapters(platform.GhostTunCleanupAggressive)
		if err != nil {
			debuglog.WarnLog("CleanupStaleTunAtStart: cleanup returned error: %v", err)
			return
		}
		if removed > 0 {
			debuglog.WarnLog("CleanupStaleTunAtStart: removed %d stale adapter(s) from previous sessions", removed)
		}
	}()
}

// ProcessService encapsulates sing-box process lifecycle management.
// It handles starting, stopping, monitoring, and auto-restarting the sing-box process.
// The service ensures proper cleanup of TUN interfaces, log rotation, and process state management.
type ProcessService struct {
	ac *AppController
	// coreLog — куда пишет вывод последнее запущенное ядро (SPEC 137.1):
	// coreLogUnknown до первого старта в сессии, coreLogUser — лог в
	// каталоге пользователя, coreLogPrivileged — лог старта с TUN в root-owned каталоге.
	coreLog atomic.Int32
	// pidFileWriteFailed — pid-файл последнего привилегированного старта
	// записать не удалось (SPEC 145). Ядро при этом работает: файл нужен лишь
	// для опознания после перезапуска лаунчера, поэтому старт не отменяется,
	// но состояние видно UI и логу.
	pidFileWriteFailed atomic.Bool
	// stopOverrideForTest — шов для проверки, что OnAppExit остаётся
	// синхронным. Продакшн его не задаёт (nil).
	stopOverrideForTest func()
	// privDeps — шов зависимостей привилегированного старта; nil = продакшн.
	privDeps *privilegedStartDeps
}

// Куда пишет вывод classic-ядро (ProcessService.coreLog).
const (
	coreLogUnknown int32 = iota
	coreLogUser
	coreLogPrivileged
)

// CoreLogPath — файл вывода текущего (или последнего) classic-ядра: лог в
// каталоге пользователя (<Logs>/sing-box.log), а после старта с TUN на
// macOS — platform.PrivilegedCoreLogPath в root-owned каталоге (SPEC 137.1): root в
// каталог пользователя не пишет. До первого старта в сессии — по конфигу
// (TUN на macOS → лог под root). Его читают Core-вкладка логов и тейлер
// профайлера трафика; дёшево после первого вызова.
func (ac *AppController) CoreLogPath() string {
	if ac.FileService == nil {
		// Only reachable from a partially constructed controller (tests, and the
		// window during startup before services are wired). Returning "" makes
		// callers treat the log as unavailable, which is the honest answer —
		// dereferencing here would panic inside a crash-classification path that
		// runs on a background goroutine, taking the process down.
		return ""
	}
	privileged := platform.PrivilegedCoreLogPath()
	if privileged == "" || ac.ProcessService == nil {
		return ac.FileService.ChildLogPath
	}
	state := ac.ProcessService.coreLog.Load()
	if state == coreLogUnknown {
		state = coreLogUser
		if classicElevatedUsesCopy() {
			// Windows: повышенный classic пишет в classic.log при любом конфиге.
			state = coreLogPrivileged
		} else if hasTun, err := config.ConfigHasTun(ac.FileService.ConfigPath); err == nil && hasTun {
			state = coreLogPrivileged
		}
		ac.ProcessService.coreLog.CompareAndSwap(coreLogUnknown, state)
		state = ac.ProcessService.coreLog.Load()
	}
	if state == coreLogPrivileged {
		return privileged
	}
	return ac.FileService.ChildLogPath
}

// classifyCoreExitReason — причина завершения ядра по хвосту его лога
// (SPEC 143). Ошибки ядра печатаются перед выходом, поэтому читаем
// последние строки. Лог недоступен — exitReasonUnknown: отсутствие лога
// не доказательство отсутствия ошибки, и неизвестная причина остаётся
// транзиентной, то есть авто-восстановление не отключается зря.
func (ac *AppController) classifyCoreExitReason(gen uint64) exitReason {
	path := ac.CoreLogPath()
	if path == "" {
		return exitReasonUnknown
	}
	// Читаем только хвост: лог ротируется на 2 МиБ, а причина падения всегда
	// в последних строках. Читать файл целиком ради 16 КиБ — лишняя память на
	// каждом падении ядра.
	text, err := readFileTail(path, coreLogTailBytes)
	if err != nil {
		debuglog.DebugLog("classifyCoreExitReason: cannot read %s: %v", path, err)
		return exitReasonUnknown
	}

	// ONLY THIS GENERATION'S OUTPUT COUNTS.
	//
	// The log is append-only across generations, so the tail can still contain a
	// fatal signature written by a PREVIOUS core. The classifier would then read
	// an old failure as this exit's cause and shut down auto-restart — an
	// unrelated transient crash would be reported, and treated, as a
	// deterministic config error that will never succeed.
	//
	// The offset recorded when this generation's core started is the boundary:
	// text before it belongs to a world that is already over.
	//
	// The generation is a PARAMETER for the same reason every other callback in
	// this file takes one: a lookup of the current generation would classify an
	// exiting process against a successor's log window, which can turn a transient
	// crash into a reported deterministic config failure (or hide one), and a
	// deterministic verdict stops auto-restart.
	if start := ac.classic.logOffsetFor(gen); start > 0 {
		if skipped := skipToOffset(text, path, start); skipped >= 0 {
			text = text[skipped:]
		}
	}

	reason := classifyExitText(lastLines(text, 40))
	debuglog.InfoLog("classifyCoreExitReason: %s (log %s)", reason, path)
	return reason
}

// skipToOffset returns the index within `tail` at which the log content from
// `offset` onward begins.
//
// `tail` is the last N bytes of the file, so the relevant part starts
// (offset - (size - len(tail))) bytes into it. A negative result means the whole
// tail is already newer than the offset — nothing to skip — and a start beyond
// the tail means the offset is stale (the file was rotated or truncated), in
// which case the caller keeps the whole tail rather than discarding evidence.
func skipToOffset(tail, path string, offset int64) int {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	size := info.Size()
	tailStart := size - int64(len(tail))
	if tailStart < 0 {
		tailStart = 0
	}
	skip := offset - tailStart
	if skip <= 0 {
		return -1
	}
	if skip >= int64(len(tail)) {
		// The offset points past what we read: the tail is entirely from the
		// previous generation, so there is no current-generation output to
		// classify — an empty string is the honest answer, not the old text.
		return len(tail)
	}
	return int(skip)
}

// coreLogTailBytes — сколько хвоста лога достаточно для классификации.
const coreLogTailBytes = 16 * 1024

// readFileTail возвращает последние maxBytes файла, не читая его целиком.
//
// Файл может быть меньше — тогда возвращается он весь. Хвост может начаться
// с середины строки: вызывающий разбирает текст построчно (lastLines), и
// обрезанная первая строка просто не попадает в значимые.
func readFileTail(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer debuglog.RunAndLog("readFileTail: close", f.Close)

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, size-offset)
	if _, err := io.ReadFull(f, buf); err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	return string(buf), nil
}

// showDeterministicExitDialog — диалог детерминированного отказа (SPEC 143).
// Показывает конкретную причину и действие вместо «не удалось
// перезапустить»: авто-перезапуск прекращён осознанно, потому что повтор
// ничего не изменит.
// showDeterministicExitDialog reports a core exit that restarting cannot fix
// (bad config, permissions, occupied port).
//
// The Fyne dialog is only one consumer. Returning early when no UI port exists
// meant the headless backend — which is what the macOS app talks to — reported
// NOTHING at all for the case where the core died for a reason that will recur.
// The user saw a VPN that would not stay up and no explanation anywhere except
// the log.
func (ac *AppController) showDeterministicExitDialog(reason exitReason) {
	body := locale.T(deterministicExitText(reason))
	if body == "" {
		body = "the core exited for a reason that restarting will not fix"
	}
	// Recorded regardless of UI: this is a lifecycle failure, and it is the
	// actionable kind — the message names what to fix and the code lets the
	// frontend localize it.
	code := LifecycleErrCoreStart
	switch reason {
	case exitReasonPermission:
		code = LifecycleErrPermission
	case exitReasonPortInUse:
		code = LifecycleErrPortInUse
	case exitReasonConfigInvalid, exitReasonMissingResource:
		code = LifecycleErrConfigCheck
	}
	ac.RecordLifecycleError(code, "start", body, reason.String(), false)
	if ac.uiPort != nil {
		ac.ui().ShowError(locale.T("Error"), body)
	}
}

// NewProcessService constructs a ProcessService bound to the controller.
func NewProcessService(ac *AppController) *ProcessService {
	return &ProcessService{ac: ac}
}

// Start launches the sing-box process. Behavior is identical to the previous StartSingBoxProcess.
// skipRunningCheck: если true, пропускает проверку на уже запущенный процесс (для автоперезапуска).
func (svc *ProcessService) Start(skipRunningCheck ...bool) {
	err := svc.StartContext(context.Background(), skipRunningCheck...)
	if err != nil {
		// The GUI wrapper owns presentation. The headless backend calls
		// StartContext directly and turns the error into a protocol error the
		// frontend can show — the same business function serving both, instead
		// of core showing dialogs nobody is there to see.
		svc.ac.ShowStartupError(err)
	}
}

// StartContext starts the core and RETURNS the reason on failure.
//
// This is the headless-capable entry point. `Start` remains as the GUI wrapper
// for the Fyne build and every existing caller.
//
// WHY THIS EXISTS: the only error channel used to be ShowStartupError, which
// ends in showErrorUI, which logs and then does nothing at all when uiPort is
// nil. In the headless backend uiPort IS nil, so a failed start was written to a
// log file and the user saw the button flip back to "Start" with no explanation.
// Making the failure a return value is what lets the backend hand it to the
// frontend as a structured error.
//
// ctx is honoured before the commit point (template refresh wait, lock
// acquisition), so a cancelled request does not leave a start racing behind it.
func (svc *ProcessService) StartContext(ctx context.Context, skipRunningCheck ...bool) error {
	ac := svc.ac
	if ac.RunningState.IsRunning() {
		if ac.uiPort != nil {
			ac.uiPort.ShowInfo(locale.TN(1, "Info"), locale.T("Sing-Box already running (according to internal state)."))
		}
		// Already running is SUCCESS, not failure: the user's intent (a running
		// core) is satisfied. Reporting an error here would make a harmless
		// double click look like a fault.
		return nil
	}

	// Проверяем, не запущен ли уже процесс на уровне ОС (пропускаем при автоперезапуске)
	skipCheck := len(skipRunningCheck) > 0 && skipRunningCheck[0]
	if !skipCheck {
		if svc.checkAndShowSingBoxRunningWarning("startSingBox") {
			// The dialog was shown by the check itself on the GUI path. Headless
			// there is no dialog, so the refusal carries the reason: a second core
			// would fight the running one for the TUN device.
			return NewPreconditionRefusal(
				StartErrForeignCoreRunning,
				"another sing-box process is already running; stop it before starting the VPN",
				true, ac.uiPort == nil, ErrStartAborted)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Шаблон после апгрейда докачивается в фоне (StartTemplateRefresh) —
	// ждём его ДО захвата CmdMutex: Stop, нажатый во время ожидания, иначе
	// встал бы на мьютексе. Сборка ниже ждёт того же шлюза, но он уже открыт.
	// Context-aware from here on. Two uninterruptible waits used to sit on this
	// path — the template refresh and the CmdMutex acquisition — so the
	// operation deadline on the IPC path bounded nothing: a caller whose context
	// had long expired still sat in one of them and then proceeded in a world
	// that had moved on.
	if err := ac.awaitTemplateRefreshContext(ctx); err != nil {
		return err
	}

	if !acquireWithContext(ctx, &ac.CmdMutex) {
		// The wait for the engine was abandoned. Nothing has been changed yet,
		// so this is a clean exit; the caller is told why.
		debuglog.InfoLog("startSingBox: gave up waiting for the engine lock: %v", ctx.Err())
		return ctx.Err()
	}
	defer ac.CmdMutex.Unlock()

	// Re-check under the lock: two concurrent Start() calls both pass the
	// unlocked IsRunning() check above; without this the second one would
	// launch a duplicate sing-box process and orphan the first Cmd handle.
	if ac.RunningState.IsRunning() {
		debuglog.WarnLog("startSingBox: already running (lost start race), skipping duplicate start")
		return nil
	}

	// CLAIM THE RUNTIME, under the same lock as the re-check above.
	//
	// From here until the commit point the phase is "starting", which is the
	// statement that was missing before: a start in progress (rebuild,
	// authorization, spawn) looked exactly like "stopped" to every other caller,
	// so the engine could be switched out from under it and the core this
	// function eventually spawned belonged to nobody.
	//
	// beginOperation also REFUSES when an operation is already in flight, which
	// is what makes a double click start exactly one core: the second call sees
	// a busy runtime rather than an idle one.
	startGen, startOpID, claimed := ac.classic.beginOperation(ClassicStarting, false)
	if !claimed {
		debuglog.InfoLog("startSingBox: a start is already in progress, ignoring duplicate request")
		return nil
	}
	debuglog.InfoLog("startSingBox: starting (generation=%d op=%d)", startGen, startOpID)
	// Record where the core log stands NOW, before this generation can write
	// anything to it. Crash classification reads only what follows, so a fatal
	// signature left by an earlier core cannot be mistaken for this exit's cause
	// and shut down auto-restart for an unrelated transient failure.
	if path := ac.CoreLogPath(); path != "" {
		if info, err := os.Stat(path); err == nil {
			ac.classic.noteLogOffset(startGen, info.Size())
		}
	}
	// A new attempt supersedes the previous failure: keeping a stale error
	// visible while a retry is already running would show the user a problem
	// that may no longer exist.
	ac.ClearLifecycleError()

	// SPEC 045 phase 5.C — pre-start config rebuild:
	// если Wizard Save поднял dirty-маркеры (CacheStale / ConfigStale),
	// перед запуском sing-box пересобираем config.json из state + cache.
	// Это компенсирует UX-регрессию фазы 5.B: Wizard Save больше не пишет
	// config.json, и без этого хука sing-box стартовал бы со старым.
	//
	// Ошибка пересборки ОСТАНАВЛИВАЕТ запуск и показывается пользователю.
	// Прежнее «логируем и стартуем со старым config.json» прятало провал:
	// ядро работало, а настройки не применялись, и узнать об этом было
	// неоткуда.
	if err := ac.rebuildConfigBeforeStart(false); err != nil {
		debuglog.ErrorLog("startSingBox: config rebuild failed, sing-box not started: %v", err)
		// Release the claim: nothing was started, so the runtime must not stay
		// "starting" forever. A phase that never settles would block the mode
		// switch permanently, which is a worse failure than the one it guards.
		ac.classic.setPhase(startGen, ClassicFailed)
		ac.RecordConfigError(LifecycleErrConfigRebuild, "start",
			"the config could not be rebuilt, so the core was not started", err.Error())
		return NewStartFailure(StartErrConfigRebuildFailed, err)
	}

	// SPEC 139 §4: на Windows TUN без прав администратора не стартует —
	// вместо ядра диалог (перезапуск с правами или режим прокси). Сюда
	// приходят все входы: кнопка, трей, -start, Debug API, авто-рестарт.
	if ac.tunNeedsElevation() {
		ac.showTunElevationDialog()
		if ac.uiPort != nil {
			ac.uiPort.ReportCoreStartAborted("")
		}
		ac.classic.setPhase(startGen, ClassicStopped)
		// A STRUCTURED refusal, not a bare Aborted.
		//
		// The GUI knows why because it just showed the elevation dialog. The
		// headless frontend has no uiPort, so `ErrStartAborted` meant "declined
		// for reasons nobody will ever tell you" — the user pressed Start and the
		// button returned to Start with no explanation. Silent says which of the
		// two situations this is.
		return NewPreconditionRefusal(
			StartErrTunElevationRequired,
			"starting the VPN needs administrator authorization on this system",
			true, ac.uiPort == nil, nil)
	}

	// Check capabilities on Linux before starting
	if suggestion := platform.CheckAndSuggestCapabilities(ac.coreBinaryPath()); suggestion != "" {
		debuglog.WarnLog("startSingBox: Capabilities check failed: %s", suggestion)
		if ac.uiPort != nil {
			cmd := platform.GetSetCapCommand(ac.coreBinaryPath())
			ac.uiPort.ShowCommandNeedsTerminal(locale.T("Linux capabilities required"), locale.T("Linux capabilities required")+"\n\n"+suggestion, cmd)
		}
		ac.RecordLifecycleError(LifecycleErrPermission, "start",
			"the core needs additional Linux capabilities", suggestion, false)
		ac.classic.setPhase(startGen, ClassicFailed)
		return NewPreconditionRefusal(
			StartErrPrivilegesRequired,
			"the core needs additional Linux capabilities",
			true, ac.uiPort == nil, fmt.Errorf("%s", suggestion))
	}

	// Reload Clash API configuration from config.json before starting
	// This ensures we pick up any changes made via wizard or manual config edits
	if ac.APIService != nil {
		debuglog.InfoLog("startSingBox: Reloading Clash API configuration...")
		if err := ac.APIService.ReloadClashAPIConfig(); err != nil {
			debuglog.WarnLog("startSingBox: Warning: Failed to reload Clash API config: %v", err)
			// Continue anyway - API might not be configured or config might be invalid
		}
	}

	// Reset API cache before starting
	{
		debuglog.InfoLog("startSingBox: Resetting API state cache...")
		ac.ui().ResetAPIState()
	}

	// On macOS, use privileged start only when config has TUN (so password is asked only when needed)
	if runtime.GOOS == "darwin" {
		hasTun, err := config.ConfigHasTun(ac.FileService.ConfigPath)
		if err != nil {
			// FAIL CLOSED: "I could not tell" is not "there is no TUN".
			//
			// This used to log and set hasTun = false, which silently chose the
			// UNPRIVILEGED launch path. If the config does contain a TUN inbound —
			// exactly the case where the check could not be performed — the core
			// starts without the privileges it needs and fails in a confusing way
			// far from the cause, or worse, appears to start and does not route.
			//
			// The configuration could not be read, so the launcher does not know
			// what it is about to run. Refusing is the honest answer, and the
			// reason travels to the frontend with a stable code instead of a
			// dialog the headless build cannot show.
			debuglog.ErrorLog("startSingBox: cannot determine whether the config uses TUN: %v", err)
			// Recorded in the lifecycle store AND returned as a structured
			// refusal, so the headless frontend learns the reason even though
			// there is no dialog to show it in.
			ac.RecordConfigError(LifecycleErrConfigRebuild, "start",
				"the configuration could not be read", err.Error())
			return NewPreconditionRefusal(
				StartErrConfigRebuildFailed,
				"the configuration could not be read, so the launcher cannot tell whether it needs TUN privileges",
				true, false, err)
		}
		if hasTun {
			if err := svc.startSingBoxPrivileged(startGen); err != nil {
				// Отказ гейта копии уже показан своим диалогом с командой; он
				// сообщает о себе сам, поэтому наружу уходит Aborted, а не
				// вторая ошибка про то же самое.
				if errors.Is(err, errPrivilegedCopyNotReady) {
					// The gate showed its own dialog with the copy command on the
					// GUI path; on the headless path nobody was told, so the
					// refusal carries the reason and the remedy.
					return NewPreconditionRefusal(
						StartErrPrivilegedCopyUnavailable,
						"the protected copy of the core is missing or out of date; run the copy command shown in the launcher",
						true, ac.uiPort == nil, err)
				}
				return NewStartFailure(StartErrSpawnFailed, err)
			}
			// startSingBoxPrivileged commits RunningState itself on success.
			return nil
		}
	}

	// SPEC 141 §8: повышенный лаунчер на Windows исполняет только
	// защищённую копию ядра (гейт по токену при любом конфиге), вывод — в
	// classic.log с явным DACL.
	corePath := ac.FileService.SingboxPath
	var privilegedLog *os.File
	if classicElevatedUsesCopy() {
		path, logFile, err := ac.elevatedClassicStart()
		if err != nil {
			if errors.Is(err, errPrivilegedCopyNotReady) {
				return NewPreconditionRefusal(
					StartErrPrivilegedCopyUnavailable,
					"the protected copy of the core is missing or out of date; run the copy command shown in the launcher",
					true, ac.uiPort == nil, err)
			}
			return NewStartFailure(StartErrSpawnFailed, err)
		}
		corePath, privilegedLog = path, logFile
	}

	// LATE-SUPERSEDE CHECK, immediately before spawning.
	//
	// Everything above (rebuild, authorization, capability probes) can take
	// seconds to minutes. If the runtime was abandoned in the meantime — the
	// user switched to the daemon engine, or the app is shutting down — this
	// start must not produce a process. Checking here rather than only at the
	// entry is what closes the window: at entry the generation WAS current, so
	// an entry check alone would pass and the core would still appear later.
	if !ac.classic.isCurrent(startGen) {
		debuglog.WarnLog("startSingBox: generation %d was superseded before spawn; not starting a core", startGen)
		return ErrStartAborted
	}

	debuglog.WarnLog("startSingBox: Starting Sing-Box...")
	cmd := exec.Command(corePath, "run", "-c", filepath.Base(ac.FileService.ConfigPath))
	platform.PrepareCommand(cmd)
	cmd.Dir = ac.FileService.Layout.Data.Bin()
	ac.SingboxCmd = cmd
	if privilegedLog != nil {
		cmd.Stdout = privilegedLog
		cmd.Stderr = privilegedLog
	} else if ac.FileService.ChildLogFile != nil {
		// Check and rotate log file before starting new process to prevent unbounded growth
		ac.FileService.CheckAndRotateLogFile(ac.FileService.ChildLogPath)

		// Write directly to file - no buffering in memory
		// This prevents memory leaks from accumulating log output
		// Logs are written immediately to disk, not stored in memory
		cmd.Stdout = ac.FileService.ChildLogFile
		cmd.Stderr = ac.FileService.ChildLogFile
	} else {
		debuglog.WarnLog("startSingBox: Warning: sing-box log file not available, output will not be logged.")
	}
	startErr := cmd.Start()
	if privilegedLog != nil {
		// У ядра свой дескриптор classic.log; наш больше не нужен.
		_ = privilegedLog.Close()
	}
	if err := startErr; err != nil {
		debuglog.ErrorLog("startSingBox: Failed to start Sing-Box: %v", err)
		ac.classic.setPhase(startGen, ClassicFailed)
		failure := NewClassifiedStartFailure(StartErrSpawnFailed,
			fmt.Errorf("failed to start Sing-Box process: %w", err))
		svc.recordStartFailure("start", failure)
		return failure
	}
	if privilegedLog != nil {
		svc.coreLog.Store(coreLogPrivileged)
	} else {
		svc.coreLog.Store(coreLogUser)
	}

	// THE COMMIT POINT.
	//
	// The process now exists, so ownership must be recorded — but only for the
	// generation that asked for it. A superseded generation that reaches this
	// line has a live process on its hands and nobody to own it: the correct
	// action is to kill what it just created and report cancellation, NOT to
	// adopt it into a runtime that has moved on. This is the orphan case: a
	// process that exists, holds the TUN, and appears in no state the user can
	// act on.
	if !ac.classic.commitChild(startGen, cmd, corePath) {
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		debuglog.WarnLog("startSingBox: generation %d was superseded at the commit point; stopping the just-started core (PID=%d)", startGen, pid)
		svc.killSupersededStart(cmd, corePath, pid)
		return ErrStartAborted
	}
	ac.SingboxCmd = cmd
	ac.RunningState.Set(true)
	ac.StoppedByUser = false
	ac.StateService.ResetAutoUpdateFailedAttempts() // Reset so auto-update can retry after successful Start
	// Add log with PID
	debuglog.DebugLog("startSingBox: Sing-Box started. PID=%d", cmd.Process.Pid)

	// READINESS: stay "starting" until the process has survived its own startup.
	//
	// `exec.Start` succeeding means a process was created, not that a VPN is up.
	// A core that rejects its config, fails to bind its API port or loses its
	// privileges dies within milliseconds, and the old code reported `running`
	// immediately — so the UI flashed "Connected" and then fell to "Stopped",
	// which reads as a glitch rather than as a start failure with a cause.
	//
	// The phase is promoted by a watcher that waits a short, bounded window
	// (readinessWindow) for the process to still be alive. This is not a magic
	// sleep used as a substitute for a real readiness probe: it is the shortest
	// interval that distinguishes "still starting up" from "already dead", and
	// the authoritative signal remains the process's own exit. Where an API probe
	// exists it runs alongside and its result is what the user ultimately needs.
	//
	// Crucially, a death inside the window is reported as a start FAILURE with
	// the classified reason — the exit is not left to the crash monitor, which
	// would treat a first-run config error as a crash to be restarted.
	go svc.promoteToRunningWhenReady(startGen, cmd, corePath)

	// Start auto-loading proxies after sing-box is running.
	//
	// BOUND TO THE GENERATION. This goroutine sleeps past the point where the
	// world can change: the core can be stopped, the engine switched, or a new
	// generation started. Without the check it would then reload proxies against
	// whatever backend is current — or against a core that is already gone —
	// issuing a request the user never asked for, for a VPN that is not this one.
	go ac.delayedAutoLoadProxies(startGen)

	go svc.Monitor(ac.SingboxCmd)
	return nil
}

// readinessWindow is how long a freshly spawned core must stay alive before the
// state is promoted from "starting" to "running".
//
// Chosen against the two failure shapes it must separate: a core that rejects
// its config exits in tens of milliseconds, and a healthy core is past its
// startup in well under a second. One second is comfortably outside both, and
// short enough that a successful start does not feel slow.
const readinessWindow = time.Second

// promoteToRunningWhenReady promotes the phase once the core has survived startup.
//
// Ownership and generation are both re-checked after the wait: the runtime may
// have been abandoned (mode switch, shutdown) or the process may have been
// replaced, and promoting a stale generation to running is the same class of bug
// as a stale monitor restarting a core.
func (svc *ProcessService) promoteToRunningWhenReady(gen uint64, cmd *exec.Cmd, corePath string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	deadline := time.Now().Add(readinessWindow)
	for time.Now().Before(deadline) {
		if !processIsAlive(pid, corePath) {
			// Died inside the window: this is a FAILED START, not a crash.
			//
			// The reason is classified from the core's own log, so the user gets
			// "the port is occupied" or "the config was rejected" rather than a
			// restart loop. The monitor will also observe the exit; it finds the
			// generation still current but the phase already failed, and the
			// start-failure record takes precedence in the reported state.
			svc.reportEarlyExit(gen, pid)
			return
		}
		// Polling rather than blocking on the process: the exit may arrive
		// through the monitor at any moment, and a channel-based wait here would
		// have to race it for the same event.
		time.Sleep(50 * time.Millisecond)
	}

	// Survived: promote, unless the runtime moved on.
	if !svc.ac.classic.isCurrent(gen) {
		debuglog.InfoLog("startSingBox: generation %d superseded before readiness; not promoting to running", gen)
		return
	}
	if svc.ac.classic.setPhase(gen, ClassicRunning) {
		debuglog.InfoLog("startSingBox: core is ready (PID=%d, generation=%d)", pid, gen)
		svc.ac.EmitCoreStateChange()
	}
}

// reportEarlyExit records a core that died during startup as a start failure.
//
// ONE EXIT, ONE DECISION. This runs while the crash Monitor is also waiting on
// the same process, and the two must not both act on the same death. Without
// coordination the sequence was:
//
//	readiness sees the exit  → records a failed start, phase = failed
//	monitor  sees the exit   → decides "crash", auto-restarts a core whose
//	                           config had just been rejected
//
// which is precisely the restart loop the readiness gate exists to prevent. The
// claim below makes the decision single-observation: whoever gets there first
// owns the classification, and the other side finds the claim taken and leaves
// the state alone.
func (svc *ProcessService) reportEarlyExit(gen uint64, pid int) {
	debuglog.WarnLog("startSingBox: core (PID=%d) exited during startup", pid)
	if !svc.ac.classic.isCurrent(gen) {
		return
	}
	// Claim the exit. A false result means the Monitor has already classified
	// this death (as a crash) and is acting on it — reporting a start failure on
	// top of that would give the user two causes for one event, and the second
	// one could undo a restart that is already under way.
	if !svc.ac.classic.claimExit(gen) {
		debuglog.InfoLog("startSingBox: the exit of PID %d was already classified; not reporting a start failure", pid)
		return
	}
	reason := svc.ac.classifyCoreExitReason(gen)
	code := LifecycleErrFastExit
	recoverable := true
	switch reason {
	case exitReasonPortInUse:
		code, recoverable = LifecycleErrPortInUse, false
	case exitReasonPermission:
		code, recoverable = LifecycleErrPermission, false
	case exitReasonConfigInvalid, exitReasonMissingResource:
		code, recoverable = LifecycleErrConfigCheck, false
	}
	svc.ac.RecordLifecycleError(code, "start",
		"the core exited immediately after starting", reason.String(), recoverable)
	svc.ac.classic.setPhase(gen, ClassicFailed)
}

// readinessChecker is the liveness probe the readiness gate uses.
//
// A package-level variable so tests can supply a deterministic process table:
// the question "did the core survive its first second" must be answerable
// without spawning real processes, and the seam keeps the test honest — it
// exercises the same code path production takes.
var readinessChecker processChecker = platformChecker{}

// processIsAlive reports whether a process is still the core we started.
func processIsAlive(pid int, expected string) bool {
	if pid <= 0 {
		return false
	}
	alive, err := readinessChecker.alive(pid, expected)
	if err != nil {
		// Cannot verify: assume alive rather than reporting a death that may not
		// have happened. Liveness is confirmed by the monitor on real exit.
		return true
	}
	return alive
}

// errPrivilegedCopyNotReady — гейт привилегированного старта отказал по
// root-owned копии ядра и сам показал диалог с командой (SPEC 137).
var errPrivilegedCopyNotReady = errors.New("the root-owned core copy for the privileged start is not ready")

// privilegedStartTimeout — сколько ждать ответа AEWP, прежде чем вернуть
// управление. Диалог авторизации может стоять долго (пользователь отошёл),
// и без предела GUI остался бы с «идёт запуск» навсегда. Значение заведомо
// больше любого реального ввода пароля и не влияет на уже запущенный процесс.
const privilegedStartTimeout = 3 * time.Minute

// privilegedStartDeps — внешние зависимости привилегированного старта.
//
// Шов живёт ПОЛЕМ ProcessService, а не переменной пакета: пакетные
// переменные общие для всех тестов, и параллельно идущие тесты (и
// goroutine-ожидатель, переживающая тест) гоняются за них — -race это ловит.
// nil-поле означает продакшн-поведение.
type privilegedStartDeps struct {
	// gate — гейт защищённой root-owned копии (читает /Library, нужен root).
	gate func(*AppController) (string, error)
	// start — запуск ядра под root через AEWP.
	start func(corePath, binDir, configName string) (int, int, error)
	// waitExit — ожидание выхода root-процесса.
	waitExit func(int)
}

// deps возвращает действующие зависимости.
func (svc *ProcessService) deps() privilegedStartDeps {
	if svc.privDeps != nil {
		return *svc.privDeps
	}
	return privilegedStartDeps{
		gate:     func(ac *AppController) (string, error) { return ac.privilegedCoreCopyGate() },
		start:    platform.StartPrivilegedCore,
		waitExit: platform.WaitForPrivilegedExit,
	}
}

// privilegedStartResult — то, что worker обязан вернуть вызывающему.
// Только данные: никакого состояния здесь не коммитится.
type privilegedStartResult struct {
	Script  int
	Singbox int
	Err     error
}

// commitPrivilegedStartLocked фиксирует состояние успешного привилегированного
// старта.
//
// PRECONDITION: ac.CmdMutex УЖЕ удерживается вызывающим. Эта функция НЕ берёт
// CmdMutex и не имеет права его брать: caller — это ProcessService.Start(),
// который держит мьютекс на всё время старта, и повторный Lock здесь означал бы
// самоблокировку (именно она вешала GUI на реальном TUN-старте: worker ждал
// мьютекс, который caller не отпустит до получения PID от worker'а).
//
// Вызывающий получает PID из канала и коммитит состояние сам, поэтому
// инвариант «Start() вернул успех ⇒ RunningState уже true» сохраняется без
// вложенного захвата.
// Returns false when the generation was superseded, in which case ownership was
// deliberately NOT recorded: the caller must stop the process it just started.
func (svc *ProcessService) commitPrivilegedStartLocked(gen uint64, scriptPID, singboxPID int, pidFilePath, corePath string) bool {
	ac := svc.ac
	svc.coreLog.Store(coreLogPrivileged)
	if !ac.classic.commitPrivileged(gen, scriptPID, singboxPID, pidFilePath, corePath) {
		return false
	}
	ac.SingboxCmd = nil
	ac.SingboxPrivilegedMode = true
	ac.SingboxPrivilegedPID = scriptPID
	ac.SingboxPrivilegedSingboxPID = singboxPID
	ac.SingboxPrivilegedPIDFile = pidFilePath
	ac.StoppedByUser = false
	ac.ConsecutiveCrashAttempts = 0
	if ac.StateService != nil {
		ac.StateService.ResetAutoUpdateFailedAttempts() // auto-update may retry after a successful start
	}
	ac.RunningState.Set(true)
	return true
}

// startSingBoxPrivileged starts sing-box with elevated privileges on macOS (for TUN).
// Команда root-шелла собирается в platform; оркестрация и состояние — здесь.
//
// SPEC 137: root исполняет только root-owned копию ядра и системные
// утилиты. Гейт проверяет копию до AEWP; не прошла — старта с привилегиями
// нет. Скрипт в каталоге данных больше не пишется. SPEC 137.1: вывод ядра
// root пишет в свой каталог (platform.PrivilegedCoreLogPath) и там же
// ротирует его; каталог пользователя root не трогает.
//
// PRECONDITION: ac.CmdMutex удерживается вызывающим (Start). Worker НЕ берёт
// CmdMutex — см. commitPrivilegedStartLocked.
func (svc *ProcessService) startSingBoxPrivileged(gen uint64) error {
	ac := svc.ac
	deps := svc.deps()
	corePath, err := deps.gate(ac)
	if err != nil {
		return err
	}
	binDir := ac.FileService.Layout.Data.Bin()
	configName := filepath.Base(ac.FileService.ConfigPath)

	pidFilePath := filepath.Join(binDir, platform.PrivilegedPidFileName)
	// Скрипт старта до SPEC 137 больше ничто не исполняет — убираем, чтобы
	// он не выглядел действующим.
	legacyScript := filepath.Join(binDir, platform.PrivilegedLegacyScriptName)
	if err := os.Remove(legacyScript); err == nil {
		debuglog.InfoLog("startSingBox: removed the pre-SPEC 137 start script %s", legacyScript)
	} else if !os.IsNotExist(err) {
		debuglog.WarnLog("startSingBox: cannot remove the old start script %s: %v", legacyScript, err)
	}

	debuglog.WarnLog("startSingBox: Starting Sing-Box with elevated privileges (TUN) from %s...", corePath)

	// Буферизованный канал на 1: worker кладёт ровно один результат и уходит.
	// Буфер здесь не про асинхронность, а про то, что worker не должен
	// зависеть от того, успел ли caller к моменту отправки: он обязан
	// завершиться и не держать ресурсы AEWP.
	pidCh := make(chan privilegedStartResult, 1)
	go func() {
		scriptPID, singboxPID, runErr := deps.start(corePath, binDir, configName)
		// Только публикация результата. Никаких блокировок: CmdMutex держит
		// caller, и любой Lock здесь — гарантированный deadlock.
		pidCh <- privilegedStartResult{Script: scriptPID, Singbox: singboxPID, Err: runErr}
	}()

	var pids privilegedStartResult
	timedOut := false
	select {
	case pids = <-pidCh:
	case <-time.After(privilegedStartTimeout):
		// The user stopped waiting, but the OPERATION has not stopped: the
		// authorization dialog is still on screen and the worker will still
		// return a result — possibly a running ROOT core.
		//
		// This is the orphan case. Returning here and letting the worker write
		// into a buffered channel nobody reads leaves a root sing-box holding the
		// TUN, with no RunningState, no owner and no waiter. Handing the channel
		// to a supervisor is what makes the late result actionable: it is either
		// adopted (if this generation is still current) or stopped (if not).
		timedOut = true
		go svc.superviseLatePrivilegedStart(gen, corePath, pidCh)
		return fmt.Errorf("privileged start did not return within %s (authorization still pending?)", privilegedStartTimeout)
	}

	_ = timedOut
	if pids.Err != nil {
		// Отказ/отмена авторизации: ничего не коммитим, состояние не трогаем.
		// CmdMutex отпустит defer в Start().
		debuglog.WarnLog("startSingBox: privileged run failed: %v", pids.Err)
		return fmt.Errorf("privileged start failed: %w", pids.Err)
	}
	if pids.Script <= 0 {
		return fmt.Errorf("privileged start failed or cancelled (no PID)")
	}

	// Caller владеет CmdMutex — коммитим состояние здесь, до возврата.
	if !svc.commitPrivilegedStartLocked(gen, pids.Script, pids.Singbox, pidFilePath, corePath) {
		// Superseded between the authorization returning and this commit: the
		// root core is up and this generation no longer owns anything. Stopping
		// it is the only correct outcome — adopting it would run a core for an
		// engine the user has already left.
		debuglog.WarnLog("startSingBox: generation %d was superseded during privileged start; stopping the root core (PIDs %d/%d)",
			gen, pids.Script, pids.Singbox)
		svc.stopSupersededPrivileged(pids.Script, pids.Singbox, pidFilePath, corePath)
		return ErrStartAborted
	}
	// pid-файл пишем как пользователь; ошибка не глотается (SPEC 145).
	svc.writePIDFile(pidFilePath, pids.Script, pids.Singbox)
	debuglog.DebugLog("startSingBox: Sing-Box started with privileges (script PID=%d, sing-box PID=%d).", pids.Script, pids.Singbox)

	// Долгое ожидание выхода root-процесса — в отдельной горутине, вне пути
	// старта: она не держит CmdMutex и не задерживает возврат Start().
	//
	// The generation is captured HERE and checked inside the callback: this
	// goroutine can outlive the runtime that created it (the user switches to
	// the daemon engine while the root core is running), and without the check
	// its exit would be applied to a runtime it does not belong to.
	go func(scriptPID int, g uint64) {
		deps.waitExit(scriptPID)
		if !ac.classic.isCurrent(g) {
			debuglog.InfoLog("startSingBox: privileged waiter for generation %d is stale (current=%d); ignoring exit",
				g, ac.classic.currentGeneration())
			return
		}
		svc.onPrivilegedScriptExited(g)
	}(pids.Script, gen)

	go ac.delayedAutoLoadProxies(gen)
	return nil
}

// autoLoadProxiesDelay is how long the proxy auto-load waits for the core's API
// to come up. A constant so its tests and its callers agree on one value.
const autoLoadProxiesDelay = 2 * time.Second

// delayedAutoLoadProxies reloads the proxy list once the core's API should be
// ready, but ONLY while the generation that asked for it still owns the runtime.
//
// The delay is what makes the check necessary rather than defensive: two seconds
// is long enough for the user to press Stop, switch engines, or start a new core,
// and the reload would then act on a runtime that never requested it.
func (ac *AppController) delayedAutoLoadProxies(gen uint64) {
	select {
	case <-time.After(autoLoadProxiesDelay):
	case <-ac.contextDone():
		return
	}
	if !ac.classic.isCurrent(gen) {
		debuglog.InfoLog("delayedAutoLoadProxies: generation %d is stale (current=%d); not reloading proxies",
			gen, ac.classic.currentGeneration())
		return
	}
	if ac.RunningState == nil || !ac.RunningState.IsRunning() {
		debuglog.InfoLog("delayedAutoLoadProxies: generation %d no longer has a running core; not reloading proxies", gen)
		return
	}
	ac.AutoLoadProxies()
}

// contextDone returns a channel closed when the app context ends, or nil when
// there is no context (the app is not shutting down).
func (ac *AppController) contextDone() <-chan struct{} {
	if ac == nil || ac.ctx == nil {
		return nil
	}
	return ac.ctx.Done()
}

// onPrivilegedScriptExited is called when the privileged script process exits (Wait4 returned).
// The script waits on sing-box, so when the script exits, sing-box has exited too.
// onPrivilegedScriptExited applies the exit of the wrapper process that owned
// generation `gen`.
//
// THE GENERATION IS A PARAMETER, NOT A LOOKUP. The previous version read
// `currentGeneration()` and then checked `isCurrent()` on that same value — a
// tautology that is true by construction, so the guard could never fire. The
// caller's knowledge (which generation's process actually exited) was discarded at
// the call boundary, and the function then wrote ownership and phase for whatever
// generation happened to be current.
//
// The consequence is the one the check was written to prevent: a wrapper belonging
// to a generation the user has already left — engine switched to the daemon, or
// restarted — would be reported as the live one, clearing a runtime it does not
// own or triggering a restart for a world that is gone.
func (svc *ProcessService) onPrivilegedScriptExited(gen uint64) {
	ac := svc.ac
	if ac == nil {
		return
	}
	// This function still has a direct call site in the privileged exit path, so
	// the check lives here as well as in the waiter goroutine: a state write
	// without it would let a dead generation clear a live one's ownership.
	if !ac.classic.isCurrent(gen) {
		debuglog.InfoLog("onPrivilegedScriptExited: stale generation %d, ignoring (current=%d)",
			gen, ac.classic.currentGeneration())
		return
	}
	// NO deferred Unlock in this function.
	//
	// It releases the lock mid-body (the restart path must not hold it across a
	// 2s delay and a Start call), and a deferred Unlock combined with a manual one
	// has now produced `fatal error: sync: unlock of unlocked mutex` TWICE — an
	// unrecoverable crash, not an error any caller can handle. Every early return
	// below would otherwise have to remember to re-acquire first, and the ones
	// that forgot are exactly how those crashes happened.
	//
	// Ownership is explicit instead: each return states whether it holds the lock,
	// and the function is written so that the manual Unlock and the final Lock
	// balance on every path. KillForRestart and ForceStopOwnedCore already follow
	// this discipline and have never had the bug.
	ac.CmdMutex.Lock()
	if !ac.classic.isCurrent(gen) {
		debuglog.InfoLog("onPrivilegedScriptExited: generation became stale while acquiring the lock, ignoring")
		ac.CmdMutex.Unlock()
		return
	}
	if !ac.SingboxPrivilegedMode {
		ac.CmdMutex.Unlock()
		return
	}
	// The owned root process is gone (the wrapper exited and the waiter
	// returned), so ownership is cleared together with the phase. Clearing
	// ownership here rather than only the individual fields is what keeps the
	// identity from outliving the process it describes.
	ac.classic.clearOwnership(gen)
	ac.classic.setPhase(gen, ClassicStopped)
	ac.SingboxPrivilegedMode = false
	ac.SingboxPrivilegedPID = 0
	ac.SingboxPrivilegedSingboxPID = 0
	ac.SingboxPrivilegedPIDFile = ""
	ac.RunningState.Set(false)

	// SPEC 070: shared crash/restart decision. Privileged путь не имеет err
	// (скрипт ждёт sing-box; cmd.Wait отсутствует) → cleanExit=false, поэтому
	// actionClean здесь не возникает.
	//
	// SPEC 143: причина классифицируется по логу ядра — детерминированный
	// отказ (права, порт, конфиг) не перезапускается.
	reason := ac.classifyCoreExitReason(gen)
	action, newAttempts := decideCrashActionReason(ac.StoppedByUser, ac.RestartRequestedByUser, false, ac.ConsecutiveCrashAttempts, restartAttempts, reason)
	ac.ConsecutiveCrashAttempts = newAttempts
	switch action {
	case actionStoppedByUser:
		ac.StoppedByUser = false
		debuglog.InfoLog("onPrivilegedScriptExited: Stopped by user.")
		ac.CmdMutex.Unlock()
		return
	case actionUserRestart:
		ac.RestartRequestedByUser = false
		debuglog.InfoLog("onPrivilegedScriptExited: Restart requested by user, starting sing-box...")
		ac.CmdMutex.Unlock()
		// Generation re-checked after releasing the lock: this is the window in
		// which a mode switch or a newer start can supersede us, and restarting
		// into someone else's runtime is the failure this guards.
		//
		// The lock is already released here, so this return must NOT unlock again.
		// That is the whole point of dropping the defer: with it armed, this
		// return performed a second Unlock and killed the process.
		if !ac.classic.isCurrent(gen) {
			debuglog.InfoLog("onPrivilegedScriptExited: superseded before restart, not restarting")
			return
		}
		runGhostTunCleanup(true)
		svc.Start(true)
		{
			// ac.ui() is nil-safe: the headless backend never attaches a UI, so
			// calling through the field would panic on this path — which runs on
			// every restart.
			ac.ui().UpdateCoreStatus()
		}
		ac.CmdMutex.Lock()
		return
	case actionDeterministicFailure:
		debuglog.WarnLog("onPrivilegedScriptExited: deterministic failure (%s), not restarting", reason)
		ac.showDeterministicExitDialog(reason)
		ac.CmdMutex.Unlock()
		return
	case actionMaxAttempts:
		debuglog.DebugLog("onPrivilegedScriptExited: Max restart attempts reached.")
		// Recorded before the UI branch: exhausting the restart budget is a
		// lifecycle failure the frontend must show, and the headless backend has
		// no uiPort to show it with.
		msg := locale.Tf("Sing-Box failed to restart after %d attempts. Check sing-box.log for details.", restartAttempts)
		ac.RecordLifecycleError(LifecycleErrRestartExhausted, "restart", msg, "", false)
		if ac.uiPort != nil {
			ac.uiPort.ShowError(locale.T("Error"), msg)
		}
		ac.CmdMutex.Unlock()
		return
	}
	// action == actionCrashRestart
	debuglog.WarnLog("onPrivilegedScriptExited: Sing-Box exited, auto-restart (attempt %d/%d)", ac.ConsecutiveCrashAttempts, restartAttempts)
	if ac.uiPort != nil {
		ac.ui().ShowInfo(locale.T("Crash"), locale.Tf("Sing-Box crashed, restarting... (attempt %d/%d)", ac.ConsecutiveCrashAttempts, restartAttempts))
	}
	ac.CmdMutex.Unlock()
	<-time.After(2 * time.Second)
	// Same window as the child-process monitor, same guard: after the delay the
	// runtime may belong to another engine entirely.
	//
	// No re-acquire: this function has no deferred Unlock any more, so a return
	// here simply leaves the lock released. (Under the old defer this return had
	// to re-take the lock; that requirement existed only to satisfy the defer and
	// is what made the balancing invisible.)
	if !ac.classic.isCurrent(gen) {
		debuglog.InfoLog("onPrivilegedScriptExited: generation %d superseded during the restart delay; not restarting", gen)
		return
	}
	svc.Start(true)
	ac.CmdMutex.Lock()
	if ac.RunningState.IsRunning() {
		currentAttemptCount := ac.ConsecutiveCrashAttempts
		go func() {
			if ctxutil.SleepWithContext(ac.ctx, stabilityThreshold) != nil {
				return
			}
			ac.CmdMutex.Lock()
			defer ac.CmdMutex.Unlock()
			if ac.RunningState.IsRunning() && ac.ConsecutiveCrashAttempts == currentAttemptCount {
				ac.ConsecutiveCrashAttempts = 0
				{
					ac.ui().UpdateCoreStatus()
				}
			}
		}()
	}
}

// Monitor tracks the sing-box process and auto-restarts on crash (same logic as before).
//
// THE GENERATION CHECK IS THE POINT. This goroutine blocks in Wait() for as long
// as the core lives, then unlocks and may start a NEW core. In between, the world
// can change: the user switches to the daemon engine, the app shuts down, or a
// newer start supersedes this one. The PID comparison that used to guard this
// (is ac.SingboxCmd still my PID?) cannot express those cases — a switched engine
// clears the process state, and "no cmd" was indistinguishable from "nothing to
// do", so a crash monitor in its restart delay would wake up and start a classic
// core next to the daemon's.
//
// The generation captured at spawn is checked before ANY state write and before
// any restart. A stale monitor is a complete no-op: it must not set state, clear
// ownership, count a crash, or start anything.
func (svc *ProcessService) Monitor(cmdToMonitor *exec.Cmd) {
	ac := svc.ac
	// Store the PID we're monitoring to avoid conflicts with restarted processes
	monitoredPID := cmdToMonitor.Process.Pid
	// The generation this process belongs to, captured at creation.
	monGen := ac.classic.currentGeneration()

	// Wait for process completion - no timeout for long-running processes
	// The process should exit or be stopped by user
	err := cmdToMonitor.Wait()

	// STALE MONITOR GATE — before the lock, before any state.
	if !ac.classic.isCurrent(monGen) {
		debuglog.InfoLog("monitorSingBox: monitor for generation %d is stale (current=%d); ignoring exit of PID %d",
			monGen, ac.classic.currentGeneration(), monitoredPID)
		return
	}

	// NO deferred Unlock in this function — see the note in
	// onPrivilegedScriptExited. Monitor releases the lock mid-body before the 2s
	// crash-restart delay, so a deferred Unlock would fire on the guarded returns
	// that follow it and crash the process with `fatal error: sync: unlock of
	// unlocked mutex`. Those two returns are exactly the ones that fire during a
	// mode switch, i.e. the case this function exists to handle.
	//
	// Each return below states whether it holds the lock.
	ac.CmdMutex.Lock()

	// Re-check under the lock: the generation can change between the gate above
	// and acquiring CmdMutex (that is exactly the mode-switch window).
	if !ac.classic.isCurrent(monGen) {
		debuglog.InfoLog("monitorSingBox: generation %d became stale while acquiring the lock; ignoring exit", monGen)
		ac.CmdMutex.Unlock()
		return
	}

	// GOLDEN STANDARD: Check order to prevent all race conditions
	// 1. First PID (is this my process?)
	if ac.SingboxCmd == nil || ac.SingboxCmd.Process == nil || ac.SingboxCmd.Process.Pid != monitoredPID {
		debuglog.DebugLog("monitorSingBox: Process was restarted (PID changed from %d). This monitor is obsolete. Exiting.", monitoredPID)
		ac.CmdMutex.Unlock()
		return
	}

	// 1b. Has this exit already been classified?
	//
	// The readiness gate waits on the SAME process, so a core that dies during
	// startup is observed by both. The gate owns that case and reports it as a
	// failed START; if the monitor also classified it, it would call it a crash
	// and auto-restart a core whose config had just been rejected — the restart
	// loop the readiness gate was introduced to prevent. Whoever claims the exit
	// first decides, and the other side leaves the state alone.
	//
	// Checked under CmdMutex, like every other decision in this function: the
	// gate cannot run concurrently with this block, so the claim is authoritative
	// at the moment it is read.
	if ac.classic.exitAlreadyClaimed(monGen) {
		debuglog.InfoLog("monitorSingBox: the exit of PID %d was already classified during startup; not treating it as a crash", monitoredPID)
		ac.CmdMutex.Unlock()
		return
	}
	if !ac.classic.claimExit(monGen) {
		debuglog.InfoLog("monitorSingBox: could not claim the exit of PID %d (generation moved on); ignoring", monitoredPID)
		ac.CmdMutex.Unlock()
		return
	}

	// SPEC 070: shared crash/restart decision (steps 2-5). PID-check (step 1)
	// stays above; err==nil graceful-exit is fed via cleanExit so the decision
	// lives in one place but Monitor keeps its per-branch RunningState placement.
	//
	// SPEC 143: причина берётся из лога ядра. Детерминированная ошибка
	// (конфиг, права, порт) прекращает авто-перезапуск сразу, а не после
	// трёх одинаковых попыток.
	reason := ac.classifyCoreExitReason(monGen)
	action, newAttempts := decideCrashActionReason(ac.StoppedByUser, ac.RestartRequestedByUser, err == nil, ac.ConsecutiveCrashAttempts, restartAttempts, reason)

	// 2. Then StoppedByUser (did user stop it?)
	if action == actionStoppedByUser {
		debuglog.InfoLog("monitorSingBox: Sing-Box exited as requested by user.")
		ac.ConsecutiveCrashAttempts = newAttempts
		ac.RunningState.Set(false)
		ac.StoppedByUser = false // Reset flag for next start
		// SPEC 065: cleanup phantom TUN-адаптеров на Win7 после exit sing-box.
		triggerGhostTunCleanup()
		ac.CmdMutex.Unlock()
		return
	}

	// 3. Restart by user (Restart button): bring process back up immediately
	if action == actionUserRestart {
		ac.RestartRequestedByUser = false
		ac.RunningState.Set(false)
		ac.ConsecutiveCrashAttempts = newAttempts
		debuglog.InfoLog("monitorSingBox: Restart requested by user, starting sing-box...")
		// SPEC 065: sync cleanup до Start — sing-box мёртв, нового адаптера ещё нет.
		ac.CmdMutex.Unlock()
		runGhostTunCleanup(true)
		svc.Start(true)
		{
			// nil-safe accessor: see the note on the restart path above.
			ac.ui().UpdateCoreStatus() // refresh "Restarting..." → "Running" or "Stopped" if start failed
		}
		ac.CmdMutex.Lock()
		return
	}

	// 4. Then err == nil (exited normally, not restart) — do not restart
	if action == actionClean {
		debuglog.WarnLog("monitorSingBox: Sing-Box exited gracefully (exit code 0).")
		ac.ConsecutiveCrashAttempts = newAttempts
		ac.RunningState.Set(false)
		ac.CmdMutex.Unlock()
		return
	}

	// 4b. Детерминированная ошибка (SPEC 143): повтор даст тот же
	// результат, поэтому не перезапускаем и показываем конкретную причину
	// с действием, а не «не удалось перезапустить».
	if action == actionDeterministicFailure {
		debuglog.WarnLog("monitorSingBox: deterministic failure (%s), not restarting", reason)
		ac.ConsecutiveCrashAttempts = newAttempts
		ac.RunningState.Set(false)
		ac.showDeterministicExitDialog(reason)
		ac.CmdMutex.Unlock()
		return
	}

	// 5. Crash → restart with delay and attempt limit
	ac.RunningState.Set(false)
	ac.ConsecutiveCrashAttempts = newAttempts

	if action == actionMaxAttempts {
		debuglog.DebugLog("monitorSingBox: Maximum restart attempts (%d) reached. Stopping auto-restart.", restartAttempts)
		// Recorded before the UI branch: exhausting the restart budget is a
		// lifecycle failure the frontend must show, and the headless backend has
		// no uiPort to show it with.
		msg := locale.Tf("Sing-Box failed to restart after %d attempts. Check sing-box.log for details.", restartAttempts)
		ac.RecordLifecycleError(LifecycleErrRestartExhausted, "restart", msg, "", false)
		if ac.uiPort != nil {
			ac.uiPort.ShowError(locale.T("Error"), msg)
		}
		ac.CmdMutex.Unlock()
		return
	}

	// action == actionCrashRestart
	debuglog.WarnLog("monitorSingBox: Sing-Box crashed: %v, attempting auto-restart (attempt %d/%d)", err, ac.ConsecutiveCrashAttempts, restartAttempts)
	if ac.uiPort != nil {
		ac.ui().ShowInfo(locale.T("Crash"), locale.Tf("Sing-Box crashed, restarting... (attempt %d/%d)", ac.ConsecutiveCrashAttempts, restartAttempts))
	}

	ac.CmdMutex.Unlock()
	<-time.After(2 * time.Second)

	// THE MODE-SWITCH WINDOW, closed.
	//
	// These two seconds are precisely where the reported race lived: a crash
	// enters the restart delay, the user switches to the daemon engine, the delay
	// expires — and the old code called svc.Start(true) unconditionally,
	// bringing a classic core up alongside the daemon's. Two engines, one VPN,
	// no way for the user to tell which one owns the TUN.
	//
	// Re-checking the generation AFTER the wait (not just before it) makes the
	// stale case impossible: a switch renews the generation, so this monitor's
	// claim is void and it must do nothing at all. Not even cleanup — the
	// TUN-ghost cleanup below would otherwise touch devices belonging to the
	// engine that now owns them.
	if !ac.classic.isCurrent(monGen) {
		debuglog.InfoLog("monitorSingBox: generation %d was superseded during the restart delay; not restarting", monGen)
		return
	}

	// SPEC 065 hotfix (v0.9.9.1): cleanup phantom singbox-tun adapter from
	// the just-crashed sing-box BEFORE auto-restart. Без этого хука каждый
	// retry создаёт новый адаптер (singbox-tun0 → tun1 → tun2) потому что
	// старое имя ещё занято dead-but-not-cleaned-up phantom'ом. На 3
	// попытки = 3 phantom-адаптера, даже если юзер вообще не нажимал Stop.
	// Sync mode: cleanup точно завершится ДО Start.
	runGhostTunCleanup(true)
	// Start(true) runs on THIS generation's behalf; a supersede between the
	// check above and the call itself is handled inside Start by beginOperation.
	if !ac.classic.isCurrent(monGen) {
		debuglog.InfoLog("monitorSingBox: generation %d was superseded before restart; not restarting", monGen)
		return
	}
	svc.Start(true)
	ac.CmdMutex.Lock()

	if ac.RunningState.IsRunning() {
		debuglog.InfoLog("monitorSingBox: Sing-Box restarted successfully.")
		currentAttemptCount := ac.ConsecutiveCrashAttempts
		go func() {
			if ctxutil.SleepWithContext(ac.ctx, stabilityThreshold) != nil {
				debuglog.InfoLog("monitorSingBox: Stability check cancelled (context cancelled)")
				return
			}
			ac.CmdMutex.Lock()
			defer ac.CmdMutex.Unlock()
			if ac.RunningState.IsRunning() && ac.ConsecutiveCrashAttempts == currentAttemptCount {
				debuglog.DebugLog("monitorSingBox: Process has been stable for %v. Resetting crash counter from %d to 0.", stabilityThreshold, ac.ConsecutiveCrashAttempts)
				ac.ConsecutiveCrashAttempts = 0
				{
					ac.ui().UpdateCoreStatus()
				}
			}
		}()
	} else {
		debuglog.DebugLog("monitorSingBox: Restart attempt %d failed.", ac.ConsecutiveCrashAttempts)
	}
}

// ForceStopOwnedCore is the shutdown-time last resort: it terminates whatever
// core this launcher currently OWNS and verifies the exit, rather than killing a
// bare PID.
//
// GracefulExit used to fall back to `ac.SingboxCmd.Process.Kill()` on timeout.
// That had two problems, both of which the identity-verified primitive exists to
// solve: it is a raw PID signal with no check that the PID is still our core (a
// recycled PID means signalling an unrelated process in the final seconds of
// shutdown), and it cannot reach a privileged root core at all — SingboxCmd is
// nil on that path, so the "forcing kill" logged a line and did nothing while the
// TUN stayed up.
//
// Returns true when a core was owned and is now confirmed gone.
func (svc *ProcessService) ForceStopOwnedCore() bool {
	if svc == nil || svc.ac == nil {
		return false
	}
	ac := svc.ac
	ac.CmdMutex.Lock()
	owned, hasOwned, privileged := ac.classic.ownedProcess()
	if !hasOwned {
		ac.CmdMutex.Unlock()
		return !ac.RunningState.IsRunning()
	}
	scriptPID := ac.SingboxPrivilegedPID
	singboxPID := ac.SingboxPrivilegedSingboxPID
	pidFile := ac.SingboxPrivilegedPIDFile
	corePath := svc.currentCorePath()
	cmd := ac.SingboxCmd
	ac.CmdMutex.Unlock()

	checker := platformChecker{}
	var err error
	if privileged {
		_, err = terminatePrivilegedOwned(scriptPID, singboxPID, pidFile, corePath, "shutdown-force", checker)
	} else {
		_, err = terminateOwnedProcess(owned, "shutdown-force", checker, func(pid int, force bool) error {
			return signalLocalProcess(cmd, pid, force)
		})
	}
	if err != nil {
		debuglog.WarnLog("GracefulExit: forced stop could not confirm exit: %v", err)
		return false
	}
	gen := ac.classic.currentGeneration()
	ac.classic.clearOwnership(gen)
	ac.classic.setPhase(gen, ClassicStopped)
	ac.noteTeardown(events.TeardownUserStop)
	ac.RunningState.Set(false)
	if pidFile != "" {
		_ = os.Remove(pidFile)
	}
	return true
}

// Stop attempts graceful shutdown, mirroring previous StopSingBoxProcess.
// Stop terminates the owned core and does not report success until it is gone.
//
// THE INVARIANT: "stopped" means the launcher has CONFIRMED that the core it
// owns has exited. It does not mean "a signal was sent". The old implementation
// sent SIGTERM and immediately set RunningState=false, so a root sing-box that
// ignored the signal kept the TUN up and its routes installed while the UI
// offered to start another one.
//
// OWNERSHIP, NOT PHASE. The old entry check was `if !RunningState.IsRunning()
// { return }`, which is a statement about the launcher's BELIEF. A logical state
// that says stopped while a root core is up is exactly the desynchronisation
// this must survive: the process exists, we own it, and the user asked for it to
// stop. So the decision is made from the recorded process identity, and the
// phase is only bookkeeping.
func (svc *ProcessService) Stop() {
	if svc.stopOverrideForTest != nil {
		svc.stopOverrideForTest()
		return
	}
	ac := svc.ac

	ac.CmdMutex.Lock()
	owned, hasOwned, privileged := ac.classic.ownedProcess()
	if !hasOwned {
		// Nothing recorded as ours. Fall back to the legacy fields only when
		// they describe a process we can still verify — a recorded PID without a
		// verified executable is not something to signal.
		if ac.SingboxPrivilegedMode && ac.SingboxPrivilegedPID != 0 {
			owned = ProcessIdentity{PID: ac.SingboxPrivilegedPID, Executable: svc.privilegedCorePath()}
			hasOwned = owned.Executable != ""
			privileged = true
		} else if ac.SingboxCmd != nil && ac.SingboxCmd.Process != nil {
			owned = ProcessIdentity{PID: ac.SingboxCmd.Process.Pid, Executable: svc.currentCorePath()}
			hasOwned = owned.Executable != ""
		}
	}
	if !hasOwned {
		// Genuinely nothing to stop.
		debuglog.InfoLog("stopSingBox: no owned process to stop")
		ac.RunningState.Set(false)
		ac.classic.setPhase(ac.classic.currentGeneration(), ClassicStopped)
		ac.CmdMutex.Unlock()
		return
	}

	// Intent is recorded BEFORE the signal and belongs to this operation: the
	// monitor must see "the user stopped this" even if the process exits
	// instantly, or a deliberate stop is misread as a crash and auto-restarted.
	gen := ac.classic.currentGeneration()
	ac.classic.setIntent(gen, true, false)
	ac.classic.setPhase(gen, ClassicStopping)
	ac.StoppedByUser = true
	ac.ConsecutiveCrashAttempts = 0
	ac.classic.clearIntent(gen)

	// Everything needed for termination is captured under the lock; the signals
	// themselves run without it so a slow exit does not block the UI thread.
	cmd := ac.SingboxCmd
	scriptPID := ac.SingboxPrivilegedPID
	singboxPID := ac.SingboxPrivilegedSingboxPID
	pidFile := ac.SingboxPrivilegedPIDFile
	corePath := svc.currentCorePath()
	ac.CmdMutex.Unlock()

	checker := platformChecker{}
	var termErr error

	if privileged {
		debuglog.InfoLog("stopSingBox: stopping privileged core (script PID %d, sing-box PID %d)", scriptPID, singboxPID)
		_, termErr = terminatePrivilegedOwned(scriptPID, singboxPID, pidFile, corePath, "stop", checker)
	} else {
		debuglog.InfoLog("stopSingBox: stopping core (PID %d)", owned.PID)
		_, termErr = terminateOwnedProcess(owned, "stop", checker, func(pid int, force bool) error {
			return signalLocalProcess(cmd, pid, force)
		})
	}

	ac.CmdMutex.Lock()
	defer ac.CmdMutex.Unlock()

	if termErr != nil {
		// NOT stopped. Saying otherwise would be the original lie: the UI would
		// offer Start while a root core still holds the interface.
		debuglog.ErrorLog("stopSingBox: could not confirm exit: %v", termErr)
		// Keep ownership: the process may still be alive, and dropping the
		// identity would make it unreachable for the next stop attempt.
		ac.classic.setPhase(gen, ClassicFailed)
		ac.RecordLifecycleError(LifecycleErrStopFailed, "stop",
			"the core could not be stopped", termErr.Error(), true)
		if ac.hasUI() && !ac.IsExiting() {
			ac.uiPort.ShowError(locale.T("Error"), locale.T(stopPrivilegedFailedText)+": "+termErr.Error())
		}
		return
	}

	// Confirmed gone.
	ac.classic.clearOwnership(gen)
	ac.classic.setPhase(gen, ClassicStopped)
	ac.SingboxPrivilegedMode = false
	ac.SingboxPrivilegedPID = 0
	ac.SingboxPrivilegedSingboxPID = 0
	ac.SingboxPrivilegedPIDFile = ""
	ac.StoppedByUser = false
	ac.noteTeardown(events.TeardownUserStop)
	ac.RunningState.Set(false)
	if pidFile != "" {
		_ = os.Remove(pidFile)
	}
	// The TUN interface is released only now, after the process that owned it is
	// confirmed dead: cleaning up while it is still alive races the core's own
	// teardown and can leave a phantom adapter.
	triggerGhostTunCleanup()
}

// signalLocalProcess sends a graceful or forced signal to a locally owned child.
//
// Identity was verified by the caller before this point; the verified PID is
// passed explicitly so the signal can never be aimed at a recycled one.
func signalLocalProcess(cmd *exec.Cmd, pid int, force bool) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d", pid)
	}
	// Prefer the exec.Cmd handle when it is the same process: signaling through
	// it cannot target anything else.
	if cmd != nil && cmd.Process != nil && cmd.Process.Pid == pid {
		if force {
			return cmd.Process.Kill()
		}
		if runtime.GOOS == "windows" {
			return platform.KillProcessByPID(pid)
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	if force {
		return platform.KillProcessByPID(pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return platform.KillProcessByPID(pid)
	}
	return proc.Signal(os.Interrupt)
}

// coreBinaryPath returns the core binary path, or "" when the controller is not
// fully wired yet. Used on paths that can run on a background goroutine, where a
// nil dereference would take the whole process down rather than fail one start.
func (ac *AppController) coreBinaryPath() string {
	if ac == nil || ac.FileService == nil {
		return ""
	}
	return ac.FileService.SingboxPath
}

// currentCorePath returns the core binary path this launcher would run.
func (svc *ProcessService) currentCorePath() string {
	if svc == nil || svc.ac == nil || svc.ac.FileService == nil {
		return ""
	}
	return svc.ac.FileService.SingboxPath
}

// privilegedCorePath returns the path of the root-owned core copy, which is the
// executable identity a privileged core must match.
func (svc *ProcessService) privilegedCorePath() string {
	if svc == nil || svc.ac == nil {
		return ""
	}
	if path, err := svc.ac.privilegedCoreCopyGate(); err == nil && path != "" {
		return path
	}
	return svc.currentCorePath()
}

// KillForRestart stops the owned core and asks for a restart.
//
// Uses the SAME termination primitive as Stop. Previously restart was a second,
// weaker implementation: it sent the interrupt and returned, relying on the
// crash watcher to notice the exit and bring the process back. When the signal
// was ignored — a wedged core — restart never happened at all: no timeout, no
// forced kill, no error, and a Restart button that appeared dead. Stop had a
// watchdog; restart did not. One primitive, two policies, is the fix.
//
// The restart intent is OPERATION-SCOPED. It is recorded only after the
// termination is confirmed and consumed by whoever acts on it, so a failed
// restart cannot leave a sticky "the user wants a restart" that makes an
// unrelated later exit start a core nobody asked for.
func (svc *ProcessService) KillForRestart() {
	ac := svc.ac
	ac.CmdMutex.Lock()

	owned, hasOwned, privileged := ac.classic.ownedProcess()
	if !hasOwned {
		if ac.SingboxPrivilegedMode && ac.SingboxPrivilegedPID != 0 {
			owned = ProcessIdentity{PID: ac.SingboxPrivilegedPID, Executable: svc.privilegedCorePath()}
			hasOwned = owned.Executable != ""
			privileged = true
		} else if ac.SingboxCmd != nil && ac.SingboxCmd.Process != nil {
			owned = ProcessIdentity{PID: ac.SingboxCmd.Process.Pid, Executable: svc.currentCorePath()}
			hasOwned = owned.Executable != ""
		}
	}
	if !hasOwned {
		// Nothing to restart from. Starting fresh is what the user wants, and it
		// is honest: there was no process to preserve.
		debuglog.InfoLog("KillForRestart: nothing owned to restart; starting instead")
		ac.CmdMutex.Unlock()
		svc.Start(true)
		return
	}

	gen := ac.classic.currentGeneration()
	// Mark the intent for THIS generation before the signal, so the exit that
	// follows is understood as deliberate rather than a crash.
	ac.classic.setIntent(gen, false, true)
	ac.classic.setPhase(gen, ClassicRestarting)
	ac.RestartRequestedByUser = true

	cmd := ac.SingboxCmd
	scriptPID := ac.SingboxPrivilegedPID
	singboxPID := ac.SingboxPrivilegedSingboxPID
	pidFile := ac.SingboxPrivilegedPIDFile
	corePath := svc.currentCorePath()
	ac.CmdMutex.Unlock()

	checker := platformChecker{}
	var termErr error
	if privileged {
		debuglog.InfoLog("KillForRestart: stopping privileged core (script PID %d, sing-box PID %d)", scriptPID, singboxPID)
		_, termErr = terminatePrivilegedOwned(scriptPID, singboxPID, pidFile, corePath, "restart", checker)
	} else {
		debuglog.InfoLog("KillForRestart: stopping core (PID %d)", owned.PID)
		_, termErr = terminateOwnedProcess(owned, "restart", checker, func(pid int, force bool) error {
			return signalLocalProcess(cmd, pid, force)
		})
	}

	ac.CmdMutex.Lock()
	if termErr != nil {
		// The restart did not happen. Clear the intent rather than leaving it
		// set: a sticky restart flag turns the NEXT, unrelated exit into an
		// unwanted start. Reporting the failure is what stops the user waiting
		// for a restart that will never come.
		ac.classic.clearIntent(gen)
		ac.RestartRequestedByUser = false
		ac.classic.setPhase(gen, ClassicFailed)
		debuglog.ErrorLog("KillForRestart: could not confirm exit: %v", termErr)
		ac.RecordLifecycleError(LifecycleErrStopFailed, "restart",
			"the core could not be stopped for a restart", termErr.Error(), true)
		ac.CmdMutex.Unlock()
		return
	}

	// Confirmed gone. Ownership is dropped here and the intent is left for the
	// restart to consume; the exit watcher may already have raced us, in which
	// case it saw the intent and is starting the replacement itself.
	ac.classic.clearOwnership(gen)
	ac.SingboxPrivilegedMode = false
	ac.SingboxPrivilegedPID = 0
	ac.SingboxPrivilegedSingboxPID = 0
	ac.SingboxPrivilegedPIDFile = ""
	ac.noteTeardown(events.TeardownRestart)
	ac.RunningState.Set(false)
	if pidFile != "" {
		_ = os.Remove(pidFile)
	}
	ac.CmdMutex.Unlock()

	// The watcher (Monitor or the privileged waiter) normally observes the exit
	// and performs the restart. If the process was adopted — no watcher exists —
	// or the watcher is gone, nothing would bring it back, so the restart is
	// performed here. takeRestartIntent makes this mutually exclusive with the
	// watcher's own handling.
	if !ac.classic.isCurrent(gen) {
		debuglog.InfoLog("KillForRestart: generation %d was superseded; not restarting", gen)
		return
	}
	runGhostTunCleanup(true)
	svc.Start(true)
}

// RestartContext restarts the classic core and returns the REAL outcome.
//
// The contextual counterpart of KillForRestart, for the IPC path that must be
// able to report a reason. KillForRestart reports failures by recording a
// lifecycle error and returning nothing; a headless caller cannot distinguish
// "restarted" from "the core could not be stopped", so this returns the error
// as well.
//
// It performs the same teardown and then AWAITS the replacement start, which is
// what makes the caller's operation end at a real commit point rather than at
// the moment a goroutine was launched.
func (svc *ProcessService) RestartContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ac := svc.ac
	ac.CmdMutex.Lock()

	owned, hasOwned, privileged := ac.classic.ownedProcess()
	if !hasOwned {
		if ac.SingboxPrivilegedMode && ac.SingboxPrivilegedPID != 0 {
			owned = ProcessIdentity{PID: ac.SingboxPrivilegedPID, Executable: svc.privilegedCorePath()}
			hasOwned = owned.Executable != ""
			privileged = true
		} else if ac.SingboxCmd != nil && ac.SingboxCmd.Process != nil {
			owned = ProcessIdentity{PID: ac.SingboxCmd.Process.Pid, Executable: svc.currentCorePath()}
			hasOwned = owned.Executable != ""
		}
	}
	if !hasOwned {
		// Nothing to restart from: the user asked for a running core, and
		// starting one is the honest way to provide it.
		debuglog.InfoLog("RestartContext: nothing owned to restart; starting instead")
		ac.CmdMutex.Unlock()
		return svc.StartContext(ctx, true)
	}

	gen := ac.classic.currentGeneration()
	ac.classic.setIntent(gen, false, true)
	ac.classic.setPhase(gen, ClassicRestarting)
	ac.RestartRequestedByUser = true

	cmd := ac.SingboxCmd
	scriptPID := ac.SingboxPrivilegedPID
	singboxPID := ac.SingboxPrivilegedSingboxPID
	pidFile := ac.SingboxPrivilegedPIDFile
	corePath := svc.currentCorePath()
	ac.CmdMutex.Unlock()

	// The termination helpers poll with their own bounded timeouts, so the
	// context is checked around them rather than threaded into every poll. A
	// cancelled restart abandons the WAIT, which is what the caller needs; the
	// teardown itself must run to completion or the core would be left in a
	// half-killed state.
	if err := ctx.Err(); err != nil {
		ac.CmdMutex.Lock()
		ac.classic.clearIntent(gen)
		ac.RestartRequestedByUser = false
		ac.CmdMutex.Unlock()
		return err
	}

	checker := platformChecker{}
	var termErr error
	if privileged {
		_, termErr = terminatePrivilegedOwned(scriptPID, singboxPID, pidFile, corePath, "restart", checker)
	} else {
		_, termErr = terminateOwnedProcess(owned, "restart", checker, func(pid int, force bool) error {
			return signalLocalProcess(cmd, pid, force)
		})
	}

	ac.CmdMutex.Lock()
	if termErr != nil {
		ac.classic.clearIntent(gen)
		ac.RestartRequestedByUser = false
		ac.classic.setPhase(gen, ClassicFailed)
		debuglog.ErrorLog("RestartContext: could not confirm exit: %v", termErr)
		ac.RecordLifecycleError(LifecycleErrStopFailed, "restart",
			"the core could not be stopped for a restart", termErr.Error(), true)
		ac.CmdMutex.Unlock()
		return NewStartFailure(StartErrSpawnFailed, termErr)
	}

	ac.classic.clearOwnership(gen)
	ac.SingboxPrivilegedMode = false
	ac.SingboxPrivilegedPID = 0
	ac.SingboxPrivilegedSingboxPID = 0
	ac.SingboxPrivilegedPIDFile = ""
	// Label the transition BEFORE flipping the flag. This teardown is part of a
	// restart, and the core is expected back — so the state change must not be
	// readable as "the user's stop completed". Without the label, a stop that
	// superseded this restart was settled as SUCCESS right here, and the restart
	// then went on to spawn a new core (see the StartContext call at the end).
	ac.noteTeardown(events.TeardownRestart)
	ac.RunningState.Set(false)
	if pidFile != "" {
		_ = os.Remove(pidFile)
	}
	ac.CmdMutex.Unlock()

	if !ac.classic.isCurrent(gen) {
		debuglog.InfoLog("RestartContext: generation %d was superseded; not restarting", gen)
		return nil
	}
	runGhostTunCleanup(true)
	// AWAIT the replacement, unlike KillForRestart which launches it and returns.
	return svc.StartContext(ctx, true)
}

// CheckIfRunningAtStart checks if sing-box is already running at application start.
// Shows a warning dialog if a running instance is detected.
func (svc *ProcessService) CheckIfRunningAtStart() {
	svc.checkAndShowSingBoxRunningWarning("CheckIfSingBoxRunningAtStart")
}

// checkAndShowSingBoxRunningWarning checks if sing-box is running and shows warning dialog if found.
// Returns true if process was found and warning was shown, false otherwise.
func (svc *ProcessService) checkAndShowSingBoxRunningWarning(ctx string) bool {
	found, foundPID := svc.isSingBoxProcessRunning()
	if found {
		// Своё ядро из прошлой сессии — не «чужой sing-box» (SPEC 144).
		// Убивать его нельзя: это работающий VPN пользователя. Вместо
		// диалога с предложением снять процесс лаунчер его ПРИЗНАЁТ и
		// показывает как работающий, чтобы Stop/Restart работали штатно.
		if own := svc.ownCorePID(); own > 0 && own == foundPID {
			debuglog.InfoLog("%s: adopting this launcher's own core from a previous session (PID=%d)", ctx, own)
			svc.adoptRunningCore(own)
			return false
		}
		debuglog.DebugLog("%s: Found sing-box process already running (PID=%d). Showing warning dialog.", ctx, foundPID)
		if svc.ac.hasUI() {
			killIt := func() {
				if runtime.GOOS == "darwin" {
					// Снимаем только те PID, чью личность подтвердили по
					// executable path (SPEC 145): широкий `pkill -f` снял бы
					// и чужой sing-box, и ядро другого профиля пользователя.
					svc.killVerifiedCores()
				} else {
					processName := platform.GetProcessNameForCheck()
					var err error
					if runtime.GOOS == "windows" && foundPID > 0 && foundPID == findPrivilegedCopyInUserSession() {
						// SPEC 141 §8: копия ядра повышенного classic — только
						// по PID: sing-box-lxd.exe так же зовётся служба.
						err = platform.KillProcessByPID(foundPID)
					} else {
						err = platform.KillProcess(processName)
					}
					// SPEC 139 §6 п. 7: ядро повышенного экземпляра без прав не
					// снять — сообщение с перезапуском, RunningState не трогаем.
					if svc.ac.KillNeedsElevation(err) {
						svc.ac.ShowKillNeedsElevation()
						return
					}
				}
				// В daemon-режиме RunningState отражает ядро ДЕМОНА, а убили
				// мы осиротевший classic-процесс — состояние демона не
				// менялось, и Set(false) показал бы ложный «остановлен» до
				// следующего статус-кадра supervisor'а.
				if svc.ac.BackendMode() != BackendDaemon {
					svc.ac.RunningState.Set(false)
				}
			}
			// The confirmation is presented by the GUI; the action itself is
			// the same closure as before. Through ac.ui() so the headless
			// backend, which has no port, does not panic here.
			svc.ac.ui().ConfirmKillExistingCore(killIt)
		}
		return true
	}
	debuglog.DebugLog("%s: No sing-box process found", ctx)
	return false
}

// getTrackedPID safely gets the PID of the tracked sing-box process.
func (svc *ProcessService) getTrackedPID() int {
	svc.ac.CmdMutex.Lock()
	defer svc.ac.CmdMutex.Unlock()
	if svc.ac.SingboxPrivilegedMode && svc.ac.SingboxPrivilegedPID != 0 {
		return svc.ac.SingboxPrivilegedPID
	}
	if svc.ac.SingboxCmd != nil && svc.ac.SingboxCmd.Process != nil {
		return svc.ac.SingboxCmd.Process.Pid
	}
	return -1
}

// parseCSVLine parses a CSV line, handling quoted fields.
func parseCSVLine(line string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false

	for _, r := range line {
		switch r {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				parts = append(parts, current.String())
				current.Reset()
			} else {
				current.WriteRune(r)
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 || len(parts) > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// IsSingBoxProcessRunningOnSystem returns true if a sing-box process matching the launcher
// heuristic is running (any PID). Used when internal RunningState may be out of sync (e.g. failed privileged Stop).
func (svc *ProcessService) IsSingBoxProcessRunningOnSystem() (bool, int) {
	return svc.isSingBoxProcessRunning()
}

// isSingBoxProcessRunning checks if sing-box process is running on the system.
// Returns (isRunning, pid) tuple.
// pidFilePath — pid-файл привилегированного запуска (SPEC 137): лаунчер
// пишет его сам как пользователь, там же его читает (SPEC 144). Пусто на
// платформах без привилегированного classic-запуска.
func (svc *ProcessService) pidFilePath() string {
	if platform.PrivilegedPidFileName == "" || svc.ac == nil || svc.ac.FileService == nil {
		return ""
	}
	return filepath.Join(svc.ac.FileService.Layout.Data.Bin(), platform.PrivilegedPidFileName)
}

// writePIDFile сохраняет PID привилегированного запуска и НЕ глотает ошибку
// (SPEC 145).
//
// Файл — единственный способ опознать своё ядро после перезапуска лаунчера.
// Если запись не удалась, ядро всё равно уже работает и снимать его нельзя;
// но пользователь обязан об этом узнать, а сессия — сохранить возможность
// штатной остановки. Поэтому ошибка логируется как WARN, состояние
// помечается, и Stop продолжает работать по PID в памяти.
func (svc *ProcessService) writePIDFile(path string, scriptPID, corePID int) {
	if path == "" {
		return
	}
	content := fmt.Sprintf("%d\n%d", scriptPID, corePID)
	if err := os.WriteFile(path, []byte(content), platform.DefaultFileMode); err != nil {
		debuglog.WarnLog("startSingBox: cannot write the pid file %s: %v; "+
			"cross-session recovery of the privileged core will not be possible this time", path, err)
		svc.pidFileWriteFailed.Store(true)
		return
	}
	svc.pidFileWriteFailed.Store(false)
	debuglog.DebugLog("startSingBox: wrote the pid file %s", path)
}

// PIDFileWriteFailed — сообщает, что pid-файл последнего привилегированного
// старта записать не удалось. UI может показать предупреждение.
func (svc *ProcessService) PIDFileWriteFailed() bool {
	return svc.pidFileWriteFailed.Load()
}

// clearPIDFileIfStale удаляет pid-файл, только если ни один из записанных в
// нём PID больше не принадлежит нашему ядру (SPEC 145).
//
// Безусловное удаление опасно: файл мог быть уже перезаписан новым стартом,
// и снос стёр бы актуальную запись работающего ядра.
func (svc *ProcessService) clearPIDFileIfStale() {
	path := svc.pidFilePath()
	if path == "" {
		return
	}
	if svc.ownCorePID() > 0 {
		debuglog.DebugLog("clearPIDFileIfStale: %s still describes a live core; keeping it", path)
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		debuglog.WarnLog("clearPIDFileIfStale: remove %s: %v", path, err)
		return
	}
	debuglog.DebugLog("clearPIDFileIfStale: removed stale %s", path)
}

// coreIdentity — чем должно быть наше ядро: защищённая root-owned копия и
// ядро лаунчера. Сравниваются executable path, а не подстрока командной
// строки, поэтому чужой sing-box (другая сборка, другой пользователь, другой
// путь установки) никогда не сойдёт за наш (SPEC 145).
func (svc *ProcessService) coreIdentity() coreIdentity {
	id := coreIdentity{}
	if svc.ac == nil || svc.ac.FileService == nil {
		return id
	}
	id.LauncherCorePath = svc.ac.FileService.SingboxPath
	id.CopyPath = systemDaemonServiceLayout().CorePath
	return id
}

// ownCorePID — PID нашего ядра среди процессов системы, или -1.
//
// Подтверждается и PID (из нашего pid-файла), и executable path (наша копия
// или ядро лаунчера). Одного «PID жив» мало: номера переиспользуются, и
// старый pid-файл легко указывает на посторонний процесс.
func (svc *ProcessService) ownCorePID() int {
	if runtime.GOOS != "darwin" {
		return -1
	}
	procs, err := listProcessDetailsDarwin()
	return ownCorePID(svc.pidFilePath(), svc.coreIdentity(), procs, err)
}

// verifyPIDIsOurCore — подтверждает личность конкретного PID перед тем, как
// его снимать. Возвращает false, если путь процесса не совпал с нашей
// копией/ядром: убивать такой процесс нельзя, даже если PID взят из
// pid-файла или найден по argv.
func (svc *ProcessService) verifyPIDIsOurCore(pid int) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if pid <= 0 {
		return false
	}
	procs, err := listProcessDetailsDarwin()
	if err != nil {
		debuglog.WarnLog("verifyPIDIsOurCore: cannot list processes (%v); refusing to kill PID %d", err, pid)
		return false
	}
	path, ok := pidExecutablePath(pid, procs)
	if !ok {
		debuglog.WarnLog("verifyPIDIsOurCore: PID %d executable path unknown; refusing to kill it", pid)
		return false
	}
	if !svc.coreIdentity().matches(path) {
		debuglog.WarnLog("verifyPIDIsOurCore: PID %d is %q, not our core; refusing to kill it", pid, path)
		return false
	}
	return true
}

// KillVerifiedCores — публичная обёртка для аварийного снятия ядра
// (Diagnostics). Снимает только процессы с подтверждённой личностью
// (SPEC 145), поэтому чужой sing-box не затрагивается.
func (svc *ProcessService) KillVerifiedCores() { svc.killVerifiedCores() }

// killVerifiedCores снимает процессы нашего ядра из прошлой сессии,
// подтверждая личность каждого PID (SPEC 145).
//
// Берём объединение двух источников кандидатов: наш pid-файл и поиск по
// argv. Ни один кандидат не убивается без проверки executable path, поэтому
// чужой sing-box (другая сборка, другой пользователь, другой путь) остаётся
// нетронутым — раньше именно он и попадал под `pkill -f`.
func (svc *ProcessService) killVerifiedCores() {
	candidates := map[int]struct{}{}
	for _, pid := range readPrivilegedPidFile(svc.pidFilePath()) {
		candidates[pid] = struct{}{}
	}
	if pids, err := platform.FindPrivilegedCandidatePIDs(); err != nil {
		debuglog.WarnLog("killVerifiedCores: cannot enumerate candidates: %v", err)
	} else {
		for _, pid := range pids {
			candidates[pid] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		debuglog.DebugLog("killVerifiedCores: no candidate processes")
		return
	}
	killed := 0
	for pid := range candidates {
		if !svc.verifyPIDIsOurCore(pid) {
			continue // не наш — не трогаем
		}
		if err := platform.KillPrivilegedProcess(pid, 0, ""); err != nil {
			debuglog.WarnLog("killVerifiedCores: kill PID %d: %v", pid, err)
			continue
		}
		killed++
		debuglog.InfoLog("killVerifiedCores: stopped our core PID %d", pid)
	}
	// pid-файл описывал именно эти процессы: после снятия он неактуален.
	if killed > 0 {
		svc.clearPIDFileIfStale()
	}
	debuglog.InfoLog("killVerifiedCores: stopped %d of %d candidates", killed, len(candidates))
}

// adoptRunningCore признаёт живое ядро прошлой сессии своей работой
// (SPEC 144): без этого после перезапуска GUI лаунчер считал состояние
// «остановлен», хотя ядро с TUN работало, и Stop/Restart были неактуальны.
//
// Заполняются только те поля, которых не хватает: PID ядра и путь pid-файла
// (их использует Stop, чтобы снять процесс). Cmd не подделываем — процесса,
// порождённого этим процессом, не существует, и Wait по нему невозможен;
// поэтому сироту снимает privileged-путь по PID/шаблону.
// adoptRunningCore takes ownership of a core this launcher started in a PREVIOUS
// session and has just re-identified by PID and executable path.
//
// # WHY THE WATCHER MATTERS
//
// An adopted process is not our child: there is no exec.Cmd, so Wait() is
// unavailable and the Monitor goroutine that normally reports an exit does not
// exist. Recording the PID and calling it running is therefore only half the
// job — without a watcher, an adopted core that dies leaves the UI saying
// "running" forever, with Stop and Restart both operating on a process that is
// already gone.
//
// The watcher below is deliberately NOT a Wait: macOS cannot wait on a
// non-child, and pretending otherwise (a fake Cmd) would be a lie that breaks
// the moment it is used. It polls for the process's existence, verified by
// executable path so a recycled PID is never mistaken for ours, and only then
// reports the exit through the same crash-classification path a child would use.
func (svc *ProcessService) adoptRunningCore(pid int) {
	ac := svc.ac
	ac.CmdMutex.Lock()
	if ac.RunningState != nil && ac.RunningState.IsRunning() {
		ac.CmdMutex.Unlock()
		return
	}

	exe := svc.privilegedCorePath()
	if exe == "" {
		// Without a verified executable there is nothing to watch and nothing
		// safe to signal. Recording ownership anyway would let a later Stop
		// target whatever now holds that PID.
		debuglog.WarnLog("adoptRunningCore: refusing to adopt PID %d without a verified executable path", pid)
		ac.CmdMutex.Unlock()
		return
	}

	gen := ac.classic.adoptExisting(exe, pid, true)
	ac.SingboxPrivilegedMode = true
	ac.SingboxPrivilegedPID = pid
	ac.SingboxPrivilegedPIDFile = svc.pidFilePath()
	ac.RunningState.Set(true)
	ac.CmdMutex.Unlock()
	debuglog.InfoLog("adoptRunningCore: adopted PID %d as the running core (generation %d, pid file %s)",
		pid, gen, svc.pidFilePath())

	go svc.watchAdoptedCore(gen, ProcessIdentity{PID: pid, Executable: exe})
}

// watchAdoptedCore reports the exit of a process we adopted rather than spawned.
//
// Identity-checked on every poll: the loop must stop following a PID that has
// been recycled, or it would report the death of an unrelated process as our
// core's exit — and worse, hand that PID to the kill paths.
func (svc *ProcessService) watchAdoptedCore(gen uint64, id ProcessIdentity) {
	ac := svc.ac
	checker := platformChecker{}
	for {
		// A superseded generation stops watching immediately: after a mode
		// switch this loop must not report anything about a runtime it no longer
		// belongs to.
		if !ac.classic.isCurrent(gen) {
			debuglog.InfoLog("watchAdoptedCore: generation %d superseded; stopping the watcher for PID %d", gen, id.PID)
			return
		}
		alive, err := checker.alive(id.PID, id.Executable)
		if err != nil {
			// Cannot verify: keep watching rather than guess. Treating an
			// unreadable process table as "it died" would report a false exit for
			// a live VPN.
			debuglog.DebugLog("watchAdoptedCore: cannot check PID %d: %v", id.PID, err)
			time.Sleep(adoptedCorePollInterval)
			continue
		}
		if !alive {
			debuglog.InfoLog("watchAdoptedCore: adopted core PID %d has exited", id.PID)
			ac.CmdMutex.Lock()
			if !ac.classic.isCurrent(gen) {
				ac.CmdMutex.Unlock()
				return
			}
			ac.classic.clearOwnership(gen)
			ac.classic.setPhase(gen, ClassicStopped)
			ac.SingboxPrivilegedMode = false
			ac.SingboxPrivilegedPID = 0
			ac.SingboxPrivilegedPIDFile = ""
			ac.RunningState.Set(false)
			ac.CmdMutex.Unlock()
			// The core was not our child and its exit was not requested, so the
			// reason is classified from its log exactly as a crash would be. The
			// state is corrected to stopped either way — an adopted core that is
			// gone must never leave the UI claiming it runs.
			return
		}
		time.Sleep(adoptedCorePollInterval)
	}
}

// adoptedCorePollInterval is how often an adopted core's existence is checked.
//
// One second is chosen against its two costs: a process-table read per tick, and
// the latency before a dead adopted core stops showing as running. Faster buys
// nothing the user can perceive; slower delays the correction visibly.
const adoptedCorePollInterval = time.Second

// isSingBoxProcessRunning проверяет, запущен ли sing-box, который лаунчер
// должен учитывать.
func (svc *ProcessService) isSingBoxProcessRunning() (bool, int) {
	ourPID := svc.getTrackedPID()

	if runtime.GOOS == "darwin" {
		// Аргумент-чувствительная проверка (pgrep -f по тому же паттерну,
		// что и privileged pkill): установленный демон `sing-box lxd` — это
		// процесс с тем же именем бинаря, но НЕ «чужой запущенный sing-box».
		// Имя-ориентированный скан (go-ps) ловил бы демона и предлагал его
		// убить — что противоречит всему daemon-режиму.
		if found, pid, err := findSingboxRunProcessDarwin(); err == nil {
			if !found {
				return false, -1
			}
			// Ядро, запущенное ЭТИМ лаунчером в прошлой сессии, — не чужой
			// процесс (SPEC 144). После перезапуска GUI в памяти нет ни Cmd,
			// ни privileged-PID, и раньше живое ядро с TUN принималось за
			// чужое: пользователю предлагали его убить, то есть снести
			// работающий VPN при обычном открытии клиента.
			//
			// Личность подтверждается по executable path (SPEC 145), а не по
			// одному «PID жив»: номера переиспользуются.
			if own := svc.ownCorePID(); own > 0 {
				debuglog.DebugLog("isSingBoxProcessRunning: PID %d is this launcher's own core from a previous session", own)
				return true, own
			}
			return true, pid
		}
		// pgrep недоступен/сломан. В daemon-режиме имя-ориентированный
		// fallback поймал бы демона `sing-box lxd` и предложил его убить —
		// тогда честнее пропустить проверку, чем стрелять по демону.
		if svc.ac.BackendMode() == BackendDaemon {
			return false, -1
		}
		// classic: fallback на старый скан по имени.
		return svc.isSingBoxProcessRunningWithPS(ourPID)
	}

	if runtime.GOOS == "windows" {
		// Use tasklist on Windows for better reliability
		processName := platform.GetProcessNameForCheck()
		cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("IMAGENAME eq %s", processName), "/FO", "CSV", "/NH")
		platform.PrepareCommand(cmd)
		output, err := cmd.Output()
		if err != nil {
			debuglog.WarnLog("isSingBoxProcessRunning: tasklist failed: %v", err)
			// Fallback to ps library
			return svc.isSingBoxProcessRunningWithPS(ourPID)
		}

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := parseCSVLine(line)
			if len(parts) >= 2 {
				name := strings.Trim(parts[0], "\"")
				pidStr := strings.Trim(parts[1], "\"")
				// CRITICAL: Check that the process name matches sing-box.exe
				if strings.EqualFold(name, processName) {
					if pid, err := strconv.Atoi(pidStr); err == nil {
						isOurProcess := (ourPID != -1 && pid == ourPID)
						debuglog.DebugLog("isSingBoxProcessRunning: Found process: PID=%d, name='%s' (our tracked PID=%d, isOurProcess=%v)",
							pid, name, ourPID, isOurProcess)
						return true, pid
					} else {
						debuglog.DebugLog("isSingBoxProcessRunning: Failed to parse PID '%s': %v", pidStr, err)
					}
				}
			}
		}
		debuglog.DebugLog("isSingBoxProcessRunning: tasklist found processes but none matched '%s'", processName)
		// SPEC 141 §8: копия ядра повышенного classic в сессии пользователя;
		// служба (сессия 0) к завершению не предлагается.
		if pid := findPrivilegedCopyInUserSession(); pid > 0 {
			debuglog.DebugLog("isSingBoxProcessRunning: found the protected core copy in a user session: PID=%d", pid)
			return true, pid
		}
		return false, -1
	}

	// For other OS use ps library
	return svc.isSingBoxProcessRunningWithPS(ourPID)
}

// isSingBoxProcessRunningWithPS uses ps library to check for running process
func (svc *ProcessService) isSingBoxProcessRunningWithPS(ourPID int) (bool, int) {
	processes, err := process.GetProcesses()
	if err != nil {
		debuglog.WarnLog("isSingBoxProcessRunningWithPS: error listing processes: %v", err)
		return false, -1
	}
	processName := platform.GetProcessNameForCheck()

	for _, p := range processes {
		execName := p.Name
		if strings.EqualFold(execName, processName) {
			foundPID := p.PID
			isOurProcess := (ourPID != -1 && foundPID == ourPID)
			debuglog.DebugLog("isSingBoxProcessRunningWithPS: Found process: PID=%d, name='%s' (our tracked PID=%d, isOurProcess=%v)", foundPID, execName, ourPID, isOurProcess)
			return true, foundPID
		}
	}
	debuglog.DebugLog("isSingBoxProcessRunningWithPS: No sing-box process found (checked %d processes)", len(processes))
	return false, -1
}

// recordStartFailure records a start failure in the unified lifecycle store so
// the headless backend and the SwiftUI client see it, not just the log.
//
// The Fyne dialog path is kept for the Fyne UI, but it is no longer the only
// consumer: that arrangement left the macOS app structurally unable to learn
// that a start had failed.
func (svc *ProcessService) recordStartFailure(operation string, err error) {
	if svc == nil || svc.ac == nil || err == nil {
		return
	}
	code, recoverable := ClassifyLifecycleError(err)
	svc.ac.RecordLifecycleError(code, operation, "the core failed to start", err.Error(), recoverable)
}

// killSupersededStart stops a core this (now abandoned) generation just spawned.
//
// This is the second half of the orphan fix. A superseded generation must not
// leave a process behind: it would hold the TUN, be invisible in the UI, and be
// impossible for the user to stop. Killing it is safe by construction — the
// process was created microseconds ago by this function, and the identity is
// taken from the cmd handle we still own, so no PID reuse is possible.
func (svc *ProcessService) killSupersededStart(cmd *exec.Cmd, exe string, pid int) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Best-effort but *reported*: if this fails the process outlives its
	// owner, which is exactly the condition worth telling the user about.
	if err := cmd.Process.Kill(); err != nil {
		debuglog.ErrorLog("startSingBox: could not stop the superseded core (PID=%d): %v", pid, err)
		if svc.ac != nil {
			svc.ac.RecordLifecycleError(LifecycleErrStopFailed, "start",
				"a superseded start left a process behind",
				fmt.Sprintf("PID %d could not be stopped: %v", pid, err), false)
		}
		return
	}
	// Reap it so the handle does not linger, and confirm it is actually gone
	// rather than assuming the kill call was the end of it.
	_ = cmd.Wait()
	checker := platformChecker{}
	if alive, err := checker.alive(pid, exe); err == nil && alive {
		debuglog.ErrorLog("startSingBox: superseded core (PID=%d) survived the kill", pid)
		if svc.ac != nil {
			svc.ac.RecordLifecycleError(LifecycleErrStopFailed, "start",
				"a superseded start left a process behind",
				fmt.Sprintf("PID %d survived a forced kill", pid), false)
		}
		return
	}
	debuglog.InfoLog("startSingBox: superseded core (PID=%d) stopped", pid)
}

// superviseLatePrivilegedStart drains a privileged start whose result the UI
// stopped waiting for.
//
// # WHY THIS EXISTS
//
// Authorization can take arbitrarily long — the user may leave the password
// dialog on screen for minutes. The UI must not wait forever, so the start path
// times out. But timing out the WAIT is not the same as abandoning the
// OPERATION: the worker keeps running, and if the user finally authenticates it
// starts a ROOT sing-box. Previously that result was written into a buffered
// channel that nobody read again, producing a root core that held the TUN while
// the launcher reported "stopped" and offered to start another one.
//
// This supervisor is what makes the late result safe. It waits for the worker
// however long it takes and then does exactly one of two things:
//
//   - the generation is still current: adopt the process (recording ownership
//     and starting the exit watcher), so it appears as running and can be
//     stopped;
//   - the generation was superseded (mode switch, shutdown, a newer start):
//     stop the process immediately and confirm it is gone.
//
// Either way no process is left unowned.
func (svc *ProcessService) superviseLatePrivilegedStart(gen uint64, corePath string, pidCh <-chan privilegedStartResult) {
	if svc == nil || svc.ac == nil {
		// Nothing can own the result; still must not leak a root process.
		if res, ok := <-pidCh; ok && res.Err == nil && res.Script > 0 {
			svc.stopSupersededPrivileged(res.Script, res.Singbox, svc.pidFilePath(), corePath)
		}
		return
	}
	res, ok := <-pidCh
	if !ok {
		return
	}
	// The worker failed or the user cancelled: nothing was started, so there is
	// nothing to adopt or kill. Recording the reason is still worthwhile — this
	// is precisely the case that used to vanish into the log.
	if res.Err != nil {
		debuglog.WarnLog("startSingBox: late privileged result for generation %d failed: %v", gen, res.Err)
		svc.recordStartFailure("start", NewStartFailure(StartErrSpawnFailed, res.Err))
		return
	}
	if res.Script <= 0 {
		debuglog.WarnLog("startSingBox: late privileged result for generation %d carried no PID", gen)
		return
	}

	if !svc.ac.classic.isCurrent(gen) {
		debuglog.WarnLog("startSingBox: late privileged start for generation %d arrived after it was superseded; stopping the root core (PIDs %d/%d)",
			gen, res.Script, res.Singbox)
		svc.stopSupersededPrivileged(res.Script, res.Singbox, svc.pidFilePath(), corePath)
		return
	}

	// Still current: adopt it properly, which means recording ownership (so Stop
	// can find it) and starting the exit watcher (so an unexpected death is
	// noticed rather than leaving the UI showing "running" forever).
	debuglog.InfoLog("startSingBox: adopting the late privileged start (PIDs %d/%d, generation %d)", res.Script, res.Singbox, gen)
	svc.ac.CmdMutex.Lock()
	defer svc.ac.CmdMutex.Unlock()
	if svc.ac.RunningState.IsRunning() {
		// A newer successful start already owns the runtime; this late result is
		// a duplicate and must not overwrite it.
		debuglog.WarnLog("startSingBox: a core is already running; stopping the late privileged start (PIDs %d/%d)", res.Script, res.Singbox)
		svc.stopSupersededPrivileged(res.Script, res.Singbox, svc.pidFilePath(), corePath)
		return
	}
	pidFilePath := svc.pidFilePath()
	if !svc.commitPrivilegedStartLocked(gen, res.Script, res.Singbox, pidFilePath, corePath) {
		svc.stopSupersededPrivileged(res.Script, res.Singbox, pidFilePath, corePath)
		return
	}
	svc.writePIDFile(pidFilePath, res.Script, res.Singbox)
	svc.ac.classic.setPhase(gen, ClassicRunning)
	go func(scriptPID int, g uint64) {
		svc.deps().waitExit(scriptPID)
		if !svc.ac.classic.isCurrent(g) {
			return
		}
		svc.onPrivilegedScriptExited(g)
	}(res.Script, gen)
	svc.ac.EmitCoreStateChange()
}

// stopSupersededPrivileged terminates a root core that no generation owns.
//
// Used for the two orphan-producing cases: a start superseded at its commit
// point, and a late result arriving after abandonment. Both have the same
// requirement — the process exists and must not, so it is stopped and the exit
// is CONFIRMED rather than assumed.
func (svc *ProcessService) stopSupersededPrivileged(scriptPID, singboxPID int, pidFile, corePath string) {
	if scriptPID <= 0 && singboxPID <= 0 {
		return
	}
	checker := platformChecker{}
	outcome, err := terminatePrivilegedOwned(scriptPID, singboxPID, pidFile, corePath,
		"superseded start", checker)
	if err != nil {
		debuglog.ErrorLog("startSingBox: superseded privileged core (PIDs %d/%d) could not be stopped: %v",
			scriptPID, singboxPID, err)
		if svc.ac != nil {
			svc.ac.RecordLifecycleError(LifecycleErrStopFailed, "start",
				"a superseded start left a root process running",
				fmt.Sprintf("PIDs %d/%d: %v", scriptPID, singboxPID, err), false)
		}
		return
	}
	if pidFile != "" {
		_ = os.Remove(pidFile)
	}
	debuglog.InfoLog("startSingBox: superseded privileged core stopped (PIDs %d/%d, graceful=%v forced=%v, %s)",
		scriptPID, singboxPID, outcome.Graceful, outcome.Forced, outcome.Elapsed)
}
