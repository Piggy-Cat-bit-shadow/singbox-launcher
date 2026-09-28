package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"singbox-launcher/internal/debuglog"

	"singbox-launcher/api"
	"singbox-launcher/core/config"
	"singbox-launcher/core/config/subscription"
	"singbox-launcher/core/events"
	"singbox-launcher/core/services"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/paths"
	"singbox-launcher/internal/platform"
	"singbox-launcher/internal/process"
	"singbox-launcher/internal/uiport"
)

// Длинные тексты локализации: ключ = английский текст (SPEC 111).
const (
	configNotFoundMessageText = "⚠️ Configuration file not found!\n\nThe file %s is missing from the bin/ folder.\n\nTo get started:\n1. open ⚙️ Configurator\n2. add subscription URLs in the Sources tab and click Save\n3. click 🔄 Update on this dashboard to fetch and build config\n4. press Start\n"
)

// AppController - the main structure encapsulating all application state and logic.
// AppController is the central controller coordinating all application components.
// It manages UI state, process lifecycle, configuration, API interactions, and logging.
// The controller delegates specific responsibilities to specialized services:
// - ProcessService: sing-box process management
// - ConfigService: configuration parsing and updates
// The controller maintains application-wide state and provides callbacks for UI updates.
type AppController struct {
	// --- Services ---
	// uiPort is the GUI boundary (see uiport.go). It is nil in the headless
	// backend, which is why core never imports a GUI toolkit.
	uiPort uiport.Port
	// ownershipPolicy answers "may a rebuild replace config.json?" for the
	// pre-start hook. Installed by whichever backend owns config provenance
	// (backend/service), so the marker is parsed in exactly one place and this
	// package never learns the marker's format.
	//
	// nil means "no opinion", which is treated as NOT rebuildable: a caller that
	// forgot to install the policy gets the safe answer, not a silent overwrite.
	ownershipMu     sync.RWMutex
	ownershipPolicy func() bool
	// APIService manages Clash API interactions and proxy list management
	APIService *services.APIService
	// StateService manages application state including version caches and auto-update state
	StateService *services.StateService
	// FileService manages file paths and log file handles
	FileService *services.FileService
	// ProcessService manages sing-box process lifecycle (start, stop, monitor, auto-restart)
	ProcessService *ProcessService
	// ConfigService handles configuration parsing, subscription fetching, and JSON generation
	ConfigService *ConfigService
	// EventBus — типизированный sync event-bus (SPEC 047). Передаётся в
	// сервисы при их создании, чтобы UI-слой мог точечно подписываться
	// на изменения состояния вместо broadcast-callback'ов.
	EventBus events.Bus

	// backend — активный движок ядра (classic spawn / lxd daemon). Все
	// package-level обёртки (StartSingBoxProcess и т.д.) диспетчеризуют
	// сюда. Меняется только через setBackend (Settings → режим ядра).
	backend   CoreBackend
	backendMu sync.RWMutex
	// switchMu serializes WHOLE engine handovers (validate → construct →
	// quiesce → publish → persist). backendMu alone is not enough: setBackend
	// must release it while closing the previous backend, so two concurrent
	// switches could interleave and publish in an order unrelated to the order
	// they started in. The UI serializes its own calls, but the IPC path is not
	// a UI. Guarded after backendMu in the lock order (see buildMu note).
	switchMu sync.Mutex
	// backendModeChangeHook — колбэк после смены backend (traffic-источник).
	backendModeChangeHook func()

	// Колбэки страховки «ядро отвергло узел» (SPEC 132 §6.2/§6.3). Ставит их
	// UI на сборке приложения (SetCoreRejectDecider/SetCoreRejectProgress),
	// читает фоновая горутина сборки — отсюда мьютекс. nil у обоих =
	// поведение фонового входа: цикл идёт молча до жёсткого потолка.
	coreRejectHooksMu      sync.Mutex
	coreRejectDecideHook   coreRejectDecider
	coreRejectProgressHook coreRejectProgress

	// --- Process State ---
	SingboxCmd                  *exec.Cmd
	SingboxPrivilegedMode       bool   // true when sing-box was started with RunWithPrivileges (macOS TUN)
	SingboxPrivilegedPID        int    // PID of the start script (for wait/exit handling)
	SingboxPrivilegedSingboxPID int    // PID of the sing-box process (for privileged kill)
	SingboxPrivilegedPIDFile    string // temp file path holding both PIDs (for diagnostics)
	CmdMutex                    sync.Mutex
	ParserMutex                 sync.Mutex // Mutex for ParserRunning
	ParserRunning               bool
	StoppedByUser               bool
	RestartRequestedByUser      bool // true when user clicked Restart: watcher must bring process back up
	ConsecutiveCrashAttempts    int

	// --- VPN Operation State ---
	RunningState *RunningState

	// classic is the classic engine's lifecycle owner: generation, phase and
	// process identity in one place. Asynchronous work (crash monitor,
	// privileged waiter, restart delay) captures the generation it was created
	// in and must confirm it is still current before touching state or starting
	// a process — see core/classic_runtime.go.
	classic classicRuntime

	// buildMu serializes config builds.
	//
	// A rebuild is a read-modify-write on ONE file (config.json): it reads the
	// state, renders a config, writes a candidate and promotes it. Two rebuilds
	// running at once therefore interleave — the loser's candidate can be
	// promoted after the winner's, leaving on disk a config built from a state
	// snapshot that is already out of date. A build triggered by a settings
	// change must not race the build a Start performs, and the daemon applies
	// whatever is on disk.
	//
	// This is a real mutex rather than a "busy" flag: the second caller WAITS for
	// a correct config instead of being refused one. Refusing would make a
	// perfectly reasonable sequence — change a setting, press Start — fail
	// because the two happened to overlap.
	buildMu sync.Mutex

	// lifecycleErr is the single record of the last runtime failure. Every
	// layer that notices a lifecycle problem writes here, and coreState()
	// reads here, so the frontend has one authoritative source instead of
	// depending on which UI happened to be attached.
	lifecycleErr lifecycleErrors
	// exitSettler settles in-flight IPC operations at exit; see RegisterExitSettler.
	exitSettler atomic.Value

	// --- Context for goroutine cancellation ---
	ctx        context.Context    // Context for cancellation
	cancelFunc context.CancelFunc // Cancel function for stopping goroutines

	// templateRefreshDone закрывается, когда фоновое обновление шаблона после
	// апгрейда (StartTemplateRefresh) закончилось; сборка config.json ждёт его
	// (awaitTemplateRefresh). nil — обновление не запускалось, ждать нечего.
	templateRefreshDone atomic.Pointer[chan struct{}]

	// --- Shutdown state ---
	// exitOnce guards GracefulExit: it is reachable both from the tray "Quit"
	// item / dashboard Exit button *and* from main() after Application.Run()
	// returns, so without this the whole teardown runs twice.
	exitOnce sync.Once
	// exiting поднимается первой строкой gracefulExit: пути остановки ядра
	// (ProcessService.Stop и др.) по нему не показывают модальные диалоги —
	// GracefulExit из трея выполняется на main-потоке Fyne, и диалог там
	// никогда не отрисуется, а окно уже закрывается.
	exiting atomic.Bool
	// storageSwitching — идёт переезд данных переключателем Portable
	// (SwitchPortable): старт ядра и автообновление подписок отказывают,
	// чтобы ничего не писалось в каталог, который копируется. При успехе не
	// снимается — процесс уходит в перезапуск.
	storageSwitching atomic.Bool

	// --- Update popup state ---

	// --- Installed core version cache (one successful check per run) ---
	installedCoreVersionCache   string // после первой успешной проверки — без повторных запусков sing-box version
	installedCoreVersionCacheMu sync.Mutex

	// --- Кэш тегов сборки ядра для узлового гейта (SPEC 142 волна 5) ---
	// Сбрасывается по (mtime, size) бинаря ядра: переустановка ядра в той же
	// сессии пробует заново. См. core_capabilities.go.
	coreBuildTagsCache   *coreBuildTagsVerdict
	coreBuildTagsCacheMu sync.Mutex

	// D-121 / SPEC 131 W2c: кэша для гейта tls.reality.key_share здесь
	// больше нет — полевые гейты считает табличный проход по реестру
	// (core/config/node_build_gate.go), а версию ядра кэширует уже
	// GetInstalledCoreVersion.

	// --- Chain-support probe cache (SPEC 110) ---
	// Тип `chain` есть только в ядрах, собранных с `with_lx_chain`, и ядро
	// отвергает ВЕСЬ конфиг на неизвестном типе outbound'а. Кэш
	// инвалидируется тем же (mtime, size) бинаря, что и naive-проба.
	chainSupportCache   *chainSupportVerdict
	chainSupportCacheMu sync.Mutex

	// --- Auto-update per-source retry timers (SPEC 052 phase 8 event model) ---
	// Map source.ID → pending retry timer. Один retry на 15 секунд после
	// failed fetch; следующая попытка — на следующем heartbeat'е (1ч) или
	// при VPN-event'е (proxy switch / VPN on-off).
	autoUpdateRetryTimers map[string]*time.Timer
	autoUpdateRetryMu     sync.Mutex

	// autoUpdateEventLastFetch — per-source timestamp последнего
	// event-triggered fetch'а; используется eventCooldownAllow чтобы
	// rapid VPN/proxy events не дублировали fetch одной подписки
	// в рамках 5-сек окна. Защищён той же autoUpdateRetryMu.
	autoUpdateEventLastFetch map[string]time.Time

	// SubscriptionMu — state-level lock на load→mutate→save цикл при
	// мутации Meta подписок. Любые конкурентные пути (heartbeat refresh
	// всех source'ов, UI per-source Refresh, VPN-event triggered retry,
	// manual Update) сериализуются через этот mutex — иначе вторая save
	// перетирает изменения первой по полям, которых не касалась.
	//
	// UI handler'ы вызывают через async goroutine (presenter.UpdateUI),
	// поэтому ожидание lock'а не блокирует main thread; пользователь
	// видит spinner на per-source Refresh кнопке до освобождения.
	SubscriptionMu sync.Mutex
}

// RunningState - structure for tracking the VPN's running state.
type RunningState struct {
	running bool
	sync.RWMutex
	controller *AppController

	// autoPingTimer schedules a one-shot ping-all ~5s after running flips true,
	// so the Servers tab shows fresh latency the moment the user looks. The
	// timer is stopped when running flips back to false so a stopped process
	// never gets pinged. Guarded by the embedded RWMutex.
	autoPingTimer *time.Timer
}

// autoPingDelayAfterConnect is how long we wait after sing-box enters the
// running state before triggering a background ping-all. Matches LxBox mobile.
const autoPingDelayAfterConnect = 5 * time.Second

var (
	instance     *AppController
	instanceOnce sync.Once
)

// GetController returns the global AppController singleton, or nil if
// NewAppController has not run yet. main.go constructs the controller before
// any UI/service code calls this (single construction path — ADR-070-7), so
// callers can rely on a non-nil result; GetControllerOrPanic makes that
// contract explicit.
func GetController() *AppController {
	return instance
}

// GetControllerOrPanic returns the global AppController, panicking if the
// singleton has not been constructed yet. Use at callsites that cannot
// proceed without it — all of which run after main.go's NewAppController.
func GetControllerOrPanic() *AppController {
	if instance == nil {
		panic("core: GetController called before NewAppController")
	}
	return instance
}

// GetURLBytes loads url via the standard app HTTP client (timeouts, HTTP(S)_PROXY).
// UI and other layers should use this instead of calling CreateHTTPClient directly.
func (*AppController) GetURLBytes(ctx context.Context, url string, timeout time.Duration) ([]byte, int, error) {
	return GetURLBytes(ctx, url, timeout)
}

// NewAppController creates and initializes a new AppController instance.
// This function should be called only once at application startup (typically in main.go).
// It sets the global singleton instance that can be accessed via GetController().
// layout is the data layout resolved once in main() (SPEC 135).
func NewAppController(layout paths.Layout, appIconData, greyIconData, greenIconData, redIconData []byte) (*AppController, error) {
	ac := &AppController{}
	locale.CreateHTTPClientFunc = CreateHTTPClient

	// Initialize FileService first (needed by other services)
	fileService, err := services.NewFileService(layout)
	if err != nil {
		return nil, fmt.Errorf("NewAppController: cannot create FileService: %w", err)
	}
	ac.FileService = fileService

	// Open log files with rotation support
	if err := ac.FileService.OpenLogFiles(); err != nil {
		return nil, fmt.Errorf("NewAppController: cannot open log files: %w", err)
	}
	api.SetAPILogFile(ac.FileService.ApiLogFile)

	// Initialize RunningState before UIService (needed for callback)
	ac.RunningState = &RunningState{controller: ac}
	ac.RunningState.Set(false)

	// No GUI is created here. The presentation layer builds its own service
	// and installs it with SetUIPort; a headless backend simply never does.
	// This is what keeps fyne.io out of core's dependency graph.
	_ = appIconData
	_ = greyIconData
	_ = greenIconData
	_ = redIconData
	ac.ConsecutiveCrashAttempts = 0
	ac.ProcessService = NewProcessService(ac)
	ac.ConfigService = NewConfigService(ac)
	// Активный движок ядра. По умолчанию — классический spawn; daemon-режим
	// (macOS) поднимается ниже из settings.json после инициализации сервисов.
	ac.backend = NewLegacyBackend(ac)

	// Узловой гейт ядра (SPEC 044/122/123 → SPEC 142 волна 5): генератор
	// снимает узел, который ядру не по силам (протокол без тега сборки, поле
	// новее ядра), вместо того чтобы отдать конфиг, который целиком завалит
	// `check`. Что чему нужно — реестр; отсюда только теги сборки ядра.
	config.CoreBuildTagsProbe = ac.CoreBuildTags
	config.ChainSupportProbe = ac.CoreSupportsChain
	// SPEC 131 W2c: полевые гейты (снимается ПОЛЕ, узел живёт) больше не
	// заводятся пробой на каждое поле — их считает один табличный проход по
	// реестру, которому нужна лишь версия ядра. Так ушла
	// RealityKeyShareSupportProbe, и так же уйдёт всякое следующее поле с
	// min_core: правка реестра вместо пробы, хука и ветки в эмиттере.
	config.CoreVersionProbe = ac.coreVersionForBuildGate

	// SPEC 122: корень каталогов состояния tailnet. Тот же корень
	// `<DataDir>/bin`, относительно которого лежат локальные .srs — эмиссия
	// DataDir не знает, и путь приходит сюда единственной точкой.
	config.SetTailscaleStateDirRoot(platform.GetTailscaleStateDir(ac.FileService.Layout.Data))

	// SPEC 112: идентичность узла (тег) и УПРАЗДНЁННЫЙ контент-хеш для
	// миграции legacy-отметок. Обе живут в config (эмиттер нужен второй),
	// парсер — в subscription, поэтому зависимости подставляются здесь
	// (прямой вызов дал бы цикл импорта).
	subscription.NodeIdentityFunc = config.NodeIdentity
	subscription.LegacyNodeIdentityHashFunc = config.LegacyNodeIdentityHash

	// OnWindowShown намеренно НЕ занимается проверкой обновлений приложения
	// (SPEC 147): self-update удалён целиком, см. комментарий в main.go.
	// Сам хук остаётся — им пользуются другие подписчики (показ окна,
	// уведомление о отклонённых ядром узлах, сброс состояния при показе).

	// Initialize APIService
	apiService, err := services.NewAPIService(
		ac.FileService.ConfigPath,
		func() bool { return ac.RunningState.IsRunning() },
		func() {
			// OnProxiesUpdated callback.
			if ac.uiPort != nil {
				group := ac.APIService.GetSelectedClashGroup()
				active := ac.APIService.GetActiveProxyName()
				ac.uiPort.SetListStatus(fmt.Sprintf("Proxies loaded for '%s'. Active: %s", group, active))
				ac.uiPort.RefreshProxyList()
				ac.uiPort.UpdateCoreStatus()
			}
		},
		func() {
			// OnProxySwitched callback
			if ac.hasUI() {
				ac.uiPort.UpdateCoreStatus()
				ac.uiPort.RefreshProxyList()
			}
		},
	)
	if err != nil {
		return nil, fmt.Errorf("NewAppController: cannot create APIService: %w", err)
	}
	ac.APIService = apiService

	// UI callback defaults (no-op placeholders) are installed by
	// uiservice.NewUIService above; the UI layer (dashboard / clash tab)
	// replaces them with real handlers. No need to re-set them here.

	// Initialize context for goroutine cancellation
	ac.ctx, ac.cancelFunc = context.WithCancel(context.Background())

	// Initialize EventBus + StateService (SPEC 045/047 — typed events
	// заменят coarse-grained UpdateConfigStatusFunc/UpdateCoreStatusFunc
	// в фазе 6; пока bus используется только для StateChanged из StateService).
	ac.EventBus = events.NewMemoryBus()
	ac.StateService = services.NewStateService()
	ac.StateService.EventBus = ac.EventBus

	// Daemon-режим (macOS): если включён в settings.json — заменяет
	// LegacyBackend, установленный выше. Требует готовых FileService и
	// APIService (транспорт-override ставится в конструкторе).
	ac.initBackendFromSettings()

	// Имя собственного TUN — из конфига, лежащего с прошлого запуска. Без него
	// пикер аплинков не отличил бы наш singbox-tun0 от чужого системного
	// туннеля, который теперь законно предлагается к выбору (SPEC 113-F).
	ac.refreshOwnTunNames()

	// Check if config file exists before starting auto-update
	if _, err := os.Stat(ac.FileService.ConfigPath); os.IsNotExist(err) {
		debuglog.InfoLog("Auto-update: Config file does not exist (%s), auto-update disabled", ac.FileService.ConfigPath)
		ac.StateService.SetAutoUpdateEnabled(false)
	}
	go ac.startAutoUpdateLoop()

	// Set global singleton instance
	instanceOnce.Do(func() {
		instance = ac
	})

	return ac, nil
}

// UpdateUI asks the GUI to re-render from current state.
//
// A no-op in the headless backend, where uiPort is nil.
func (ac *AppController) UpdateUI() {
	if ac.uiPort != nil {
		ac.ui().UpdateCoreStatus()
	}
}

// hasUI reports whether a GUI is attached.
func (ac *AppController) hasUI() bool {
	return ac.uiPort != nil
}

// GracefulExit performs a graceful shutdown of the application.
//
// Two call sites reach this: the tray "Quit" item (and the dashboard Exit
// button), and main() after Application.Run() returns. exitOnce makes the
// second call a no-op instead of a second full teardown.
//
// На macOS есть третий путь: запрос системы на завершение (Cmd+Q,
// «Завершить» в Dock, выход из системы) — platform.SetQuitRequestHandler.
// Там GracefulExit идёт в горутине, пока AppKit держит главный поток и ждёт
// ответа не дольше бюджета из main.go; Quit в конце встаёт в очередь Fyne,
// до которой дело не доходит, — процесс завершает AppKit.
func (ac *AppController) GracefulExit() {
	ac.exitOnce.Do(ac.gracefulExit)
}

// Дедлайны сторожевых таймеров gracefulExit. Первый взводится до остановки
// ядра и покрывает всю фазу teardown (на Windows она ограничена: taskkill +
// ожидание ≤ 2 с; на macOS privileged-путь может ждать пароль — 15 с
// хватает, чтобы ввести его, но не оставляют зомби навсегда). Второй —
// прежний короткий бюджет на размотку Fyne после Quit.
const (
	shutdownTeardownDeadline = 15 * time.Second
	shutdownUnwindDeadline   = 3 * time.Second
)

// IsExiting reports whether GracefulExit has begun.
func (ac *AppController) IsExiting() bool {
	return ac.exiting.Load()
}

func (ac *AppController) gracefulExit() {
	ac.exiting.Store(true)
	// Test seam: runs at the start of the teardown so a test can hold it open and
	// observe the difference between "begun" and "finished".
	if exitHookForTest != nil {
		exitHookForTest()
	}
	// Сторож взводится ДО остановки ядра. Из трея этот код идёт на
	// main-потоке Fyne (драйвер маршалит action через runOnMain), и всё,
	// что здесь зависнет, зависнет вместе с циклом событий: окно не
	// отрисуется, диалоги не покажутся, Quit из очереди не выполнится —
	// процесс останется жить без окна и без иконки. Раньше сторож
	// взводился только после teardown и эту фазу не покрывал.
	if ac.hasUI() {
		ac.forceExitAfter(shutdownTeardownDeadline, "teardown (core stop / log close)")
	}

	// Cancel context to signal all goroutines to stop
	if ac.cancelFunc != nil {
		ac.cancelFunc()
		debuglog.InfoLog("GracefulExit: Context cancelled, signalling goroutines to stop")
	}

	// Stop any pending menu update timer
	if ac.hasUI() {
		ac.ui().UpdateCoreStatus()
	}

	// Через backend: classic останавливает ядро (как раньше), daemon может
	// оставить его работать (выход из лаунчера ≠ выключение VPN).
	waitForStop := true
	if b := ac.Backend(); b != nil {
		if p, ok := b.(persistentCoreBackend); ok && p.PersistsAfterAppExit() {
			debuglog.InfoLog("GracefulExit: launcher is quitting; daemon core will remain running")
		} else if _, ok := b.(persistentCoreBackend); ok {
			debuglog.InfoLog("GracefulExit: stopping daemon core by user preference")
		} else {
			debuglog.InfoLog("GracefulExit: stopping classic core")
		}
		waitForStop = b.OnAppExit()
	} else {
		StopSingBoxProcess()
	}

	if runtime.GOOS == "darwin" {
		platform.FreePrivilegedAuthorization()
	}

	if waitForStop {
		debuglog.InfoLog("GracefulExit: Waiting for sing-box to stop...")
		// Use ProcessService constant for timeout
		timeout := time.After(2 * time.Second) // gracefulShutdownTimeout from ProcessService
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
	waitLoop:
		for {
			if !ac.RunningState.IsRunning() {
				debuglog.InfoLog("GracefulExit: Sing-box confirmed stopped.")
				break waitLoop
			}
			select {
			case <-timeout:
				// Identity-verified force stop, not a bare Process.Kill(): the raw
				// signal could hit a recycled PID, and it could not reach a
				// privileged root core at all (SingboxCmd is nil there), so the
				// old code logged "Forcing kill" and left the TUN up.
				debuglog.WarnLog("GracefulExit: Timeout waiting for sing-box to stop. Forcing verified stop.")
				if ac.ProcessService != nil {
					if !ac.ProcessService.ForceStopOwnedCore() {
						debuglog.ErrorLog("GracefulExit: the core could not be confirmed stopped before exit")
					}
				}
				break waitLoop
			case <-ticker.C:
				// Check state on each tick - continue loop to re-check IsRunning()
			}
		}
	} else {
		debuglog.InfoLog("GracefulExit: daemon mode keeps the core running; skipping stop wait")
	}

	// A stop operation must not outlive the process that owns its record.
	//
	// Everything above has had its chance to stop the core and to report it. If a
	// stop is STILL registered at this point, the app is about to exit with an
	// operation that will never reach a terminal state — and the wire state it
	// would have published no longer has a reader. Settling it here is not a
	// cosmetic cleanup: it is what keeps "the record is only cleared by a real
	// completion" from turning into "the record is never cleared", which is how the
	// UI came to sit in `stopping` for the rest of a session.
	//
	// The state itself is NOT faked: RunningState keeps whatever truth the stop
	// paths established, so a core that survived the stop is still reported as
	// running to anyone who asks before the process ends.
	ac.SettleOperationsAtExit()

	if ac.FileService != nil {
		api.SetAPILogFile(nil)
		ac.FileService.CloseLogFiles()
	}

	if ac.hasUI() {
		// Armed before Quit so a driver that refuses to unwind can't strand
		// the process. By this point sing-box is stopped and the log files
		// are closed, so os.Exit loses nothing.
		ac.forceExitAfter(shutdownUnwindDeadline, "Fyne event loop unwind after Quit")
		ac.ui().QuitApplication()
	}
}

// persistentCoreArmed reports whether the running core is owned by the system
// daemon and is configured to outlive the GUI. It only decides log wording:
// a forced exit is a real orphan in classic mode, but in daemon mode the core
// is expected to keep running (SPEC 150).
func (ac *AppController) persistentCoreArmed() bool {
	b := ac.Backend()
	if b == nil {
		return false
	}
	p, ok := b.(persistentCoreBackend)
	return ok && p.PersistsAfterAppExit()
}

// forceExitAfter arms a last-resort os.Exit for one phase of shutdown.
// Field reports (2026-08): quitting from the tray with the window hidden
// leaves the process alive — on Windows the tray icon stays behind and
// relaunching the .exe shows "already running". The tray icon part is fixed
// deterministically in UIService.QuitApplication (systray.Quit); the
// watchdogs guarantee the process itself dies even if teardown or the
// driver's shutdown path stalls. Armed twice from gracefulExit: once before
// core stop (long budget) and once before Quit (short budget, nothing left
// to lose: sing-box stopped, logs closed).
func (ac *AppController) forceExitAfter(d time.Duration, phase string) {
	time.AfterFunc(d, func() {
		ac.forceExitHook(phase, d)
		os.Exit(0)
	})
}

// forceExitHookImpl writes the watchdog line. Wording depends on what the
// forced exit actually strands: in classic mode the core is a child of this
// process and is orphaned by the exit (a real problem — warning), while in
// daemon mode the core is owned by the system service and is *supposed* to
// outlive the GUI, so nothing is wrong (plain INFO, no scary "core left
// behind").
func (ac *AppController) forceExitHookImpl(phase string, d time.Duration) {
	if ac.persistentCoreArmed() {
		debuglog.InfoLog("Shutdown watchdog: %s did not finish within %s; forcing launcher exit — the daemon keeps the VPN core running (this is the configured behaviour)", phase, d)
		return
	}
	debuglog.WarnLog("Shutdown watchdog: %s did not finish within %s, forcing process exit (a running classic core may be left behind)", phase, d)
}

// forceExitHook is the seam through which forceExitAfter writes its line:
// tests replace it to observe the wording decision without calling os.Exit.
var forceExitHook = func(ac *AppController, phase string, d time.Duration) {
	ac.forceExitHookImpl(phase, d)
}

func (ac *AppController) forceExitHook(phase string, d time.Duration) {
	forceExitHook(ac, phase, d)
}

// RunHidden launches an external command in a hidden window.
func (ac *AppController) RunHidden(name string, args []string, logPath string, dir string) error {
	cmd := exec.Command(name, args...)
	platform.PrepareCommand(cmd)
	if dir != "" {
		cmd.Dir = dir
	}

	if logPath != "" {
		if logPath == ac.FileService.ChildLogPath && ac.FileService.ChildLogFile != nil {
			// For sing-box logs, check and rotate if needed before writing
			ac.FileService.CheckAndRotateLogFile(logPath)
			logFile := ac.FileService.ChildLogFile
			// Don't truncate - append to preserve logs, rotation handles size limits
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		} else {
			// For other logs (parser), use truncate mode for clean start
			logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, platform.DefaultFileMode)
			if err != nil {
				return fmt.Errorf("RunHidden: cannot open log file '%s': %w", logPath, err)
			}
			defer debuglog.RunAndLog(fmt.Sprintf("RunHidden: close log file %s", logPath), logFile.Close)
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
	}

	return cmd.Run()
}

// CheckLinuxCapabilities checks Linux capabilities and shows a suggestion if needed
func CheckLinuxCapabilities() {
	ac := GetController()
	if ac == nil {
		return
	}
	if suggestion := platform.CheckAndSuggestCapabilities(ac.FileService.CoreBinaryPath()); suggestion != "" {
		debuglog.InfoLog("CheckLinuxCapabilities: %s", suggestion)
		// Show dialog with selectable command and Copy button (issue #34)
		if ac.hasUI() {
			cmd := platform.GetSetCapCommand(ac.FileService.CoreBinaryPath())
			ac.uiPort.ShowCommandNeedsTerminal("Linux Capabilities", suggestion, cmd)
		}
	}
}

// Set sets the new value for the 'running' state and triggers a UI update,
// with no teardown reason: the transition is a crash, a routine refresh, or the
// core coming up.
//
// USE SetStopped WHEN THE CORE WENT DOWN DELIBERATELY. `Running == false` has
// several causes that are indistinguishable on the wire and need opposite
// responses, so a deliberate teardown must say so AT THE MOMENT IT HAPPENS.
//
// WHY THE REASON IS A PARAMETER RATHER THAN A RECORDED FLAG. An earlier version of
// this stored the reason in a slot on the controller and read it here. That was
// wrong in a way that produced real bugs: the store and the flip are two separate
// steps on a shared mutable slot, so a reason could survive a no-op write, outlive
// the teardown it described, and later label an unrelated CRASH as a completed user
// stop — settling a stop operation that never happened. Passing the reason makes
// the note and the flip ONE operation, which removes the entire class.
func (r *RunningState) Set(value bool) {
	r.set(value, events.TeardownNone)
}

// SetStopped records that the core went down for a stated reason.
func (r *RunningState) SetStopped(reason events.TeardownReason) {
	r.set(false, reason)
}

// set is the single implementation: the value and its reason are applied and
// published together, so no reader can observe one without the other.
func (r *RunningState) set(value bool, reason events.TeardownReason) {
	r.Lock()
	if r.running == value {
		r.Unlock()
		// A no-op write changes nothing, and now it also records nothing: there is
		// no slot to leave stale. This is precisely the leak the previous design
		// admitted.
		return
	}
	r.running = value
	// On false→true: arm auto-ping timer. On true→false: cancel any pending one.
	// Capturing ac here avoids touching r.controller in the timer goroutine after
	// unlock, and makes the intent explicit.
	ac := r.controller
	if value {
		if r.autoPingTimer != nil {
			r.autoPingTimer.Stop()
			r.autoPingTimer = nil
		}
		if ac != nil && ac.StateService != nil && ac.StateService.IsAutoPingAfterConnectEnabled() {
			r.autoPingTimer = time.AfterFunc(autoPingDelayAfterConnect, func() {
				// Re-check running: user may have hit Stop inside the 5s window.
				if !r.IsRunning() {
					return
				}
				// Soft-cap: skip auto-ping on huge subscriptions — opening hundreds
				// of TCP+TLS handshakes through TUN starves in-flight game / app
				// traffic in the connect window. Manual «Test» / Cmd+P / debug-API
				// are unaffected. See SPEC 039 §1.3.
				if max := ac.StateService.GetAutoPingMaxProxies(); max > 0 {
					if count := len(ac.GetProxiesList()); count > max {
						debuglog.InfoLog("auto-ping: skipped on connect (proxies=%d > cap=%d); use Servers → Test or Cmd/Ctrl+P for manual ping", count, max)
						return
					}
				}
				if ac.uiPort != nil {
					debuglog.DebugLog("auto-ping: triggering after %v of running state", autoPingDelayAfterConnect)
					ac.uiPort.AutoPingAfterConnect()
				}
			})
		}
	} else if r.autoPingTimer != nil {
		r.autoPingTimer.Stop()
		r.autoPingTimer = nil
	}
	r.Unlock()

	r.controller.UpdateUI()

	// SPEC 047 / audit BUG6: publish typed VpnStateChanged on the ACTUAL
	// running transition (Set dedups no-op calls above). Subscribers such as
	// the Core-tab icon (ui/app.go) were previously dead — nothing published
	// this event; only the legacy UpdateCoreStatusFunc callback fired.
	if ac != nil && ac.EventBus != nil {
		// The reason travels WITH the transition, because `false` alone cannot
		// distinguish the causes and they need opposite responses: a user stop ends
		// a stop operation, while a restart's teardown must not — the core is
		// coming back, and reporting the stop as complete would be a lie told
		// moments before a new core appears.
		//
		// A `false` with no reason is a crash or a routine refresh, and ends
		// nothing. The reason is this call's own argument, so it cannot belong to
		// some other teardown.
		publishReason := events.TeardownNone
		if !value {
			publishReason = reason
		}
		ac.EventBus.Publish(events.Event{
			Kind:    events.VpnStateChanged,
			Payload: events.VpnStateChangedPayload{Running: value, Teardown: publishReason},
		})
	}
}

// IsRunning checks if the VPN is running.
// Uses RLock to allow concurrent reads without blocking each other.
func (r *RunningState) IsRunning() bool {
	r.RLock()
	defer r.RUnlock()
	return r.running
}

// SetProxiesList safely sets the proxies list with mutex protection.
func (ac *AppController) SetProxiesList(proxies []api.ProxyInfo) {
	if ac.APIService != nil {
		ac.APIService.SetProxiesList(proxies)
	}
}

// GetProxiesList safely gets a copy of the proxies list with mutex protection.
func (ac *AppController) GetProxiesList() []api.ProxyInfo {
	if ac.APIService != nil {
		return ac.APIService.GetProxiesList()
	}
	return []api.ProxyInfo{}
}

// SetActiveProxyName safely sets the active proxy name with mutex protection.
func (ac *AppController) SetActiveProxyName(name string) {
	if ac.APIService != nil {
		ac.APIService.SetActiveProxyName(name)
	}
}

// GetActiveProxyName safely gets the active proxy name with mutex protection.
func (ac *AppController) GetActiveProxyName() string {
	if ac.APIService != nil {
		return ac.APIService.GetActiveProxyName()
	}
	return ""
}

// GetLastSelectedProxyForGroup gets the last selected proxy name for a specific selector group.
func (ac *AppController) GetLastSelectedProxyForGroup(group string) string {
	if ac.APIService != nil {
		return ac.APIService.GetLastSelectedProxyForGroup(group)
	}
	return ""
}

// SetSelectedIndex safely sets the selected index with mutex protection.
func (ac *AppController) SetSelectedIndex(index int) {
	if ac.APIService != nil {
		ac.APIService.SetSelectedIndex(index)
	}
}

// GetSelectedIndex safely gets the selected index with mutex protection.
func (ac *AppController) GetSelectedIndex() int {
	if ac.APIService != nil {
		return ac.APIService.GetSelectedIndex()
	}
	return -1
}

// StartSingBoxProcess launches the sing-box process.
// skipRunningCheck: если true, пропускает проверку на уже запущенный процесс (для автоперезапуска)
// Note: ProcessService must be initialized in NewAppController. This is a wrapper for backward compatibility.
func StartSingBoxProcess(skipRunningCheck ...bool) {
	ac := GetController()
	if ac == nil {
		return
	}
	if ac.ProcessService == nil {
		debuglog.WarnLog("StartSingBoxProcess: ProcessService is nil, this should not happen. Initializing...")
		ac.ProcessService = NewProcessService(ac)
	}
	// Единая точка старта ядра (UI, трей, -start, Debug API): во время
	// переезда данных ядро не стартует — оно писало бы в копируемый каталог.
	if ac.IsStorageSwitching() {
		debuglog.WarnLog("StartSingBoxProcess: refused, data move in progress")
		if ac.hasUI() {
			ac.uiPort.ShowError(locale.T("Error"), locale.T("Data move in progress"))
		}
		return
	}
	if b := ac.Backend(); b != nil {
		b.StartVPN(skipRunningCheck...)
		return
	}
	ac.ProcessService.Start(skipRunningCheck...)
}

// EnsureVPNRunning запускает VPN только если он ещё не поднят (SPEC 150).
//
// Нужен автозапуску (`-start`), который срабатывает через секунду после
// старта лаунчера. В daemon-режиме ядро вполне может уже работать — его
// подняла служба в прошлой сессии или им управляет кто-то ещё. Тогда
// обычный Start означал бы лишний POST /admin/apply: пересборку config.json
// и in-process подмену инстанса, то есть короткий разрыв туннеля на ровном
// месте — ровно то, чего не должно происходить при повторном открытии
// лаунчера.
//
// Отличие от StartVPN, который проверяет только внутренний RunningState:
// здесь в daemon-режиме состояние спрашивается у самого демона. К моменту
// срабатывания таймера автозапуска стрим статуса может ещё не прислать
// первый кадр, и RunningState будет false при работающем ядре — по нему
// решать нельзя.
//
// Возвращает true, если запуск действительно инициирован.
func EnsureVPNRunning(skipRunningCheck ...bool) bool {
	ac := GetController()
	if ac == nil {
		return false
	}
	if ac.RunningState != nil && ac.RunningState.IsRunning() {
		debuglog.InfoLog("EnsureVPNRunning: already running (internal state); auto-start is a no-op")
		return false
	}
	// daemon: спросить демон, а не догадываться по локальному состоянию.
	if b := ac.Backend(); b != nil {
		if probe, ok := b.(interface{ CoreRunningOnDaemon() (bool, bool) }); ok {
			if running, known := probe.CoreRunningOnDaemon(); known && running {
				debuglog.InfoLog("daemon: VPN already running; auto-start is a no-op")
				return false
			}
		}
	}
	StartSingBoxProcess(skipRunningCheck...)
	return true
}

// StopSingBoxProcess is the unified function to stop the sing-box process.
// Note: ProcessService must be initialized in NewAppController. This is a wrapper for backward compatibility.
func StopSingBoxProcess() {
	ac := GetController()
	if ac == nil {
		return
	}
	if ac.ProcessService == nil {
		debuglog.WarnLog("StopSingBoxProcess: ProcessService is nil, this should not happen. Initializing...")
		ac.ProcessService = NewProcessService(ac)
	}
	if b := ac.Backend(); b != nil {
		b.StopVPN()
		return
	}
	ac.ProcessService.Stop()
}

// KillSingBoxForRestart applies a fresh config through the active backend:
// classic — kill so the monitor restarts the process; daemon — in-process
// apply without killing anything.
func KillSingBoxForRestart() {
	ac := GetController()
	if ac == nil || ac.ProcessService == nil {
		return
	}
	if b := ac.Backend(); b != nil {
		b.RestartVPN()
		return
	}
	ac.ProcessService.KillForRestart()
}

// RunParserProcess starts the internal configuration update process.
// Note: ConfigService must be initialized in NewAppController. This is a wrapper for backward compatibility.
func RunParserProcess() {
	ac := GetController()
	if ac == nil {
		return
	}
	if ac.ConfigService == nil {
		debuglog.WarnLog("RunParserProcess: ConfigService is nil, this should not happen. Initializing...")
		ac.ConfigService = NewConfigService(ac)
	}
	ac.ConfigService.RunParserProcess()
}

// CheckIfSingBoxRunningAtStartUtil checks if sing-box is already running at application start.
// Note: ProcessService must be initialized in NewAppController. This is a wrapper for backward compatibility.
func CheckIfSingBoxRunningAtStartUtil() {
	ac := GetController()
	if ac == nil {
		return
	}
	if ac.ProcessService == nil {
		debuglog.WarnLog("CheckIfSingBoxRunningAtStartUtil: ProcessService is nil, this should not happen. Initializing...")
		ac.ProcessService = NewProcessService(ac)
	}
	// Проверяем и в daemon-режиме: осиротевшее classic-ядро (`sing-box run`,
	// пережившее прошлую сессию лаунчера) держит маршруты на своём TUN, пока
	// профилировщик и статус смотрят на пустое ядро демона. Детектор на
	// darwin аргумент-чувствительный (pgrep -f `sing-box run|…-privileged`)
	// и демона `sing-box lxd` не ловит — ложного предложения убить демона
	// не будет; деградацию pgrep гейтит isSingBoxProcessRunning.
	ac.ProcessService.CheckIfRunningAtStart()
}

// CleanupStaleTunAtStartUtil runs Win7 ghost-TUN cleanup on launcher startup when
// sing-box is not already running (SPEC 065).
//
// Без прав администратора на Windows (SPEC 139 §6 п. 2–3) ни NLA-профили в
// HKLM, ни адаптеры, ни правила брандмауэра не трогаются: одна строка INFO
// с перечнем пропущенного, очистки пройдут при старте с правами.
func CleanupStaleTunAtStartUtil() {
	ac := GetController()
	if ac == nil || ac.ProcessService == nil {
		return
	}
	if windowsNotElevated() {
		logSkippedAdminCleanups()
		return
	}
	ac.ProcessService.CleanupStaleTunAtStart()
	cleanupOrphanSingTunFirewallRulesAtStart()
}

// cleanupOrphanSingTunFirewallRulesAtStart убирает накопившиеся правила
// брандмауэра `sing-tun (<путь>)` от старых установок (Windows; no-op иначе).
// Без гейта на запущенное ядро: удаляются только правила с несуществующим
// бинарём, правило живого пути не трогается, а своё ядро пересоздаёт правило
// само при следующем поднятии TUN.
func cleanupOrphanSingTunFirewallRulesAtStart() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				debuglog.WarnLog("cleanupOrphanSingTunFirewallRulesAtStart: recovered from panic: %v", r)
			}
		}()
		removed, err := platform.CleanupOrphanSingTunFirewallRules()
		if err != nil {
			debuglog.WarnLog("cleanupOrphanSingTunFirewallRulesAtStart: %v", err)
			return
		}
		if removed > 0 {
			debuglog.WarnLog("cleanupOrphanSingTunFirewallRulesAtStart: removed %d orphan firewall rule(s)", removed)
		}
	}()
}

// CheckConfigFileExists checks if config.json exists and shows a warning if it doesn't
func CheckConfigFileExists() {
	ac := GetController()
	if ac == nil {
		return
	}
	if _, err := os.Stat(ac.FileService.ConfigPath); os.IsNotExist(err) {
		debuglog.WarnLog("CheckConfigFileExists: config.json not found at %s", ac.FileService.ConfigPath)

		message := locale.Tf(configNotFoundMessageText, constants.ConfigFileName)

		if ac.hasUI() {
			ac.uiPort.ShowInfo(locale.T("Configuration Not Found"), message)
		}
	}
}

func CheckIfLauncherAlreadyRunningUtil() {
	ac := GetController()
	if ac == nil {
		return
	}
	execPath, err := os.Executable()
	if err != nil {
		debuglog.ErrorLog("CheckIfLauncherAlreadyRunning: cannot detect executable path: %v", err)
		return
	}
	execName := strings.ToLower(filepath.Base(execPath))
	currentPID := os.Getpid()

	processes, err := process.GetProcesses()
	if err != nil {
		debuglog.ErrorLog("CheckIfLauncherAlreadyRunning: error listing processes: %v", err)
		return
	}

	for _, p := range processes {
		if p.PID == currentPID {
			continue
		}
		if strings.EqualFold(p.Name, execName) {
			if ac.hasUI() {
				ac.uiPort.ShowInfo(locale.T("Information"), locale.T("The application is already running. Use the existing instance or close it before starting a new one."))
			}
			return
		}
	}
}

// AutoLoadProxies attempts to load proxies with retry intervals (1, 3, 7, 13, 17 seconds).
func (ac *AppController) AutoLoadProxies() {
	if ac.APIService != nil {
		ac.APIService.AutoLoadProxies(ac.ctx)
	}
}

// VPNButtonState represents the state of Start/Stop VPN buttons
type VPNButtonState struct {
	BinaryExists bool
	IsRunning    bool
	StartEnabled bool
	StopEnabled  bool
}

// DaemonEngineAvailable reports whether the lxd daemon engine can run on this
// platform and build.
//
// Exported for the headless backend, which advertises it as a capability so
// the frontend can hide daemon-only settings instead of guessing from the OS.
func DaemonEngineAvailable() bool {
	return daemonEngineAvailable() == nil
}

// ElevationSupported reports whether the backend can request privileges on
// this platform.
func ElevationSupported() bool {
	return ElevateAtStartSupported || runtime.GOOS == "darwin"
}

// GetVPNButtonState returns the current state for VPN buttons (used by both Core Dashboard and Tray Menu)
func (ac *AppController) GetVPNButtonState() VPNButtonState {
	// Check if sing-box executable exists (same logic as Core Dashboard tab)
	_, err := ac.GetInstalledCoreVersion()
	binaryExists := err == nil

	// Check if config.json exists
	configExists := false
	if _, err := os.Stat(ac.FileService.ConfigPath); err == nil {
		configExists = true
	}

	// Check if wintun.dll exists (only on Windows)
	wintunExists := true // Default to true for non-Windows
	if runtime.GOOS == "windows" {
		exists, err := ac.CheckWintunDLL()
		if err != nil {
			// Error checking - assume not available
			wintunExists = false
		} else {
			wintunExists = exists
		}
	}

	// Get current running state
	isRunning := ac.RunningState.IsRunning()

	state := VPNButtonState{
		BinaryExists: binaryExists,
		IsRunning:    isRunning,
	}

	// Invariants (SPEC 045):
	//
	//   * Start  = есть binary + config + wintun (Win) + НЕ running.
	//             Не зависит от state.json (start умеет работать на legacy
	//             config'е без state); не зависит от template.
	//
	//   * Stop   = ТОЛЬКО running. Если sing-box работает — стопнуть всегда
	//             должно быть можно, даже если в этот момент config.json
	//             удалили или wintun.dll исчез. Иначе пользователь может
	//             залипнуть с running-процессом без UI-способа его убить.
	state.StartEnabled = binaryExists && configExists && wintunExists && !isRunning
	state.StopEnabled = isRunning

	return state
}
