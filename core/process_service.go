package core

import (
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
func (ac *AppController) classifyCoreExitReason() exitReason {
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
	reason := classifyExitText(lastLines(text, 40))
	debuglog.InfoLog("classifyCoreExitReason: %s (log %s)", reason, path)
	return reason
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
func (ac *AppController) showDeterministicExitDialog(reason exitReason) {
	if ac.uiPort == nil {
		return
	}
	body := locale.T(deterministicExitText(reason))
	if body == "" {
		return
	}
	ac.ui().ShowError(locale.T("Error"), body)
}

// NewProcessService constructs a ProcessService bound to the controller.
func NewProcessService(ac *AppController) *ProcessService {
	return &ProcessService{ac: ac}
}

// Start launches the sing-box process. Behavior is identical to the previous StartSingBoxProcess.
// skipRunningCheck: если true, пропускает проверку на уже запущенный процесс (для автоперезапуска).
func (svc *ProcessService) Start(skipRunningCheck ...bool) {
	ac := svc.ac
	if ac.RunningState.IsRunning() {
		if ac.uiPort != nil {
			ac.uiPort.ShowInfo(locale.TN(1, "Info"), locale.T("Sing-Box already running (according to internal state)."))
		}
		return
	}

	// Проверяем, не запущен ли уже процесс на уровне ОС (пропускаем при автоперезапуске)
	skipCheck := len(skipRunningCheck) > 0 && skipRunningCheck[0]
	if !skipCheck {
		if svc.checkAndShowSingBoxRunningWarning("startSingBox") {
			return
		}
	}

	// Шаблон после апгрейда докачивается в фоне (StartTemplateRefresh) —
	// ждём его ДО захвата CmdMutex: Stop, нажатый во время ожидания, иначе
	// встал бы на мьютексе. Сборка ниже ждёт того же шлюза, но он уже открыт.
	ac.awaitTemplateRefresh()

	ac.CmdMutex.Lock()
	defer ac.CmdMutex.Unlock()

	// Re-check under the lock: two concurrent Start() calls both pass the
	// unlocked IsRunning() check above; without this the second one would
	// launch a duplicate sing-box process and orphan the first Cmd handle.
	if ac.RunningState.IsRunning() {
		debuglog.WarnLog("startSingBox: already running (lost start race), skipping duplicate start")
		return
	}

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
		ac.ShowRebuildError(err)
		return
	}

	// SPEC 139 §4: на Windows TUN без прав администратора не стартует —
	// вместо ядра диалог (перезапуск с правами или режим прокси). Сюда
	// приходят все входы: кнопка, трей, -start, Debug API, авто-рестарт.
	if ac.tunNeedsElevation() {
		ac.showTunElevationDialog()
		if ac.uiPort != nil {
			ac.uiPort.ReportCoreStartAborted("")
		}
		return
	}

	// Check capabilities on Linux before starting
	if suggestion := platform.CheckAndSuggestCapabilities(ac.FileService.SingboxPath); suggestion != "" {
		debuglog.WarnLog("startSingBox: Capabilities check failed: %s", suggestion)
		if ac.uiPort != nil {
			cmd := platform.GetSetCapCommand(ac.FileService.SingboxPath)
			ac.uiPort.ShowCommandNeedsTerminal(locale.T("Linux capabilities required"), locale.T("Linux capabilities required")+"\n\n"+suggestion, cmd)
		}
		return
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
			debuglog.WarnLog("startSingBox: Could not check TUN in config: %v; assuming no TUN (no password).", err)
			hasTun = false
		}
		if hasTun {
			if err := svc.startSingBoxPrivileged(); err != nil {
				// Отказ гейта копии уже показан своим диалогом с командой.
				if !errors.Is(err, errPrivilegedCopyNotReady) {
					ac.ShowStartupError(err)
				}
				return
			}
			return
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
			// Отказ гейта копии уже показан своим диалогом с командой.
			if !errors.Is(err, errPrivilegedCopyNotReady) {
				ac.ShowStartupError(err)
			}
			return
		}
		corePath, privilegedLog = path, logFile
	}

	debuglog.WarnLog("startSingBox: Starting Sing-Box...")
	ac.SingboxCmd = exec.Command(corePath, "run", "-c", filepath.Base(ac.FileService.ConfigPath))
	platform.PrepareCommand(ac.SingboxCmd)
	ac.SingboxCmd.Dir = ac.FileService.Layout.Data.Bin()
	if privilegedLog != nil {
		ac.SingboxCmd.Stdout = privilegedLog
		ac.SingboxCmd.Stderr = privilegedLog
	} else if ac.FileService.ChildLogFile != nil {
		// Check and rotate log file before starting new process to prevent unbounded growth
		ac.FileService.CheckAndRotateLogFile(ac.FileService.ChildLogPath)

		// Write directly to file - no buffering in memory
		// This prevents memory leaks from accumulating log output
		// Logs are written immediately to disk, not stored in memory
		ac.SingboxCmd.Stdout = ac.FileService.ChildLogFile
		ac.SingboxCmd.Stderr = ac.FileService.ChildLogFile
	} else {
		debuglog.WarnLog("startSingBox: Warning: sing-box log file not available, output will not be logged.")
	}
	startErr := ac.SingboxCmd.Start()
	if privilegedLog != nil {
		// У ядра свой дескриптор classic.log; наш больше не нужен.
		_ = privilegedLog.Close()
	}
	if err := startErr; err != nil {
		ac.ShowStartupError(fmt.Errorf("failed to start Sing-Box process: %w", err))
		debuglog.ErrorLog("startSingBox: Failed to start Sing-Box: %v", err)
		return
	}
	if privilegedLog != nil {
		svc.coreLog.Store(coreLogPrivileged)
	} else {
		svc.coreLog.Store(coreLogUser)
	}
	ac.RunningState.Set(true)
	ac.StoppedByUser = false
	ac.StateService.ResetAutoUpdateFailedAttempts() // Reset so auto-update can retry after successful Start
	// Add log with PID
	debuglog.DebugLog("startSingBox: Sing-Box started. PID=%d", ac.SingboxCmd.Process.Pid)

	// Start auto-loading proxies after sing-box is running
	go func() {
		// Small delay to ensure API is ready
		<-time.After(2 * time.Second)
		ac.AutoLoadProxies()
	}()

	go svc.Monitor(ac.SingboxCmd)
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
func (svc *ProcessService) commitPrivilegedStartLocked(scriptPID, singboxPID int, pidFilePath string) {
	ac := svc.ac
	svc.coreLog.Store(coreLogPrivileged)
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
func (svc *ProcessService) startSingBoxPrivileged() error {
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
	select {
	case pids = <-pidCh:
	case <-time.After(privilegedStartTimeout):
		// Авторизация может висеть сколько угодно долго, но не бесконечно:
		// иначе GUI остаётся с «идёт запуск» навсегда. Таймаут не снимает
		// процесс (его мог запустить пользователь позже) — он только
		// возвращает управление, а PID подхватит следующий Start/adoption.
		return fmt.Errorf("privileged start did not return within %s (authorization still pending?)", privilegedStartTimeout)
	}

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
	svc.commitPrivilegedStartLocked(pids.Script, pids.Singbox, pidFilePath)
	// pid-файл пишем как пользователь; ошибка не глотается (SPEC 145).
	svc.writePIDFile(pidFilePath, pids.Script, pids.Singbox)
	debuglog.DebugLog("startSingBox: Sing-Box started with privileges (script PID=%d, sing-box PID=%d).", pids.Script, pids.Singbox)

	// Долгое ожидание выхода root-процесса — в отдельной горутине, вне пути
	// старта: она не держит CmdMutex и не задерживает возврат Start().
	go func(scriptPID int) {
		deps.waitExit(scriptPID)
		svc.onPrivilegedScriptExited()
	}(pids.Script)

	go func() {
		<-time.After(2 * time.Second)
		ac.AutoLoadProxies()
	}()
	return nil
}

// onPrivilegedScriptExited is called when the privileged script process exits (Wait4 returned).
// The script waits on sing-box, so when the script exits, sing-box has exited too.
func (svc *ProcessService) onPrivilegedScriptExited() {
	ac := svc.ac
	ac.CmdMutex.Lock()
	defer ac.CmdMutex.Unlock()
	if !ac.SingboxPrivilegedMode {
		return
	}
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
	reason := ac.classifyCoreExitReason()
	action, newAttempts := decideCrashActionReason(ac.StoppedByUser, ac.RestartRequestedByUser, false, ac.ConsecutiveCrashAttempts, restartAttempts, reason)
	ac.ConsecutiveCrashAttempts = newAttempts
	switch action {
	case actionStoppedByUser:
		ac.StoppedByUser = false
		debuglog.InfoLog("onPrivilegedScriptExited: Stopped by user.")
		return
	case actionUserRestart:
		ac.RestartRequestedByUser = false
		debuglog.InfoLog("onPrivilegedScriptExited: Restart requested by user, starting sing-box...")
		ac.CmdMutex.Unlock()
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
		return
	case actionMaxAttempts:
		debuglog.DebugLog("onPrivilegedScriptExited: Max restart attempts reached.")
		if ac.uiPort != nil {
			ac.uiPort.ShowError(locale.T("Error"), locale.Tf("Sing-Box failed to restart after %d attempts. Check sing-box.log for details.", restartAttempts))
		}
		return
	}
	// action == actionCrashRestart
	debuglog.WarnLog("onPrivilegedScriptExited: Sing-Box exited, auto-restart (attempt %d/%d)", ac.ConsecutiveCrashAttempts, restartAttempts)
	if ac.uiPort != nil {
		ac.ui().ShowInfo(locale.T("Crash"), locale.Tf("Sing-Box crashed, restarting... (attempt %d/%d)", ac.ConsecutiveCrashAttempts, restartAttempts))
	}
	ac.CmdMutex.Unlock()
	<-time.After(2 * time.Second)
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
func (svc *ProcessService) Monitor(cmdToMonitor *exec.Cmd) {
	ac := svc.ac
	// Store the PID we're monitoring to avoid conflicts with restarted processes
	monitoredPID := cmdToMonitor.Process.Pid

	// Wait for process completion - no timeout for long-running processes
	// The process should run until it exits or is stopped by user
	err := cmdToMonitor.Wait()

	ac.CmdMutex.Lock()
	defer ac.CmdMutex.Unlock()

	// GOLDEN STANDARD: Check order to prevent all race conditions
	// 1. First PID (is this my process?)
	if ac.SingboxCmd == nil || ac.SingboxCmd.Process == nil || ac.SingboxCmd.Process.Pid != monitoredPID {
		debuglog.DebugLog("monitorSingBox: Process was restarted (PID changed from %d). This monitor is obsolete. Exiting.", monitoredPID)
		return
	}

	// SPEC 070: shared crash/restart decision (steps 2-5). PID-check (step 1)
	// stays above; err==nil graceful-exit is fed via cleanExit so the decision
	// lives in one place but Monitor keeps its per-branch RunningState placement.
	//
	// SPEC 143: причина берётся из лога ядра. Детерминированная ошибка
	// (конфиг, права, порт) прекращает авто-перезапуск сразу, а не после
	// трёх одинаковых попыток.
	reason := ac.classifyCoreExitReason()
	action, newAttempts := decideCrashActionReason(ac.StoppedByUser, ac.RestartRequestedByUser, err == nil, ac.ConsecutiveCrashAttempts, restartAttempts, reason)

	// 2. Then StoppedByUser (did user stop it?)
	if action == actionStoppedByUser {
		debuglog.InfoLog("monitorSingBox: Sing-Box exited as requested by user.")
		ac.ConsecutiveCrashAttempts = newAttempts
		ac.RunningState.Set(false)
		ac.StoppedByUser = false // Reset flag for next start
		// SPEC 065: cleanup phantom TUN-адаптеров на Win7 после exit sing-box.
		triggerGhostTunCleanup()
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
		return
	}

	// 5. Crash → restart with delay and attempt limit
	ac.RunningState.Set(false)
	ac.ConsecutiveCrashAttempts = newAttempts

	if action == actionMaxAttempts {
		debuglog.DebugLog("monitorSingBox: Maximum restart attempts (%d) reached. Stopping auto-restart.", restartAttempts)
		if ac.uiPort != nil {
			ac.uiPort.ShowError(locale.T("Error"), locale.Tf("Sing-Box failed to restart after %d attempts. Check sing-box.log for details.", restartAttempts))
		}
		return
	}

	// action == actionCrashRestart
	debuglog.WarnLog("monitorSingBox: Sing-Box crashed: %v, attempting auto-restart (attempt %d/%d)", err, ac.ConsecutiveCrashAttempts, restartAttempts)
	if ac.uiPort != nil {
		ac.ui().ShowInfo(locale.T("Crash"), locale.Tf("Sing-Box crashed, restarting... (attempt %d/%d)", ac.ConsecutiveCrashAttempts, restartAttempts))
	}

	ac.CmdMutex.Unlock()
	<-time.After(2 * time.Second)
	// SPEC 065 hotfix (v0.9.9.1): cleanup phantom singbox-tun adapter from
	// the just-crashed sing-box BEFORE auto-restart. Без этого хука каждый
	// retry создаёт новый адаптер (singbox-tun0 → tun1 → tun2) потому что
	// старое имя ещё занято dead-but-not-cleaned-up phantom'ом. На 3
	// попытки = 3 phantom-адаптера, даже если юзер вообще не нажимал Stop.
	// Sync mode: cleanup точно завершится ДО Start.
	runGhostTunCleanup(true)
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

// Stop attempts graceful shutdown, mirroring previous StopSingBoxProcess.
func (svc *ProcessService) Stop() {
	if svc.stopOverrideForTest != nil {
		svc.stopOverrideForTest()
		return
	}
	ac := svc.ac
	ac.CmdMutex.Lock()

	// CRITICAL: Set flag BEFORE sending signal
	// This ensures the monitor sees the flag even if the process exits very quickly
	ac.StoppedByUser = true
	ac.ConsecutiveCrashAttempts = 0

	if !ac.RunningState.IsRunning() {
		ac.StoppedByUser = false
		ac.CmdMutex.Unlock()
		return
	}

	if ac.SingboxPrivilegedMode && ac.SingboxPrivilegedPID != 0 && ac.SingboxPrivilegedPIDFile != "" {
		pidFile := ac.SingboxPrivilegedPIDFile
		scriptPID := ac.SingboxPrivilegedPID
		singboxPID := ac.SingboxPrivilegedSingboxPID
		ac.CmdMutex.Unlock()
		debuglog.InfoLog("stopSingBox: Killing privileged Sing-Box (script PID %d, sing-box PID %d)...", scriptPID, singboxPID)
		if err := platform.KillPrivilegedProcess(scriptPID, singboxPID, pidFile); err != nil {
			debuglog.WarnLog("stopSingBox: Privileged kill failed: %v", err)
			ac.CmdMutex.Lock()
			ac.StoppedByUser = false
			ac.CmdMutex.Unlock()
			// На пути выхода диалог бессмысленен: GracefulExit идёт на
			// main-потоке Fyne, окно закрывается — ошибка остаётся в логе.
			if ac.hasUI() && !ac.IsExiting() {
				ac.uiPort.ShowError(locale.T("Error"), locale.T(stopPrivilegedFailedText)+": "+err.Error())
			}
			return
		}
		ac.CmdMutex.Lock()
		ac.SingboxPrivilegedMode = false
		ac.SingboxPrivilegedPID = 0
		ac.SingboxPrivilegedSingboxPID = 0
		ac.SingboxPrivilegedPIDFile = ""
		ac.RunningState.Set(false)
		ac.StoppedByUser = false
		ac.CmdMutex.Unlock()
		// SPEC 065: privileged path не идёт через Monitor (нет cmd.Wait),
		// поэтому хук дублируется здесь. KillPrivilegedProcess вернулся
		// успешно ⇒ sing-box процесс подтверждённо завершён.
		triggerGhostTunCleanup()
		return
	}

	if ac.SingboxCmd == nil || ac.SingboxCmd.Process == nil {
		debuglog.InfoLog("StopSingBoxProcess: Inconsistent state detected. Correcting state.")
		ac.RunningState.Set(false)
		ac.StoppedByUser = false
		ac.CmdMutex.Unlock()
		return
	}

	debuglog.InfoLog("stopSingBox: Attempting graceful shutdown...")
	processToStop := ac.SingboxCmd.Process

	// Разблокируем мьютекс перед отправкой сигнала, чтобы не блокировать
	ac.CmdMutex.Unlock()

	var err error
	if runtime.GOOS == "windows" {
		debuglog.InfoLog("stopSingBox: Stopping Sing-Box PID %d...", processToStop.Pid)
		err = platform.KillProcessByPID(processToStop.Pid)
	} else {
		err = processToStop.Signal(os.Interrupt)
	}

	if err != nil {
		debuglog.WarnLog("stopSingBox: Graceful signal failed: %v. Forcing kill.", err)
		if killErr := processToStop.Kill(); killErr != nil {
			debuglog.ErrorLog("stopSingBox: Failed to kill Sing-Box process: %v", killErr)
		}
	} else {
		// Start watchdog timer that will kill the process if it doesn't close itself
		go func(pid int) {
			<-time.After(gracefulShutdownTimeout)
			pInfo, found, err := process.FindProcess(pid)
			if err == nil && found {
				_ = pInfo // pInfo is the process info; we only need to know it exists
				debuglog.DebugLog("stopSingBox watchdog: Process %d still running after timeout. Forcing kill.", pid)
				// Reliably kill the process and its child processes
				_ = platform.KillProcessByPID(pid)
			} else if err != nil {
				debuglog.DebugLog("stopSingBox watchdog: error checking process %d: %v", pid, err)
			}
		}(processToStop.Pid)
	}
}

// KillForRestart kills the sing-box process and asks the watcher to restart it (RestartRequestedByUser).
func (svc *ProcessService) KillForRestart() {
	ac := svc.ac
	ac.CmdMutex.Lock()

	if !ac.RunningState.IsRunning() {
		ac.CmdMutex.Unlock()
		return
	}

	ac.RestartRequestedByUser = true // watcher will see exit and call Start(true)

	if ac.SingboxPrivilegedMode && ac.SingboxPrivilegedPID != 0 && ac.SingboxPrivilegedPIDFile != "" {
		pidFile := ac.SingboxPrivilegedPIDFile
		scriptPID := ac.SingboxPrivilegedPID
		singboxPID := ac.SingboxPrivilegedSingboxPID
		ac.CmdMutex.Unlock()
		debuglog.InfoLog("KillForRestart: Killing privileged Sing-Box (script PID %d, sing-box PID %d)...", scriptPID, singboxPID)
		if err := platform.KillPrivilegedProcess(scriptPID, singboxPID, pidFile); err != nil {
			debuglog.WarnLog("KillForRestart: Privileged kill failed: %v", err)
		}
		return
	}

	if ac.SingboxCmd == nil || ac.SingboxCmd.Process == nil {
		ac.CmdMutex.Unlock()
		return
	}

	processToStop := ac.SingboxCmd.Process
	ac.CmdMutex.Unlock()

	debuglog.InfoLog("KillForRestart: Sending signal to PID %d...", processToStop.Pid)
	if runtime.GOOS == "windows" {
		_ = platform.KillProcessByPID(processToStop.Pid)
	} else {
		if err := processToStop.Signal(os.Interrupt); err != nil {
			_ = processToStop.Kill()
		}
	}
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
func (svc *ProcessService) adoptRunningCore(pid int) {
	svc.ac.CmdMutex.Lock()
	defer svc.ac.CmdMutex.Unlock()
	if svc.ac.RunningState.IsRunning() {
		return
	}
	svc.ac.SingboxPrivilegedMode = true
	svc.ac.SingboxPrivilegedPID = pid
	svc.ac.SingboxPrivilegedPIDFile = svc.pidFilePath()
	svc.ac.RunningState.Set(true)
	debuglog.InfoLog("adoptRunningCore: adopted PID %d as the running core (pid file %s)", pid, svc.ac.SingboxPrivilegedPIDFile)
}

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
