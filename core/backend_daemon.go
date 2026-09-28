//go:build darwin || (windows && !386)

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	"singbox-launcher/api"
	"singbox-launcher/core/services"
	daemonpb "singbox-launcher/internal/daemonpb"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/lxdclient"
	"singbox-launcher/internal/paths"
	"singbox-launcher/internal/platform"

	"singbox-launcher/core/events"
)

// DaemonBackend — движок daemon-режима: ядро живёт внутри долгоживущего
// `sing-box lxd` (служба ОС; на macOS — launchd), лаунчер управляет им по admin REST
// (apply/start/stop) и наблюдает по gRPC daemon.StartedService (протокол
// Android-линии). Смена конфига — in-process подмена инстанса в демоне:
// без убийства процесса, без пароля, с валидацией и автооткатом на
// last-good на стороне демона.
type DaemonBackend struct {
	ac    *AppController
	admin *lxdclient.Client

	// stopping is true from the moment a stop is accepted until the daemon
	// confirms the core is gone. Guarded internally; see SetStopping.
	stopping atomic.Bool

	// closed is set by Close() to retire this engine's epoch. Every async
	// callback must consult isActive(), which now requires !closed, so nothing
	// from a retired backend can write state after the handover.
	closed atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc

	connMu sync.Mutex
	conn   *grpc.ClientConn

	// caps — какие RPC реально реализует доступный демон. Определяется
	// однократной пробой (core/daemon_rpc_compat.go): интерфейс сгенерированного
	// клиента о сервере не говорит ничего.
	caps *daemonCapabilities
	// transport — этот backend's gRPC proxy-транспорт (установлен в
	// APIService.transportOverride). Хранится, чтобы Close снимал ТОЛЬКО
	// свой override, а setBackend мог переустановить его после Close чужого.
	transport *daemonProxyTransport

	// clashFallback — локальный Clash API ЭТОГО демона: чем заменить RPC,
	// которых у сборки демона нет (см. daemon_clash_fallback.go).
	clashFallback daemonClashFallback
	// fallbackMu охраняет expectedGroups (пишется из apply, читается из
	// proxy-запросов на других горутинах).
	fallbackMu sync.Mutex
	// expectedGroups — selector-теги конфига, который демон реально получил.
	// Доказательство при проверке fallback-эндпоинта.
	expectedGroups []string
	// expectedMembers completes the identity proof. Group names alone can
	// coincide with a foreign core's; members are derived from the config WE sent
	// and cannot.
	expectedMembers map[string][]string

	// applyMu сериализует Start/Restart/Stop от дребезга кнопок.
	applyMu sync.Mutex

	// applyInFlight counts applies that have begun and not yet returned.
	//
	// applyMu serializes applies against EACH OTHER but says nothing to the
	// outside, so the engine handover had no way to ask "is this engine in the
	// middle of applying a config?". `RunningState` cannot answer it either: an
	// apply has not started a core yet, so the running flag is legitimately
	// false for the whole window in which the daemon is being told to start one.
	//
	// Handing the engine over in that window is how a switch to classic could
	// leave a DAEMON core running: the abandoned backend's apply carried on and
	// started it. The counter is the engine's own "I am busy" answer, and the
	// handover refuses while it is non-zero.
	applyInFlight atomic.Int32

	// applyGeneration is incremented whenever this backend stops owning applies
	// (Close), so a retry loop can abandon a world that no longer exists.
	applyGeneration atomic.Uint64

	// rejectTries — сколько узлов выключено за текущий заход Start/Restart
	// по второму источнику сигнала (422 / FATAL). Сбрасывается при STARTED.
	rejectTries int32

	// Кольцевой буфер логов ядра из gRPC SubscribeLog: в daemon-режиме
	// stdout/stderr ядра принадлежат службе (файл в root-каталоге 0700),
	// поэтому вьюер логов читает отсюда.
	logMu    sync.Mutex
	logLines []string

	// connTracker — накопительный снимок соединений из SubscribeConnections
	// (traffic-график по gRPC вместо Clash /connections).
	connTracker *connTracker

	// tailscale — кеш последнего снимка SubscribeTailscaleStatus (SPEC 130);
	// стрим держит superviseTailscale.
	tailscale services.TailscaleStatusCache

	// link — состояние канала к демону для индикатора у «Core Status».
	// Считается по кадрам статус-стрима: отдельного heartbeat нет, чтобы не
	// добавлять трафик ради кружка.
	linkMu sync.Mutex
	link   DaemonLinkState

	// appliedProxy — строка системного прокси последнего успешного apply
	// (string; "" — прокси не просили). Windows: лаунчер ставит его сам
	// (SPEC 141 §7) и возвращает, когда ядро снова started.
	appliedProxy atomic.Value
}

// daemonLogMaxLines зеркалит кольцевой буфер демона (lxd logMaxLines).
const daemonLogMaxLines = 3000

// daemonLinkFailThreshold — сколько промахов подряд превращают «моргнуло»
// (жёлтый) в «не отвечает» (красный). Зеркалит heartbeatFailThreshold у
// маркеров удалённых машин, чтобы индикаторы читались одинаково.
const daemonLinkFailThreshold = 2

// LinkState отдаёт снимок состояния канала (AppController.DaemonLink).
func (b *DaemonBackend) LinkState() DaemonLinkState {
	b.linkMu.Lock()
	defer b.linkMu.Unlock()
	return b.link
}

// noteLinkOK отмечает пришедший кадр статуса; возвращает true, если состояние
// маркера изменилось и UI надо перерисовать (первый кадр, возврат после
// промахов, смена fatal-состояния ядра).
func (b *DaemonBackend) noteLinkOK(coreFatal bool, fatalErr string) bool {
	b.linkMu.Lock()
	defer b.linkMu.Unlock()
	changed := !b.link.EverConnected || b.link.FailStreak > 0 || b.link.CoreFatal != coreFatal
	b.link.EverConnected = true
	b.link.FailStreak = 0
	b.link.LastErr = ""
	b.link.LastOK = time.Now()
	b.link.CoreFatal = coreFatal
	b.link.FatalErr = fatalErr
	return changed
}

// noteLinkFail отмечает промах канала. Перерисовку просим только на смене
// цвета маркера (первый промах — жёлтый, порог — красный), как в heartbeat
// удалённых машин.
func (b *DaemonBackend) noteLinkFail(err error) bool {
	b.linkMu.Lock()
	defer b.linkMu.Unlock()
	b.link.FailStreak++
	if err != nil {
		b.link.LastErr = err.Error()
	}
	return b.link.FailStreak == 1 || b.link.FailStreak == daemonLinkFailThreshold
}

// DaemonIdentityDir — каталог клиентской пары сопряжения (bin/daemon).
func DaemonIdentityDir(dataDir paths.DataDir) string {
	return platform.GetDaemonIdentityDir(dataDir)
}

// DaemonConfigFromSettings строит конфиг клиента демона из settings.json.
// Ошибка — если daemon-режим не сконфигурирован (нет адреса) или не удалось
// поднять клиентскую пару при включённом TLS.
func DaemonConfigFromSettings(ac *AppController) (lxdclient.Config, error) {
	binDir := ac.FileService.Layout.Data.Bin()
	st := locale.LoadSettings(binDir)
	if st.DaemonAddress == "" {
		return lxdclient.Config{}, fmt.Errorf("daemon is not configured: install the service or pair via invite")
	}
	cfg := lxdclient.Config{
		Addr:              st.DaemonAddress,
		ServerFingerprint: st.DaemonServerFingerprint,
		Secret:            st.DaemonSecret,
	}
	if cfg.TLSEnabled() {
		ident, err := lxdclient.LoadOrCreateIdentity(DaemonIdentityDir(ac.FileService.Layout.Data))
		if err != nil {
			return lxdclient.Config{}, err
		}
		cfg.Identity = ident
	}
	return cfg, nil
}

// newDaemonBackend конструирует daemon-движок из settings.json и запускает
// supervisor статуса. Вызывается из initBackendFromSettings/SwitchBackendMode.
// Платформа без готового слоя службы (daemonEngineAvailable) — отказ, и
// лаунчер остаётся на classic.
func newDaemonBackend(ac *AppController) (CoreBackend, error) {
	if err := daemonEngineAvailable(); err != nil {
		return nil, err
	}
	cfg, err := DaemonConfigFromSettings(ac)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &DaemonBackend{
		ac:     ac,
		admin:  lxdclient.New(cfg),
		ctx:    ctx,
		cancel: cancel,
		caps:   &daemonCapabilities{},
	}
	b.transport = &daemonProxyTransport{b: b}
	b.connTracker = newConnTracker()
	if ac.APIService != nil {
		ac.APIService.SetTransport(b.transport)
		// THE TRAFFIC SAMPLER NEEDS THE VERIFIED ENDPOINT, NOT THE TRANSPORT.
		//
		// `b.transport` is a gRPC client for proxy operations and carries no HTTP address,
		// so a caller that needs to poll `/connections` cannot get one from it. The Clash
		// endpoint lives behind the fallback, where it is only handed out once its identity
		// has been PROVEN to be this daemon's — which is exactly the check the sampler must
		// not skip, because reading connection counters from a stranger's core is the same
		// trust decision as switching nodes on it.
		//
		// Published as a function so each sample re-asks: a captured endpoint would keep
		// answering after its verification expired.
		ac.APIService.SetVerifiedClashEndpoint(func() services.ClashTransport {
			tr, ok := b.clashFallback.transportIfReady()
			if !ok {
				return services.ClashTransport{}
			}
			return tr
		})
	}
	go b.superviseStatus()
	go b.superviseLogs()
	go b.superviseConnections()
	go b.superviseTailscale()
	return b, nil
}

// reinstallTransport переустанавливает свой transport-override в APIService.
// Вызывается setBackend после Close предыдущего backend'а (тот мог снять
// общий override в nil). Идемпотентно.
func (b *DaemonBackend) reinstallTransport() {
	if b.ac.APIService != nil && b.transport != nil {
		b.ac.APIService.SetTransport(b.transport)
	}
}

// diagnoseReachError превращает сырую ошибку связи с демоном в понятный
// пользователю совет (английский) и реализует «лаунчер следует за демоном»:
// режимом канала владеет демон (tls в его daemon.json), клиент лишь
// обнаруживает смену (lxdclient.DetectChannel) и подстраивается — на
// loopback сброс пина автоматический, по сети только совет (авто-даунгрейд
// в сетевом сценарии открывал бы MITM'у downgrade-атаку). Обратный переход
// (демон стал TLS) автоматике недоступен принципиально: пин нельзя
// выдумать, доверие приезжает только с приглашением.
func (b *DaemonBackend) diagnoseReachError(err error) string {
	s := err.Error()
	addr := b.admin.AddrString()
	switch {
	case strings.Contains(s, "first record does not look like a TLS handshake"):
		// Профиль лаунчера — TLS (пин установлен), а на адресе живёт
		// plain-демон: демон перевели на tls:false.
		if lxdclient.DetectChannel(addr) == lxdclient.ChannelPlain {
			if lxdclient.IsLoopbackAddr(addr) {
				b.ac.followDaemonPlainChannel()
				return "Daemon at " + addr + " switched to plain (secret) mode — the launcher followed automatically. " +
					"If the daemon uses a secret, set it in the connection window (Servers tab, gear button), then try again."
			}
			return "Daemon at " + addr + " now runs in plain (secret) mode. " +
				"Unpair in the connection window to follow it (automatic follow over the network is disabled: it would enable downgrade attacks)."
		}
		return "Cannot reach the daemon: " + s
	case strings.Contains(s, "connection refused"), strings.Contains(s, "no such host"), strings.Contains(s, "dial tcp"):
		return "Daemon is not reachable at " + addr +
			". Install the service and pair (Servers tab, gear button, Local), then try again."
	case strings.Contains(s, "fingerprint"), strings.Contains(s, "certificate"):
		return "Daemon certificate changed (the service was reinstalled). " +
			"Re-pair with a fresh invite (Servers tab, gear button, Local), then try again."
	case strings.Contains(s, "HTTP request to an HTTPS server"):
		// Профиль лаунчера — plain, а демон отвечает 400-сигнатурой
		// net/http-TLS-сервера: его вернули на tls:true.
		return "Daemon at " + addr + " now requires mTLS. " +
			"Pair with a fresh invite (mint one with `sudo sing-box lxd client add` on its host), then try again."
	case strings.Contains(s, "EOF"), strings.Contains(s, "connection reset"):
		// Профиль лаунчера — plain, а демон рвёт HTTP-запросы: вероятно, его
		// вернули на TLS (tls:true) — plain-клиенту он отвечать не будет.
		if lxdclient.DetectChannel(addr) == lxdclient.ChannelTLS {
			return "Daemon at " + addr + " now requires mTLS. " +
				"Pair with a fresh invite (mint one with `sudo sing-box lxd client add` on its host), then try again."
		}
		return "Cannot reach the daemon: " + s
	default:
		return "Cannot reach the daemon: " + s
	}
}

// isActive сообщает, является ли этот backend всё ещё активным в контроллере.
// Мёртвый backend (вытеснен setBackend) не должен трогать общее состояние
// (RunningState, UI): его стримы/apply могли пережить Close.
func (b *DaemonBackend) isActive() bool {
	// !closed is not redundant with the pointer check: during setBackend the
	// outgoing backend is Closed while ac.backend still points at it, so the
	// pointer comparison alone reports a dying engine as live.
	return !b.closed.Load() && b.ac.Backend() == CoreBackend(b)
}

// Mode implements CoreBackend.
func (b *DaemonBackend) Mode() BackendMode { return BackendDaemon }

// Admin возвращает REST-клиент admin-плоскости (для секции настроек).
func (b *DaemonBackend) Admin() *lxdclient.Client { return b.admin }

// StartVPN implements CoreBackend: пересборка config.json (как classic) и
// доставка его демону через POST /admin/apply. Apply валидирует конфиг
// сабпроцессом и поднимает ядро; провал старта откатывается на last-good.
func (b *DaemonBackend) StartVPN(skipRunningCheck ...bool) {
	go func() {
		if err := b.StartVPNContext(context.Background()); err != nil {
			b.ac.ShowStartupError(err)
		}
	}()
}

// StartVPNContext applies the current config to the daemon and returns the
// reason on failure.
//
// THE BUG THIS FIXES: StartVPN used to `go b.applyCurrentConfig(...)` and return
// immediately. The IPC call therefore reported SUCCESS before the daemon had
// been contacted, the frontend cleared its "starting" state on that success, and
// when the apply then failed the only trace was a log line. The result was the
// reported symptom — the button snapped back to "Start" with no explanation —
// and it could not have been otherwise, because the failure happened strictly
// after the success had already been announced.
//
// Now the apply is awaited and its outcome returned, so "the request succeeded"
// and "the core is running" are no longer conflated. The transition to running
// still arrives asynchronously via the supervisor's status stream, which is the
// only truthful source for it.
func (b *DaemonBackend) StartVPNContext(ctx context.Context) error {
	ac := b.ac
	if ac.RunningState.IsRunning() {
		if ac.uiPort != nil {
			ac.uiPort.ShowInfo(locale.TN(1, "Info"), locale.T("Sing-Box already running (according to internal state)."))
		}
		return nil
	}
	return b.applyCurrentConfigContext(ctx, "StartVPN", false)
}

// RestartVPN implements CoreBackend: тот же apply — демон подменит инстанс
// in-process, канал управления и соединения переживут смену конфига.
//
// forced=true: Restart всегда ПОЛНОСТЬЮ пересобирает config.json, даже если
// dirty-маркеры уже сняты (авто-rebuild мог успеть их сбросить). Иначе
// applyCurrentConfig взял бы старый config.json, и правки пользователя не
// доехали бы до демона — ровно тот баг «новый конфиг не загружается при
// перезапуске». Согласовано с classic-путём кнопки Rebuild (forced=true).
func (b *DaemonBackend) RestartVPN() {
	go func() {
		if err := b.RestartVPNContext(context.Background()); err != nil {
			b.ac.ShowStartupError(err)
		}
	}()
}

// RestartVPNContext is RestartVPN with the apply awaited and its error returned.
// Same reasoning as StartVPNContext.
func (b *DaemonBackend) RestartVPNContext(ctx context.Context) error {
	return b.applyCurrentConfigContext(ctx, "RestartVPN", true)
}

// applyCurrentConfig — общий путь Start/Restart: rebuild → read → apply.
// forced прокидывается в RebuildConfigIfDirty: Restart форсирует полную
// пересборку, Start — обычный dirty-путь. 422 с именем узла выключает
// узел и повторяет apply в этом же заходе (потолок daemonRejectStartCap).
func (b *DaemonBackend) applyCurrentConfig(caller string, forced bool) error {
	// b.ctx, NOT context.Background(): this path is reached from the status
	// supervisor on a core FATAL, i.e. from a goroutine nobody owns. With a
	// Background parent, Close() could not cancel it, so after a daemon→classic
	// switch an abandoned backend could still push a config to the daemon the
	// user just left — and, worse, retryCoreReject would silently disable user
	// nodes on the way. Falling back to Background only when b.ctx is nil keeps
	// the "unconfigured backend" case working.
	parent := b.ctx
	if parent == nil {
		parent = context.Background()
	}
	return b.applyCurrentConfigContext(parent, caller, forced)
}

// applyCurrentConfigContext is applyCurrentConfig with cancellation.
//
// ctx is checked before each attempt, so a superseded or cancelled request does
// not leave an apply racing behind it, and between attempts so a retry loop
// cannot outlive its caller.
func (b *DaemonBackend) applyCurrentConfigContext(ctx context.Context, caller string, forced bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// WAIT FOR THE ENGINE, CANCELLABLY.
	//
	// A plain Mutex cannot be acquired with a context, so a caller stuck behind a
	// long apply was blocked for that apply's entire duration however short its
	// own deadline was — the "operation timeout" bounded only the work that
	// happened to get the lock, which is not a bound at all.
	//
	// Acquiring in a goroutine and selecting on the context makes the WAIT
	// interruptible. The lock is still taken and released by the same goroutine
	// discipline, and the value is handed back so the deferred Unlock runs on
	// this goroutine — Go's Mutex must be released by the goroutine that acquired
	// it, which this preserves.
	if !acquireWithContext(ctx, &b.applyMu) {
		return ctx.Err()
	}
	defer b.applyMu.Unlock()

	// Publish "this engine is applying" for the whole duration, so the handover
	// guard can refuse to switch the engine out from under it.
	b.applyInFlight.Add(1)
	defer b.applyInFlight.Add(-1)
	// Сброс — только у настоящего нового захода (Start/Restart). Заход
	// "core-reject-fatal" — это повтор apply ВНУТРИ того же цикла FATAL→
	// выключили→apply, счётчик там должен копиться, а не обнуляться, иначе
	// daemonRejectStartCap не защищает от бесконечной пары
	// «применили → FATAL → выключили → применили» (SPEC 132 §5, ловушка 8).
	if caller != "core-reject-fatal" {
		atomic.StoreInt32(&b.rejectTries, 0)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := b.applyOnce(caller, forced)
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
		forced = true
	}
}

// applyOnce — один проход rebuild→apply.
//
// retry=true means "a node was disabled, run again". A non-nil error always ends
// the loop: the caller owns reporting, so this function never shows UI. That is
// what lets the headless backend return a structured failure instead of writing
// to a log nobody reads.
func (b *DaemonBackend) applyOnce(caller string, forced bool) (retry bool, err error) {
	ac := b.ac

	// Pre-start rebuild — тот же хук, что в classic ProcessService.Start:
	// dirty-маркеры Wizard'а материализуются в config.json перед доставкой.
	// Restart форсирует полную пересборку (forced=true), иначе взяли бы
	// устаревший config.json. Провал пересборки — отказ с диалогом, а не
	// доставка демону старого файла.
	// OWNERSHIP BEFORE EVERY SIDE EFFECT.
	//
	// This used to be checked in exactly one place — after admin.Apply had
	// already returned. That ordering made the check useless for its purpose: by
	// then the backend had already rebuilt config.json on disk, reloaded the
	// Clash API config, prepared the daemon copy and POSTed the config. A backend
	// the user had already left could therefore still deliver a config and start
	// a core, and the single late check only stopped it from updating the UI —
	// the side effects had happened, and the daemon core was running under a user
	// who had switched away.
	//
	// The rebuild is the first side effect because it WRITES THE USER'S CONFIG:
	// a retired backend must not modify files on behalf of an engine nobody
	// selected any more.
	if !b.isActive() {
		debuglog.InfoLog("daemon.%s: backend is no longer active; not rebuilding or applying", caller)
		return false, nil
	}
	if err := ac.rebuildConfigBeforeStart(forced); err != nil {
		debuglog.ErrorLog("daemon.%s: config rebuild failed, config not applied: %v", caller, err)
		b.refreshUI()
		return false, NewStartFailure(StartErrConfigRebuildFailed, err)
	}

	// Синхронизируем APIService с пересобранным config.json. В daemon-режиме
	// Clash API из доставляемой демону копии ВЫРЕЗАН (prepareConfigForDaemon),
	// а proxy-операции идут через gRPC transport-override — но состояние
	// APIService (endpoint, Enabled) должно отражать config.json на диске,
	// чтобы возврат на classic-режим не унёс устаревший endpoint.
	if ac.APIService != nil {
		if err := ac.APIService.ReloadClashAPIConfig(); err != nil {
			debuglog.WarnLog("daemon.%s: reload Clash API config: %v", caller, err)
		}
	}
	{
		ac.ui().ResetAPIState()
	}

	config, err := os.ReadFile(ac.FileService.ConfigPath)
	if err != nil {
		// No config to deliver: the same class as a failed rebuild, and equally
		// the user's business — the core did not start.
		return false, NewStartFailure(StartErrConfigRebuildFailed,
			fmt.Errorf("daemon apply: cannot read config.json: %w", err))
	}

	// Pre-flight: убеждаемся, что демон жив и его сертификат совпадает с
	// закреплённым, ДО отправки конфига — иначе пользователь видит сырой
	// "connection refused" / "fingerprint mismatch" вместо понятного совета.
	if _, err := b.admin.StatusCtx(b.ctxOrNil()); err != nil {
		// The daemon did not answer. "Cannot reach the daemon" is a different
		// remedy from "the daemon rejected the config", so it gets its own code
		// rather than one generic failure.
		return false, NewStartFailuref(StartErrDaemonUnreachable, "%s", b.diagnoseReachError(err))
	}

	// Подготовка конфига для демона: (1) абсолютизация cache_file в каталог,
	// которым ВЛАДЕЕТ демон — state_dir из его паспорта (/admin/info); демон
	// работает с cwd="/", относительный путь ушёл бы в read-only корень.
	// Fallback на историческую константу — только если info недоступен
	// (демон старой сборки). (2) Полное удаление clash_api (всё по gRPC).
	// Classic-режим этой подготовки не проходит (cwd=bin/, единственное ядро).
	runtimeDir := daemonFallbackStateDir()
	passport, infoErr := b.admin.InfoCtx(b.ctxOrNil())
	if infoErr == nil && passport.StateDir != "" {
		runtimeDir = passport.StateDir
	} else if infoErr != nil {
		debuglog.WarnLog("daemon.%s: /admin/info unavailable (%v); using fallback runtime dir", caller, infoErr)
	}
	// SPEC 136: служба не на актуальной root-owned копии — громко в лог, но
	// apply не блокируется (у пользователя работающий VPN, ремонт — одна
	// команда).
	var passportPtr *lxdclient.InfoData
	if infoErr == nil {
		passportPtr = &passport
	}
	if check := ac.daemonServiceCheck(passportPtr, b.admin.AddrString()); check.NeedsInstall() {
		debuglog.WarnLog("daemon.%s: the daemon service is %s (%s) — run the Install or update service command",
			caller, check.State, check.Detail)
	} else if check.NeedsBootstrap() {
		debuglog.WarnLog("daemon.%s: the daemon service is installed but not running (%s) — run: %s",
			caller, check.Detail, daemonBootstrapCommand())
	}
	prepared, err := prepareDaemonConfig(config, runtimeDir, daemonPlatformPrepOptions())
	if err != nil {
		return false, NewStartFailure(StartErrDaemonApplyFailed,
			fmt.Errorf("daemon apply: prepare config: %w", err))
	}
	config, proxyServer := prepared.Bytes, prepared.ProxyServer

	// The last checkpoint before the IRREVERSIBLE step. Everything above is
	// recoverable local work; this call is what starts a core on the daemon. A
	// switch landing anywhere in the window above must be observed here, or the
	// daemon starts a core for an engine the user has left.
	if !b.isActive() {
		debuglog.InfoLog("daemon.%s: backend retired during preparation; config NOT applied", caller)
		return false, nil
	}
	if err := b.ctxOrNil().Err(); err != nil {
		debuglog.InfoLog("daemon.%s: cancelled before apply (%v); config NOT applied", caller, err)
		return false, err
	}

	debuglog.InfoLog("daemon.%s: applying config.json (%d bytes) to %s", caller, len(config), b.admin.AddrString())
	// ApplyCtx, not Apply: this is the call that can START A CORE, and it is the
	// longest one. With an uncancellable call the launcher's operation timeout
	// guaranteed nothing — a request stuck in the network outlived every deadline
	// and completed in a world that no longer existed (the user had switched
	// engines, the backend was closed).
	if err := b.admin.ApplyCtx(b.ctxOrNil(), config); err != nil {
		var applyErr *lxdclient.ApplyError
		if errors.As(err, &applyErr) && applyErr.Rejected() && b.retryCoreReject(applyErr.Message) {
			debuglog.WarnLog("daemon.%s: apply rejected a node — rebuild and retry", caller)
			return true, nil
		}
		debuglog.ErrorLog("daemon.%s: apply failed: %v", caller, err)
		// Статус мог смениться (откат/фатал) — supervisor подтянет.
		b.refreshUI()
		return false, NewClassifiedStartFailure(StartErrDaemonApplyFailed,
			fmt.Errorf("daemon apply: %w", err))
	}

	if !b.isActive() {
		// Backend вытеснен (смена адреса/пересопряжение) пока летел apply —
		// не трогаем общее состояние: им владеет новый backend.
		debuglog.InfoLog("daemon.%s: applied but backend is no longer active; skipping state update", caller)
		return false, nil
	}
	atomic.StoreInt32(&b.rejectTries, 0)
	// The daemon accepted the config, so the Clash API it will serve is now the
	// one this transformation described. Committing here and NOT before is the
	// whole point: a failed apply leaves the daemon on its previous config (or
	// on last-good after a rollback), and registering the new endpoint early
	// would aim the fallback at a port that nothing is listening on.
	//
	// Remote daemons never get a local fallback: 127.0.0.1 on this machine is
	// not the daemon's host.
	if isLocalDaemonAddress(b.admin.AddrString()) {
		b.clashFallback.setConfigured(prepared.ClashFallback)
		b.fallbackMu.Lock()
		// Record the FULL proof, not just the group names: names are common
		// enough to coincide by accident, and the secret may legitimately be
		// empty, in which case structure is the only evidence there is.
		b.expectedGroups = prepared.SelectorGroups
		b.expectedMembers = prepared.SelectorMembers
		b.fallbackMu.Unlock()
	} else {
		b.clashFallback.block()
		debuglog.InfoLog("daemon.%s: remote daemon — local Clash API fallback disabled", caller)
	}
	// SPEC 141 §7: системный прокси пользователя ставит лаунчер (Windows;
	// на macOS — no-op).
	b.appliedProxy.Store(proxyServer)
	if proxyServer != "" {
		ac.setDaemonSystemProxy(proxyServer)
	} else {
		ac.clearDaemonSystemProxy("the applied config asks for no system proxy")
	}
	// НЕ объявляем running здесь.
	//
	// Раньше стояло `ac.RunningState.Set(true)` с комментарием «стрим статусов
	// подтвердит» — и это противоречие: если стрим авторитетен, то ПРИНЯТЫЙ
	// apply ещё не STARTED. Ядро, которое падает сразу после приёма конфига
	// (ровно сценарий `default outbound not found`), успевало показать
	// пользователю «подключено» до того, как стало ясно, что оно не поднялось.
	//
	// Теперь running приходит ТОЛЬКО из потока статусов демона (STARTED), где
	// он и является проверенным фактом. Apply означает «принято»: это видно
	// как `starting` через фазу backend'а и operation-record, и этого
	// достаточно, чтобы кнопка показала прогресс.
	ac.StateService.ResetAutoUpdateFailedAttempts()
	debuglog.InfoLog("daemon.%s: config applied; waiting for the daemon to report STARTED", caller)
	b.refreshUI()

	go func() {
		select {
		case <-time.After(2 * time.Second):
		case <-b.ctx.Done():
			return
		}
		ac.AutoLoadProxies()
	}()
	return false, nil
}

// retryCoreReject выключает названный узел, если не исчерпан потолок захода.
func (b *DaemonBackend) retryCoreReject(errText string) bool {
	if b == nil || b.ac == nil {
		return false
	}
	if atomic.LoadInt32(&b.rejectTries) >= int32(daemonRejectStartCap) {
		debuglog.WarnLog("daemon: core-reject start cap of %d reached — stopping", daemonRejectStartCap)
		return false
	}
	if !b.ac.DisableNodeNamedByCore(errText) {
		return false
	}
	atomic.AddInt32(&b.rejectTries, 1)
	return true
}

// retryAfterCoreFatal reacts to a core FATAL reported by the daemon: it disables
// the node the core named and re-applies, then records the outcome.
//
// THE SILENT PATH THIS FIXES. When retryCoreReject declined — the retry cap was
// reached, or the FATAL named something that is not a node (the real case was
// `default outbound not found: proxy-out`, a route.final pointing at an outbound
// that does not exist) — this function simply returned. Nothing was recorded,
// nothing was shown, and the core stayed down: the user pressed Connect and the
// app went quiet. That is how a config-integrity bug turned into "the VPN just
// does not start".
//
// The failure is now recorded with the daemon's own message, which is the only
// place the true cause exists — it comes from the core, not from the launcher.
func (b *DaemonBackend) retryAfterCoreFatal(msg string) {
	if b == nil || b.ac == nil {
		return
	}

	// A RETIRED BACKEND MUST NOT REPAIR ANYTHING — and the check has to come
	// FIRST, before anything is mutated.
	//
	// `retryCoreReject` below calls DisableNodeNamedByCore, which PERSISTS a node
	// as disabled in the user's state. It is not a cache update or a hint: the
	// node disappears from the working set. The isActive guard used to sit after
	// it, so a late FATAL frame arriving after the user switched to the classic
	// engine still disabled a node — an irreversible, user-visible mutation made
	// on behalf of an engine nobody is running any more, and the log said only
	// that the retry had been skipped.
	//
	// This runs on a status-supervisor goroutine nobody owns, triggered by a
	// frame that may have been produced seconds ago, so the window is real rather
	// than theoretical.
	if !b.isActive() {
		debuglog.InfoLog("daemon: backend is no longer active; ignoring the FATAL frame entirely")
		return
	}

	if !b.retryCoreReject(msg) {
		// Not a node problem, or we are out of retries. Either way the core is
		// down for a reason the user needs to see, verbatim: the message names
		// the offending tag, which is what makes it actionable.
		debuglog.ErrorLog("daemon: core FATAL could not be resolved by disabling a node: %s", msg)
		b.ac.RecordLifecycleError(LifecycleErrCoreStart, "start",
			"the core refused the configuration", msg, false)
		return
	}

	// Re-checked after the node mutation: disabling a node is not instantaneous,
	// and a switch can land inside it. The apply is what must not happen for a
	// retired backend; the mutation above was already gated on being active.
	if !b.isActive() {
		debuglog.InfoLog("daemon: backend retired while handling the FATAL; not re-applying")
		return
	}
	b.applyCurrentConfig("core-reject-fatal", true)
}

// daemonStopTimeout bounds how long a stop waits for the daemon to confirm the
// core is gone. Generous for a service that has to tear down a TUN device.
const daemonStopTimeout = 20 * time.Second

// daemonStopPollInterval is how often the confirmation is re-read.
const daemonStopPollInterval = 150 * time.Millisecond

// StopVPN implements CoreBackend: ядро гаснет, демон и канал остаются жить.
//
// CONFIRMED STOP, not "a command was accepted". `/admin/stop` is an ordinary
// POST that returns 200 as soon as the daemon has ACCEPTED the request; it says
// nothing about whether the core actually exited. The old code treated that 200
// as proof, cleared RunningState and dropped the Clash fallback verification
// immediately — so a daemon whose core survived (still applying, wedged, or
// restarting itself) was reported to the user as stopped while the tunnel was
// still carrying traffic. That is precisely the lie the Classic path stopped
// telling, and the two engines must not disagree about what "stopped" means.
//
// The daemon is the authority here: its own /admin/status is polled until it
// reports the core is no longer up. Only then does the launcher publish stopped.
// On timeout the state is NOT faked: the failure is recorded, RunningState keeps
// saying "running", and the user is told the VPN may still be up — because it
// may well be.
func (b *DaemonBackend) StopVPN() {
	go func() {
		b.applyMu.Lock()
		defer b.applyMu.Unlock()
		ac := b.ac

		// Publish "stopping" before the work starts, so the UI shows progress
		// instead of a still-"connected" screen during the teardown.
		if ac != nil {
			ac.BeginDaemonStop(b)
		}

		if err := b.admin.Stop(); err != nil {
			debuglog.ErrorLog("daemon.StopVPN: %v", err)
			// Recorded before the UI branch. A stop that FAILED is not a cosmetic
			// problem: the tunnel is still up, and the user must be told so rather
			// than left believing it is down. The headless backend has no uiPort,
			// so the Fyne dialog alone reached nobody.
			msg := fmt.Errorf("daemon stop: %w", err).Error()
			if ac != nil {
				ac.RecordLifecycleError(LifecycleErrStopFailed, "stop", msg, "", true)
				ac.EndDaemonStop(b, false)
				if ac.hasUI() {
					b.ac.uiPort.ShowError(locale.T("Error"), msg)
				}
			}
			return
		}

		if err := b.awaitDaemonStopped(); err != nil {
			// Accepted but NOT confirmed. Saying "stopped" here is the original
			// bug, so the launcher says the opposite: the tunnel may still be up.
			debuglog.ErrorLog("daemon.StopVPN: stop not confirmed: %v", err)
			if ac != nil {
				ac.RecordLifecycleError(LifecycleErrStopFailed, "stop",
					"the daemon accepted the stop but the core is still running",
					err.Error(), true)
				ac.EndDaemonStop(b, false)
				if ac.hasUI() {
					ac.uiPort.ShowError(locale.T("Error"), locale.T(stopPrivilegedFailedText)+": "+err.Error())
				}
			}
			return
		}

		// Confirmed gone by the daemon itself.
		if ac != nil {
			ac.clearDaemonSystemProxy("VPN stopped")
			ac.EndDaemonStop(b, true)
		}
		// The core is down, so its Clash API listener is gone with it. Dropping
		// verification (not the configuration) means the next use re-proves the
		// endpoint instead of trusting a socket that no longer exists.
		b.clashFallback.invalidate()
	}()
}

// acquireWithContext takes mu, abandoning the wait if ctx is cancelled.
//
// Returns false when the context ended first, in which case the lock was NOT
// taken and the caller must not unlock.
//
// Go's sync.Mutex has no cancellable Lock, and it must be released by the
// goroutine that acquired it. So the acquisition runs in a helper goroutine and
// the CALLER waits on either the acquisition or its own context.
//
// OWNERSHIP IS DECIDED BY A SINGLE BUFFERED SEND, which is what makes the
// abandoned case safe without any sleep or retry:
//
//   - channel capacity 1, so the helper's send NEVER blocks;
//   - on success the helper sends the token and returns immediately WITHOUT
//     unlocking — the caller now owns the lock;
//   - the caller either receives the token (it owns the lock, and will unlock) or
//     its context ends first. If the context ended first, the caller performs a
//     NON-BLOCKING receive: a token present means the helper won the race and the
//     lock is held, so the caller must release it; no token means the helper has
//     not sent yet, and the caller records that by CLOSING a handshake channel
//     the helper checks before sending — so the helper, when it finally acquires,
//     sees the rejection and unlocks on the spot.
//
// There is no window in which both sides wait, and none in which neither
// releases: every path ends with exactly one owner.
func acquireWithContext(ctx context.Context, mu *sync.Mutex) bool {
	if ctx == nil {
		mu.Lock()
		return true
	}
	if err := ctx.Err(); err != nil {
		return false
	}

	acquired := make(chan struct{}, 1)
	abandoned := make(chan struct{})

	go func() {
		mu.Lock()
		// Deliver ownership, unless the caller has given up. The send cannot
		// block (capacity 1), so this goroutine never lingers.
		select {
		case <-abandoned:
			mu.Unlock()
		default:
			acquired <- struct{}{}
		}
	}()

	select {
	case <-acquired:
		return true
	case <-ctx.Done():
		// The caller gives up. Decide ownership without blocking.
		select {
		case <-acquired:
			// The helper had already won: the lock is ours to release.
			mu.Unlock()
		default:
			// The helper has not delivered. Tell it to release when it acquires.
			close(abandoned)
		}
		return false
	}
}

// ctxOrNil returns the backend context, or a background context when the
// backend was built without one (tests, unconfigured engines).
//
// A nil ctx must not silently mean "uncancellable": returning a background
// context keeps the call sites uniform while making the absence explicit in one
// place instead of scattering nil checks — and Close() still marks the backend
// inactive, which is the ownership signal those call sites actually depend on.
func (b *DaemonBackend) ctxOrNil() context.Context {
	if b == nil || b.ctx == nil {
		return context.Background()
	}
	return b.ctx
}

// ApplyInFlight reports whether an apply has begun and not yet returned.
//
// The engine handover asks this before switching away. It is deliberately NOT
// derived from RunningState: an apply has not started a core yet, so the running
// flag is false for exactly the window in which the daemon is being told to
// start one — and handing the engine over then is what let an abandoned backend
// carry on and start a daemon core after the user had switched to classic.
func (b *DaemonBackend) ApplyInFlight() bool {
	if b == nil {
		return false
	}
	return b.applyInFlight.Load() > 0
}

// --- test seams -------------------------------------------------------------
//
// These exist so the backend's state tests can assert ownership and busy rules
// without a live daemon. They expose only facts the engine already tracks; none
// of them can fabricate a running core.

// IsActiveForTest exposes isActive for tests.
func (b *DaemonBackend) IsActiveForTest() bool { return b.isActive() }

// ApplyInFlightForTest exposes the in-flight apply counter.
func (b *DaemonBackend) ApplyInFlightForTest() int32 { return b.applyInFlight.Load() }

// SetApplyInFlightForTest sets the in-flight apply counter, so a test can state
// "an apply is in progress" as a precondition.
func (b *DaemonBackend) SetApplyInFlightForTest(n int32) { b.applyInFlight.Store(n) }

// ApplyGenerationForTest exposes the apply generation.
func (b *DaemonBackend) ApplyGenerationForTest() uint64 { return b.applyGeneration.Load() }

// ApplyCurrentConfigForTest drives the apply entry point, for ownership tests.
func (b *DaemonBackend) ApplyCurrentConfigForTest(caller string, forced bool) error {
	return b.applyCurrentConfig(caller, forced)
}

// StopVPNContext stops the daemon core and returns the REAL outcome.
//
// The contextual interface covered Start and Restart but not Stop, so a stop had
// no way to report a reason. Failures went to a Fyne dialog — and the headless
// frontend has no uiPort, so a failed stop produced a log line, no protocol
// error, and a state that never settled. The user was left watching a screen
// with no explanation of why the tunnel was still up.
//
// This performs the same work as StopVPN with the same confirmations, and then
// RETURNS the failure instead of only recording it. StopVPN remains the
// fire-and-forget wrapper the GUI uses.
func (b *DaemonBackend) StopVPNContext(ctx context.Context) error {
	if b == nil || b.admin == nil {
		return NewStartFailure(StartErrDaemonUnreachable, fmt.Errorf("no daemon client"))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// The lock wait is CANCELLABLE. A plain Lock here queues behind whatever
	// apply is running — and an apply can take tens of seconds — so the caller's
	// deadline bounded nothing: the stop had not even started when its timeout
	// expired, and the reply then described a teardown that never began.
	if !acquireWithContext(ctx, &b.applyMu) {
		return ctx.Err()
	}
	defer b.applyMu.Unlock()
	ac := b.ac

	if ac != nil {
		ac.BeginDaemonStop(b)
	}

	// The stop itself is context-aware for the same reason: a daemon that accepts
	// the request and then does not answer must not hold the caller past its
	// deadline. A stop is not cancelled by that — RunStopContext cannot un-ask a
	// stop — but the CALLER stops waiting and is told why.
	if err := b.admin.StopCtx(ctx); err != nil {
		msg := fmt.Errorf("daemon stop: %w", err).Error()
		if ac != nil {
			ac.RecordLifecycleError(LifecycleErrStopFailed, "stop", msg, "", true)
			ac.EndDaemonStop(b, false)
		}
		return NewStartFailure(StartErrDaemonApplyFailed, fmt.Errorf("%s", msg))
	}

	if err := b.awaitDaemonStopped(); err != nil {
		errMsg := "the daemon accepted the stop but the core is still running"
		if ac != nil {
			ac.RecordLifecycleError(LifecycleErrStopFailed, "stop", errMsg, err.Error(), true)
			ac.EndDaemonStop(b, false)
		}
		return NewStartFailure(StartErrDaemonApplyFailed, fmt.Errorf("%s: %w", errMsg, err))
	}

	// Confirmed gone by the daemon itself.
	if ac != nil {
		ac.clearDaemonSystemProxy("VPN stopped")
		ac.EndDaemonStop(b, true)
	}
	b.clashFallback.invalidate()
	return nil
}

// SetStopping implements stopStateBackend: records that a stop is in flight so
// the wire state can report `stopping` instead of `running` while the core is
// being torn down.
func (b *DaemonBackend) SetStopping(stopping bool) {
	if b == nil {
		return
	}
	b.stopping.Store(stopping)
}

// Stopping reports whether a stop is in flight.
func (b *DaemonBackend) Stopping() bool {
	return b != nil && b.stopping.Load()
}

// awaitDaemonStopped polls the daemon until it confirms the core is no longer up.
//
// Reads the daemon's own status rather than any local flag: the whole point is
// that the launcher must not be the source of truth for whether the VPN is down.
// A daemon that cannot be reached is NOT treated as "stopped" — unknown is not
// evidence of success, and reporting it as such would recreate the lie in the
// disconnected case.
func (b *DaemonBackend) awaitDaemonStopped() error {
	if b.admin == nil {
		return fmt.Errorf("no daemon client")
	}
	parent := b.ctx
	if parent == nil {
		parent = context.Background()
	}
	deadline := time.Now().Add(daemonStopTimeout)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(parent, daemonProbeTimeout)
		info, err := b.admin.StatusCtx(ctx)
		cancel()
		if err != nil {
			lastErr = err
		} else {
			switch strings.ToLower(strings.TrimSpace(info.Status)) {
			case "idle", "":
				return nil
			case "fatal":
				// A core that died on its own is not running either; the FATAL
				// path owns explaining why, but the stop itself is satisfied.
				return nil
			default:
				// started / starting / stopping: work still in progress.
				lastErr = nil
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("the daemon did not confirm the stop within %s (%v)", daemonStopTimeout, lastErr)
			}
			return fmt.Errorf("the daemon did not confirm the stop within %s", daemonStopTimeout)
		}
		select {
		case <-time.After(daemonStopPollInterval):
		case <-parent.Done():
			return parent.Err()
		}
	}
}

// OnAppExit implements CoreBackend: по умолчанию выход из лаунчера оставляет
// VPN работать (это и есть смысл daemon-режима); опция DaemonStopVPNOnExit
// возвращает классическое поведение.
// CoreRunningOnDaemon спрашивает у САМОГО демона, работает ли ядро
// (SPEC 150). Возвращает (running, known): known=false, если демон не
// ответил — тогда решать по этому ответу нельзя, и вызывающий откатывается
// на обычный путь старта.
//
// Нужен автозапуску `-start`: он срабатывает через секунду после старта
// лаунчера, когда стрим статуса вполне может ещё не прислать первый кадр.
// Без этого запроса EnsureVPNRunning опирался бы на RunningState, который в
// этот момент ложно false, и делал бы лишний apply — то есть короткий
// разрыв туннеля при повторном открытии лаунчера.
//
// Запрос ограничен по времени: автозапуск не должен ждать зависший демон.
//
// Контекст берётся с fallback'ом на Background: паника «cannot create context
// from nil parent» здесь недопустима — проба зовётся из автозапуска и из UI,
// где незаведённый ctx означал бы падение всего приложения вместо честного
// «состояние неизвестно». nil-admin (backend ещё не собран) — то же самое.
func (b *DaemonBackend) CoreRunningOnDaemon() (running bool, known bool) {
	if b.admin == nil {
		return false, false
	}
	parent := b.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, daemonProbeTimeout)
	defer cancel()
	info, err := b.admin.StatusCtx(ctx)
	if err != nil {
		debuglog.DebugLog("daemon: status probe for auto-start failed: %v", err)
		return false, false
	}
	// Демон отвечает строкой; STARTED — единственное состояние, при котором
	// apply был бы лишним. STARTING/STOPPING считаем «идёт работа»: не
	// вмешиваемся, второй apply там точно не нужен.
	switch strings.ToLower(strings.TrimSpace(info.Status)) {
	case "started", "starting", "stopping":
		return true, true
	default:
		return false, true
	}
}

// daemonProbeTimeout — сколько ждать ответа демона в пробе перед автозапуском.
const daemonProbeTimeout = 3 * time.Second

// PersistsAfterAppExit implements persistentCoreBackend (SPEC 150): ядро
// daemon'а живёт в системной службе, и выход GUI его не касается — если
// пользователь не попросил обратного настройкой.
func (b *DaemonBackend) PersistsAfterAppExit() bool {
	binDir := b.ac.FileService.Layout.Data.Bin()
	return !locale.LoadSettings(binDir).DaemonStopVPNOnExit
}

func (b *DaemonBackend) OnAppExit() bool {
	binDir := b.ac.FileService.Layout.Data.Bin()
	if !locale.LoadSettings(binDir).DaemonStopVPNOnExit {
		return false
	}
	if err := b.admin.Stop(); err != nil {
		// The tunnel is provably still up. Reporting stopped here would be the
		// same lie StopVPN was fixed to stop telling — and worse, returning true
		// makes GracefulExit enter its wait loop, whose first check is
		// !RunningState.IsRunning(): our own eager write would satisfy it, so the
		// loop would "confirm" a stop that never happened.
		debuglog.WarnLog("daemon.OnAppExit: stop failed: %v", err)
		b.ac.RecordLifecycleError(LifecycleErrStopFailed, "stop",
			"the VPN could not be stopped on exit", err.Error(), true)
		return false
	}
	// Confirmed against the daemon itself, exactly as a user-initiated stop is.
	// A wedged core must not be reported as down just because the app is closing.
	if err := b.awaitDaemonStopped(); err != nil {
		debuglog.WarnLog("daemon.OnAppExit: stop not confirmed: %v", err)
		b.ac.RecordLifecycleError(LifecycleErrStopFailed, "stop",
			"the daemon accepted the stop on exit but the core is still running",
			err.Error(), true)
		return false
	}
	b.ac.clearDaemonSystemProxy("VPN stopped on exit")
	// A USER STOP, on the app's way out. Labelling it matters for the same reason
	// it matters anywhere else: an unlabelled `false` is indistinguishable from a
	// crash, so the stop operation would never be settled and the record would sit
	// at `stopping` while the app exits.
	b.ac.RunningState.SetStopped(events.TeardownUserStop)
	return true
}

// onEngineLeave — движок daemon сменяется на classic (SwitchBackendMode):
// системный прокси, поставленный лаунчером под демон, снимается (SPEC 141 §7).
func (b *DaemonBackend) onEngineLeave() {
	b.ac.clearDaemonSystemProxy("engine switched to classic")
}

// appliedProxyServer — строка прокси последнего успешного apply.
func (b *DaemonBackend) appliedProxyServer() string {
	s, _ := b.appliedProxy.Load().(string)
	return s
}

// Close implements CoreBackend: гасит supervisor и gRPC-соединение, снимает
// транспорт-override. Ядро в демоне не трогается.
//
// Частично сконструированный backend (ctx не заведён) тоже обязан закрываться
// без паники: Close вызывается из путей смены движка и выхода, где ронять
// процесс из-за nil-поля нельзя — это утащило бы за собой и живое ядро.
func (b *DaemonBackend) Close() {
	// Invalidate callbacks BEFORE anything else, and before the caller publishes
	// the replacement backend.
	//
	// isActive() is a pointer comparison against ac.Backend(), and setBackend
	// runs prev.Close() while ac.backend still points at prev — so for the whole
	// duration of this function a stale backend still looks active, and a status
	// frame decoded just before Close could publish RunningState after the new
	// backend went live. A daemon engine has no generation counter; this flag is
	// what gives it the equivalent "my epoch is over" test.
	b.closed.Store(true)
	// Bump the apply generation so any loop that re-checks before retrying sees
	// that its world is gone, even if it holds a context that has not observed
	// cancellation yet.
	b.applyGeneration.Add(1)
	if b.cancel != nil {
		b.cancel()
	}
	// Снимаем override ТОЛЬКО если он всё ещё наш: при daemon→daemon свопе
	// новый backend уже установил свой транспорт до нашего Close, и затирать
	// его в nil нельзя (иначе proxy-операции теряют gRPC-транспорт).
	if b.ac.APIService != nil && b.ac.APIService.TransportOverride() == services.ProxyTransport(b.transport) {
		b.ac.APIService.SetTransport(nil)
		// AND THE VERIFIED-ENDPOINT PROVIDER, WHICH IS A SEPARATE REGISTRATION.
		//
		// The transport and the provider were installed together but only one was removed,
		// and the survivor is worse than useless: the provider stays INSTALLED and starts
		// answering "no endpoint" once this backend is closed. `trafficEndpoint` treats an
		// installed provider as authoritative and never falls through to the configured
		// endpoint, so a classic core with a perfectly good address got `ok=false` forever —
		// the speed readout dark in exactly the mode the trust fix was meant to protect,
		// through the opposite door from the type-assertion bug it replaced.
		//
		// Guarded by the same ownership test as the transport: a daemon→daemon swap has
		// already installed the new backend's provider by the time this runs, and clearing it
		// would leave the NEW daemon with no endpoint at all.
		b.ac.APIService.SetVerifiedClashEndpoint(nil)
	}
	b.connMu.Lock()
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	b.connMu.Unlock()
	// This backend no longer owns the daemon (reconnect, address change,
	// re-pairing, or shutdown). Its verification belongs to a connection that
	// is now gone.
	b.clashFallback.invalidate()
}

// refreshUI дёргает статус-виджеты после операций, которые могли не дать
// перехода RunningState (Set дедуплицирует no-op вызовы).
func (b *DaemonBackend) refreshUI() {
	// Guarded like every sibling UI call in this file (StopVPN, StartVPNContext):
	// the headless backend has no uiPort, and ac.ui() dereferences it.
	if b.ac == nil || !b.ac.hasUI() {
		return
	}
	b.ac.ui().UpdateCoreStatus()
}

// grpcConn лениво создаёт единственное gRPC-соединение (grpc.NewClient сам
// переустанавливает транспорт при обрывах).
func (b *DaemonBackend) grpcConn() (*grpc.ClientConn, error) {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	if b.conn != nil {
		return b.conn, nil
	}
	conn, err := b.admin.DialGRPC()
	if err != nil {
		return nil, err
	}
	b.conn = conn
	return conn, nil
}

// grpcClient возвращает клиента протокола StartedService.
func (b *DaemonBackend) grpcClient() (daemonpb.StartedServiceClient, error) {
	conn, err := b.grpcConn()
	if err != nil {
		return nil, err
	}
	return daemonpb.NewStartedServiceClient(conn), nil
}

// superviseStatus держит подписку SubscribeServiceStatus: демон шлёт текущий
// статус сразу при подписке и каждый переход дальше — один стрим видит всё
// без переподключения. Обрыв → reconnect с экспоненциальным backoff.
//
// Backoff сбрасывается ТОЛЬКО после реально полученного кадра: создание
// стрима у grpc-go ленивое (NewClient не коннектится, Subscribe* возвращает
// stream без ошибки даже при лежащем демоне — ошибка всплывает в первом
// Recv), поэтому сброс «по успешному Subscribe» держал бы reconnect-цикл на
// вечной 1s без роста.
func (b *DaemonBackend) superviseStatus() {
	backoff := time.Second
	for {
		if b.ctx.Err() != nil {
			return
		}
		client, err := b.grpcClient()
		if err == nil {
			var stream grpc.ServerStreamingClient[daemonpb.ServiceStatus]
			stream, err = client.SubscribeServiceStatus(b.ctx, &emptypb.Empty{})
			if err == nil {
				var recvErr error
				var received bool
				received, recvErr = b.consumeStatusStream(stream)
				if received {
					backoff = time.Second
				}
				err = recvErr
			}
		}
		if err != nil {
			debuglog.DebugLog("daemon.supervisor: status stream unavailable: %v", err)
		}
		// Промах канала: стрим не поднялся или оборвался. Закрытие по Close
		// (ctx отменён) промахом не считается — там уже нечего показывать.
		if b.ctx.Err() == nil && b.isActive() && b.noteLinkFail(err) {
			b.refreshUI()
		}
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// consumeStatusStream читает стрим до обрыва; первым значением возвращает,
// был ли получен хотя бы один кадр (стрим реально работал, а не умер на первом
// Recv), вторым — ошибку обрыва: supervisor показывает её в маркере канала.
func (b *DaemonBackend) consumeStatusStream(stream grpc.ServerStreamingClient[daemonpb.ServiceStatus]) (bool, error) {
	ac := b.ac
	received := false
	for {
		status, err := stream.Recv()
		if err != nil {
			debuglog.DebugLog("daemon.supervisor: status stream closed: %v", err)
			return received, err
		}
		received = true
		if !b.isActive() {
			// Стрим вытесненного backend'а не должен дёргать общее состояние.
			continue
		}
		// Кадр дошёл — демон на связи. Маркер перерисовываем только на смене
		// состояния, а не на каждом кадре.
		if b.noteLinkOK(status.GetStatus() == daemonpb.ServiceStatus_FATAL, status.GetErrorMessage()) {
			b.refreshUI()
		}
		wasRunning := ac.RunningState.IsRunning()
		var running bool
		switch status.GetStatus() {
		case daemonpb.ServiceStatus_STARTED:
			running = true
			atomic.StoreInt32(&b.rejectTries, 0)
		case daemonpb.ServiceStatus_STARTING, daemonpb.ServiceStatus_STOPPING:
			// In-process reload (apply конфига) проходит STOPPING→STARTING→
			// STARTED без разрыва туннеля. Не мигаем RunningState в false на
			// промежуточных состояниях — держим текущее, чтобы UI не дёргался.
			running = wasRunning
		case daemonpb.ServiceStatus_IDLE:
			running = false
		case daemonpb.ServiceStatus_FATAL:
			running = false
			msg := status.GetErrorMessage()
			debuglog.ErrorLog("daemon.supervisor: core FATAL: %s", msg)
			go b.retryAfterCoreFatal(msg)
		}
		ac.RunningState.Set(running)
		// SPEC 141 §7 (Windows; macOS — no-op): ядро не работает (idle,
		// fatal, откат) — снять свой прокси, в том числе после краша
		// лаунчера: первый кадр сверяет; снова started — вернуть прокси
		// последнего apply.
		switch status.GetStatus() {
		case daemonpb.ServiceStatus_IDLE, daemonpb.ServiceStatus_FATAL:
			ac.clearDaemonSystemProxy("the core is " + strings.ToLower(status.GetStatus().String()))
		case daemonpb.ServiceStatus_STARTED:
			if p := b.appliedProxyServer(); p != "" {
				ac.setDaemonSystemProxy(p)
			}
		}
		if running && !wasRunning {
			// Ядро поднялось (в т.ч. кем-то извне) — подтянуть список нод.
			debuglog.InfoLog("daemon: existing running core detected; attaching without restart")
			go func() {
				select {
				case <-time.After(2 * time.Second):
				case <-b.ctx.Done():
					return
				}
				ac.AutoLoadProxies()
			}()
		}
	}
}

// superviseLogs держит подписку SubscribeLog и наполняет кольцевой буфер.
// Тот же reconnect-паттерн, что у superviseStatus (сброс backoff — только
// после реально полученного кадра, см. комментарий там).
func (b *DaemonBackend) superviseLogs() {
	backoff := time.Second
	for {
		if b.ctx.Err() != nil {
			return
		}
		client, err := b.grpcClient()
		if err == nil {
			var stream grpc.ServerStreamingClient[daemonpb.Log]
			stream, err = client.SubscribeLog(b.ctx, &emptypb.Empty{})
			if err == nil && b.consumeLogStream(stream) {
				backoff = time.Second
			}
		}
		if err != nil {
			debuglog.DebugLog("daemon.logs: stream unavailable: %v", err)
		}
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// consumeLogStream читает стрим до обрыва; true — получен хотя бы один кадр.
func (b *DaemonBackend) consumeLogStream(stream grpc.ServerStreamingClient[daemonpb.Log]) bool {
	received := false
	for {
		batch, err := stream.Recv()
		if err != nil {
			debuglog.DebugLog("daemon.logs: stream closed: %v", err)
			return received
		}
		received = true
		b.logMu.Lock()
		if batch.GetReset_() {
			b.logLines = b.logLines[:0]
		}
		for _, message := range batch.GetMessages() {
			b.logLines = append(b.logLines, message.GetMessage())
		}
		if overflow := len(b.logLines) - daemonLogMaxLines; overflow > 0 {
			b.logLines = append(b.logLines[:0], b.logLines[overflow:]...)
		}
		b.logMu.Unlock()
	}
}

// CoreLogLines implements coreLogSource: хвост буфера логов ядра.
func (b *DaemonBackend) CoreLogLines(max int) []string {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	start := 0
	if max > 0 && len(b.logLines) > max {
		start = len(b.logLines) - max
	}
	out := make([]string, len(b.logLines)-start)
	copy(out, b.logLines[start:])
	return out
}

// ClashEndpoint implements clashEndpointSource. В daemon-режиме Clash API
// вырезан из конфига (prepareConfigForDaemon), потому что управление, ноды и
// трафик идут по gRPC. Поэтому Clash-эндпоинта нет: возвращаем ok=false, и
// UI-слой Clash (Test API, traffic-поллер по /connections) в daemon-режиме
// не активируется. Метод оставлен как явный сигнал «Clash недоступен», а не
// удалён, чтобы профайлер мог отличить daemon-режим от «Clash просто выключен
// в classic-конфиге».
func (b *DaemonBackend) ClashEndpoint() (baseURL, token string, ok bool) {
	return "", "", false
}

// PoolSlots implements poolSource через lx-RPC GetPool.
func (b *DaemonBackend) PoolSlots(group string) ([]PoolSlotInfo, error) {
	client, err := b.grpcClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(b.ctx, daemonRPCTimeout)
	defer cancel()
	// A daemon without the pool RPC has no pool to report. Gated so the answer
	// is a capability rather than a wrapped gRPC string.
	b.ensureProbed()
	if !b.caps.supported(rpcGetPool) {
		return nil, services.NewProxyCapabilityError(services.CapabilityList)
	}
	pool, err := client.GetPool(ctx, &daemonpb.GetPoolRequest{GroupTag: group})
	if err != nil {
		if isUnimplemented(err) {
			return nil, services.NewProxyCapabilityError(services.CapabilityList)
		}
		return nil, fmt.Errorf("daemon GetPool: %w", err)
	}
	slots := make([]PoolSlotInfo, 0, len(pool.GetSlots()))
	for _, slot := range pool.GetSlots() {
		slots = append(slots, PoolSlotInfo{Slot: slot.GetSlot(), Tag: slot.GetTag(), Delay: slot.GetDelay()})
	}
	return slots, nil
}

// Chains implements chainSource через lx-RPC GetChains.
func (b *DaemonBackend) Chains() ([]ChainInfo, error) {
	client, err := b.grpcClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(b.ctx, daemonRPCTimeout)
	defer cancel()
	b.ensureProbed()
	if !b.caps.supported(rpcGetChains) {
		return nil, services.NewProxyCapabilityError(services.CapabilityList)
	}
	list, err := client.GetChains(ctx, &emptypb.Empty{})
	if err != nil {
		if isUnimplemented(err) {
			return nil, services.NewProxyCapabilityError(services.CapabilityList)
		}
		return nil, fmt.Errorf("daemon GetChains: %w", err)
	}
	return chainInfosFromPB(list.GetChains()), nil
}

// ProbeLayer implements chainSource через тот же URLTestOutbound, что и
// обычный пинг узла: бюджет и эндпоинт общие, иначе цифра слоя была бы
// несопоставима с колонкой Delay в списке.
func (b *DaemonBackend) ProbeLayer(chainTag string, pos int) (int64, string, error) {
	client, err := b.grpcClient()
	if err != nil {
		return 0, "", err
	}
	// Дедлайн вызова с запасом над бюджетом теста: первый режет проба
	// внутри ядра, второй страхует от повисшего RPC.
	ctx, cancel := context.WithTimeout(b.ctx, chainProbeCallTimeout())
	defer cancel()
	b.ensureProbed()
	if !b.caps.supported(rpcURLTestOutbound) {
		return 0, "", services.NewProxyCapabilityError(services.CapabilityTest)
	}
	resp, err := client.URLTestOutbound(ctx, &daemonpb.URLTestOutboundRequest{
		OutboundTag: chainProbeTag(chainTag, pos),
		Link:        api.GetPingTestURL(),
		Timeout:     uint32(api.GetPingTestTimeoutMs()),
	})
	if err != nil {
		if isUnimplemented(err) {
			return 0, "", services.NewProxyCapabilityError(services.CapabilityTest)
		}
		return 0, "", fmt.Errorf("daemon URLTestOutbound: %w", err)
	}
	return int64(resp.GetDelay()), resp.GetError(), nil
}

// SetPositionEnabled implements chainSource через lx-RPC
// SetChainPositionEnabled (SPEC 075 ядра).
//
// warmupError возвращается ПЕРВЫМ результатом рядом с nil-ошибкой: ядро
// применяет флаг всегда, а неудачный прогрев звена сообщает данными.
// Свалить это в error значило бы сказать «не переключилось», хотя
// переключилось — и следующий GetChains показал бы обратное.
func (b *DaemonBackend) SetPositionEnabled(chainTag string, pos int, enabled bool) (string, error) {
	client, err := b.grpcClient()
	if err != nil {
		return "", err
	}
	// Бюджет как у пробы, а не общий daemonRPCTimeout: включение позиции
	// поднимает звено (WG-хендшейк, TLS), и это укладывается в секунды.
	ctx, cancel := context.WithTimeout(b.ctx, chainProbeCallTimeout())
	defer cancel()
	resp, err := client.SetChainPositionEnabled(ctx, &daemonpb.SetChainPositionEnabledRequest{
		ChainTag: chainTag,
		Position: int32(pos),
		Enabled:  enabled,
	})
	if err != nil {
		if isUnimplemented(err) {
			return "", ErrChainToggleUnsupported
		}
		return "", fmt.Errorf("daemon SetChainPositionEnabled: %w", err)
	}
	return resp.GetWarmupError(), nil
}

// --- gRPC-транспорт proxy-операций (Servers tab, tray, auto-load) --------

// daemonProxyTransport реализует services.ProxyTransport поверх gRPC:
// группы/выбор/URL-тест — те же RPC, что использует Android CommandClient.
type daemonProxyTransport struct {
	b *DaemonBackend
}

const daemonRPCTimeout = 15 * time.Second

func (t *daemonProxyTransport) rpc() (daemonpb.StartedServiceClient, context.Context, context.CancelFunc, error) {
	client, err := t.b.grpcClient()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(t.b.ctx, daemonRPCTimeout)
	return client, ctx, cancel, nil
}

// EndpointStatuses implements services.EndpointSource через GetOutbounds.
func (t *daemonProxyTransport) EndpointStatuses() (map[string]services.EndpointStatus, error) {
	client, ctx, cancel, err := t.rpc()
	if err != nil {
		return nil, err
	}
	defer cancel()
	// A daemon without GetOutbounds reports no endpoint state; that is a
	// capability, so callers get the sentinel rather than a gRPC string.
	t.b.ensureProbed()
	if !t.b.caps.supported(rpcGetOutbounds) {
		return nil, services.NewProxyCapabilityError(services.CapabilityList)
	}
	return services.EndpointStatusesRPC(ctx, client)
}

// SetEndpointEnabled implements services.EndpointSource через lx-RPC
// SetEndpointEnabled (SPEC 106 ядра). Бюджет как у пробы: включение будит
// устройство.
func (t *daemonProxyTransport) SetEndpointEnabled(tag string, enabled bool) (string, error) {
	client, err := t.b.grpcClient()
	if err != nil {
		return "", err
	}
	t.b.ensureProbed()
	if !t.b.caps.supported(rpcSetEndpointEnabled) {
		return "", services.NewProxyCapabilityError(services.CapabilitySwitch)
	}
	ctx, cancel := context.WithTimeout(t.b.ctx, chainProbeCallTimeout())
	defer cancel()
	return services.SetEndpointEnabledRPC(ctx, client, tag, enabled)
}

// GroupProxies implements services.ProxyTransport через GetGroups.
//
// GetGroups is declared in the launcher's proto but is NOT implemented by every
// daemon build: the method arrives with the fork's lx command surface, so a
// daemon built without it answers codes.Unimplemented. That is a capability
// fact, not a transient failure — retrying cannot help, and reporting it as an
// error produced a red banner reading
//
//	cannot read the proxies of group "…": daemon GetGroups: rpc error:
//	code = Unimplemented desc = unknown method GetGroups
//
// which tells the user nothing they can act on. Translating it to
// services.ErrProxyListUnsupported lets the backend answer "this engine cannot
// list proxies" and the UI explain it calmly instead.
func (t *daemonProxyTransport) GroupProxies(group string) ([]api.ProxyInfo, string, error) {
	// RPC is the daemon's native control plane: already paired, already mTLS,
	// needs no extra listener. It is therefore tried FIRST and is not second-
	// guessed by the fallback.
	//
	// Which method to use is a property of the SERVER, established once. Asking
	// the generated client's interface instead is what produced the original
	// "unknown method GetGroups" failure.
	t.b.ensureProbed()
	if !t.b.caps.supported(rpcGetGroups) {
		return t.b.groupProxiesViaFallback(group)
	}

	client, ctx, cancel, err := t.rpc()
	if err != nil {
		return nil, "", err
	}
	defer cancel()

	groups, err := t.groupsSnapshot(ctx, client)
	if err != nil {
		return nil, "", err
	}
	proxies, selected, ok := services.ProxyInfosFromGroups(groups, group)
	if !ok {
		return nil, "", fmt.Errorf("daemon: group %q not found", group)
	}
	return proxies, selected, nil
}

// groupsSnapshot reads the group list through the method the daemon has.
//
// GetGroups is the unary read. Where it is absent there is NO fallback to
// SubscribeGroups, and that is a measured decision, not an oversight:
//
//   - the live daemon that reported this bug answers `Unknown: invalid argument`
//     to SubscribeGroups(&emptypb.Empty{}), so the two do NOT take the same
//     request — the vendored subscription request type is not what that server
//     expects;
//   - even where a subscription existed, its first frame is a stream's opening
//     state, while this interface is request/response: silently reinterpreting
//     one as the other would invent a contract neither side agreed to.
//
// Reconstructing the server's expected request from a binary would be guessing
// at a private contract. So an engine without GetGroups is reported as unable to
// list proxies, and the UI explains that instead of failing — which is the
// honest outcome and the one this audit exists to produce.
func (t *daemonProxyTransport) groupsSnapshot(ctx context.Context, client daemonpb.StartedServiceClient) (*daemonpb.Groups, error) {
	if !t.b.caps.supported(rpcGetGroups) {
		return nil, services.NewProxyCapabilityError(services.CapabilityList)
	}
	g, err := client.GetGroups(ctx, &emptypb.Empty{})
	if err != nil {
		if isUnimplemented(err) {
			// The probe said yes and the call says no: trust the call, and treat
			// it as the capability it is rather than as a transient failure.
			return nil, services.NewProxyCapabilityError(services.CapabilityList)
		}
		return nil, fmt.Errorf("daemon GetGroups: %w", err)
	}
	return g, nil
}

// SwitchProxy implements services.ProxyTransport через SelectOutbound.
func (t *daemonProxyTransport) SwitchProxy(group, name string) error {
	t.b.ensureProbed()
	if !t.b.caps.supported(rpcSelectOutbound) {
		// On the measured daemon SelectOutbound IS supported, so this is the
		// rare branch; it exists so a future build that drops the RPC does not
		// lose switching merely because the list is served elsewhere.
		//
		// Consistency caveat: if list comes from the fallback and switch goes
		// over gRPC, both must observe the SAME running core. They do — both
		// address the one daemon process — but a mismatch would be a real bug,
		// which is why the hybrid consistency test exists.
		return t.b.switchProxyViaFallback(group, name)
	}
	client, ctx, cancel, err := t.rpc()
	if err != nil {
		return err
	}
	defer cancel()
	if _, err := client.SelectOutbound(ctx, &daemonpb.SelectOutboundRequest{GroupTag: group, OutboundTag: name}); err != nil {
		if isUnimplemented(err) {
			return services.NewProxyCapabilityError(services.CapabilitySwitch)
		}
		return fmt.Errorf("daemon SelectOutbound: %w", err)
	}
	return nil
}

// Delay implements services.ProxyTransport через lx-RPC URLTestOutbound —
// точечный URL-тест одного узла с granular cancel (SPEC 015 форка).
func (t *daemonProxyTransport) Delay(proxyName string) (int64, error) {
	return t.DelayContext(context.Background(), proxyName)
}

// DelayContext measures one outbound through lx-RPC URLTestOutbound under ctx.
//
// ctx carries RUN cancellation; the per-node budget travels in the request's
// Timeout field, exactly as the classic transport sends it in the query string.
// A cancelled run must stop promptly, while a merely slow node must still
// return an honest number — which is why the two are not merged.
func (t *daemonProxyTransport) DelayContext(ctx context.Context, proxyName string) (int64, error) {
	// Same RPC-first rule as the other two actions, and the same separation of
	// concerns: the latency scheduler above calls this and never learns whether
	// the number came from gRPC or HTTP.
	t.b.ensureProbed()
	if !t.b.caps.supported(rpcURLTestOutbound) {
		return t.b.delayViaFallback(ctx, proxyName)
	}
	client, err := t.b.grpcClient()
	if err != nil {
		return 0, err
	}
	// A daemon without the URL-test RPC cannot measure anything. Reported as the
	// LATENCY capability specifically, so an engine that lists fine but cannot
	// test keeps its node list usable instead of having the whole page disabled.
	t.b.ensureProbed()
	if !t.b.caps.supported(rpcURLTestOutbound) {
		return 0, services.NewProxyCapabilityError(services.CapabilityTest)
	}
	// Дедлайн вызова с запасом над бюджетом теста (как у ProbeLayer): при
	// бюджете выше daemonRPCTimeout медленный узел получал бы транспортную
	// «context deadline exceeded» вместо честной цифры.
	callTimeout := daemonRPCTimeout
	if probe := chainProbeCallTimeout(); probe > callTimeout {
		callTimeout = probe
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := client.URLTestOutbound(callCtx, &daemonpb.URLTestOutboundRequest{
		OutboundTag: proxyName,
		Link:        api.GetPingTestURL(),
		// Timeout — миллисекунды (uint32), в отличие от Interval у Subscribe*,
		// который в наносекундах (time.Duration).
		// Бюджет настраиваемый и единый во всех трёх транспортах: classic
		// GetDelay шлёт его же в query-параметре timeout.
		Timeout: uint32(api.GetPingTestTimeoutMs()),
	})
	if err != nil {
		if isUnimplemented(err) {
			return 0, services.NewProxyCapabilityError(services.CapabilityTest)
		}
		return 0, fmt.Errorf("daemon URLTestOutbound: %w", err)
	}
	if resp.GetError() != "" {
		return 0, fmt.Errorf("%s", resp.GetError())
	}
	return int64(resp.GetDelay()), nil
}

// Убедимся на компиляции, что транспорт реализует интерфейс.
var _ services.ProxyTransport = (*daemonProxyTransport)(nil)
