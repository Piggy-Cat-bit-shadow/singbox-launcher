package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"singbox-launcher/api"
	"singbox-launcher/core/config"
	"singbox-launcher/internal/ctxutil"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

// ProxyScope — чьи прокси описывает состояние: своё ядро или удалённая
// машина (SPEC 098).
type ProxyScope int

const (
	// ScopeLocal — локальное ядро (вкладка Local).
	ScopeLocal ProxyScope = iota
	// ScopeRemote — выбранная удалённая машина (вкладка Remote).
	ScopeRemote
)

// proxyScopeState — состояние списка прокси ОДНОЙ области.
//
// Всё, что зависит от того, чьё ядро мы сейчас показываем: сам список, выбор
// группы, активный узел, запомненные выборы и ошибки пинга. Хранится по
// экземпляру на область, поэтому Local и Remote не видят данных друг друга.
type proxyScopeState struct {
	SelectedClashGroup string
	ProxiesList        []api.ProxyInfo
	ActiveProxyName    string
	SelectedIndex      int
	// LastSelectedProxyByGroup maps selector group -> last selected proxy name.
	// This allows remembering the last proxy per selector (group) independently.
	LastSelectedProxyByGroup map[string]string
	// LastPingError maps proxy name -> last ping error message (for tooltip when button shows "Error").
	//
	// Kept as a projection of Measurements so existing Fyne targets keep
	// compiling and behaving; new code should read Measurements, which carries
	// the delay and timestamp a bare error string cannot.
	LastPingError map[string]string
	// Measurements is the authoritative runtime latency state for this scope.
	//
	// Runtime only: never persisted. It exists because the core's own history is
	// not a reliable echo — a daemon URLTestOutbound returning 42 ms does not
	// guarantee the next group snapshot reports 42 ms, so a measured value that
	// is discarded and re-read can silently revert to "unknown".
	Measurements map[string]ProxyMeasurementState
}

// MeasurementStatus classifies how a latency measurement ended.
//
// The UI switches on these tokens instead of inspecting error strings, so
// Classic and Daemon — whose underlying error text differs completely — produce
// the same three row states.
type MeasurementStatus string

const (
	// MeasurementSuccess — the node answered; Delay is meaningful.
	MeasurementSuccess MeasurementStatus = "success"
	// MeasurementTimeout — the node did not answer within its budget.
	MeasurementTimeout MeasurementStatus = "timeout"
	// MeasurementFailed — the node answered with an error.
	MeasurementFailed MeasurementStatus = "failed"
	// MeasurementUnsupported — the engine cannot measure at all.
	MeasurementUnsupported MeasurementStatus = "unsupported"
	// MeasurementCancelled — the run was cancelled; the node was not judged.
	MeasurementCancelled MeasurementStatus = "cancelled"
)

// ProxyMeasurementState is one node's runtime latency result.
type ProxyMeasurementState struct {
	// Delay in ms. Meaningful only when Status is MeasurementSuccess; -1
	// otherwise, because 0 is a legitimate latency and must not double as
	// "no result".
	Delay int64
	// Status is the classification the UI colours and labels by.
	Status MeasurementStatus
	// Error is the technical reason, for a tooltip. Deliberately not the row
	// label: a node list showing full transport errors is unreadable.
	Error string
	// MeasuredAt is when the result was taken. Recorded for freshness; the UI
	// does not display it yet.
	MeasuredAt time.Time
	// Generation identifies the run that produced this result, so a late result
	// from a superseded run can be recognised and dropped.
	Generation uint64
	// EverSucceeded records that this node measured successfully at some point.
	// Lets a future UI show "last known good" without conflating it with the
	// CURRENT result, which is the mistake §20 warns against.
	EverSucceeded bool
	// LastSuccessDelay is the most recent successful value, or -1.
	LastSuccessDelay int64
}

func newProxyScopeState() *proxyScopeState {
	return &proxyScopeState{
		ProxiesList:              []api.ProxyInfo{},
		SelectedIndex:            -1,
		LastSelectedProxyByGroup: make(map[string]string),
		LastPingError:            make(map[string]string),
		Measurements:             make(map[string]ProxyMeasurementState),
	}
}

// APIService manages Clash API interactions and proxy list management.
// It encapsulates all API-related state and operations to reduce AppController complexity.
type APIService struct {
	// Clash API configuration
	BaseURL string
	Token   string
	Enabled bool
	// APIState — почему API включён или нет (SPEC 143): ok / no_auth /
	// missing / invalid / unreadable. Отличает «не настроен» от «настроен
	// без secret» и от «конфиг не разобрался», чтобы UI не показывал
	// «Config Error» для валидной конфигурации ядра.
	APIState api.APIConfigState
	// APIDetail — английская причина для лога и UI.
	APIDetail string

	// Auto-load state
	AutoLoadInProgress bool
	AutoLoadMutex      sync.Mutex
	// AutoLoadGeneration растёт при каждом новом запуске автозагрузки.
	//
	// Retry-цикл живёт до ~100 секунд. Без поколения смена группы во время
	// его работы приводила к тому, что поздний ответ СТАРОГО цикла
	// перезаписывал список уже выбранной группы: в UI оставались узлы от
	// предыдущей. Цикл сверяет поколение перед каждой попыткой и перед
	// записью результата, и молча выходит, если его вытеснил новый запуск.
	AutoLoadGeneration uint64

	// Proxy list state (protected by StateMutex)
	StateMutex sync.RWMutex

	// scope — какой области принадлежит состояние, читаемое сейчас через
	// геттеры: ScopeLocal (своё ядро) или ScopeRemote (выбранная машина).
	//
	// SPEC 098: у вкладок Local и Remote независимые списки. Раньше поля
	// состояния были одни на всё приложение, и вкладки затирали данные друг
	// друга: перейдя на Remote, пользователь видел узлы локального ядра, пока
	// не нажмёт Start на сервере. Разделение виджетов этого не лечило —
	// данные-то оставались общими.
	scope ProxyScope
	// scopes хранит состояние КАЖДОЙ области отдельно. Активная выбирается
	// полем scope; переключение вкладки меняет его, а не перезаписывает
	// данные.
	scopes map[ProxyScope]*proxyScopeState

	// Dependencies (passed from AppController)
	ConfigPath            string
	RunningStateIsRunning func() bool
	OnProxiesUpdated      func() // Called when proxies are updated
	OnProxySwitched       func() // Called when proxy is switched

	// transportOverride — альтернативный транспорт proxy-операций
	// (daemon-режим: gRPC к lxd). nil = классический Clash HTTP.
	// Protected by StateMutex.
	transportOverride ProxyTransport
	// verifiedEndpoint — источник Clash-эндпоинта, принадлежность которого СВОЁМУ ядру
	// доказана. nil у движка без такого понятия (классическое ядро: второго процесса,
	// с которым можно перепутать, не существует).
	//
	// Хранится как ФУНКЦИЯ, а не как значение: доказательство стареет, и запомненный
	// эндпоинт продолжал бы отвечать после истечения проверки.
	verifiedEndpoint func() ClashTransport
}

// NewAPIService creates and initializes a new APIService instance.
func NewAPIService(configPath string,
	runningStateIsRunning func() bool,
	onProxiesUpdated func(), onProxySwitched func()) (*APIService, error) {
	apiSvc := &APIService{
		ConfigPath:            configPath,
		RunningStateIsRunning: runningStateIsRunning,
		OnProxiesUpdated:      onProxiesUpdated,
		OnProxySwitched:       onProxySwitched,
		scope:                 ScopeLocal,
		scopes: map[ProxyScope]*proxyScopeState{
			ScopeLocal:  newProxyScopeState(),
			ScopeRemote: newProxyScopeState(),
		},
	}

	// Load Clash API configuration from config.json.
	//
	// Отсутствие секции и пустой secret — штатные состояния, а не ошибка:
	// ядро может отдавать API без аутентификации на петле (SPEC 143).
	cfg, err := api.LoadClashAPIConfig(configPath)
	apiSvc.APIState = cfg.State
	apiSvc.APIDetail = cfg.Detail
	if err != nil {
		debuglog.WarnLog("NewAPIService: Clash API config error: %v", err)
	}
	if cfg.Enabled() {
		apiSvc.BaseURL = cfg.BaseURL
		apiSvc.Token = cfg.Token
		apiSvc.Enabled = true
		if !cfg.RequiresAuth() {
			debuglog.InfoLog("NewAPIService: Clash API is unauthenticated (no secret) at %s", cfg.BaseURL)
		}
	} else {
		apiSvc.BaseURL = ""
		apiSvc.Token = ""
		apiSvc.Enabled = false
	}

	// Initialize SelectedClashGroup from config.
	//
	// Группа из локального config.json — свойство ЛОКАЛЬНОГО ядра, поэтому
	// заполняется только область ScopeLocal. У удалённой машины свои группы,
	// их отдаёт она сама (RemoteDaemonGroups).
	if apiSvc.Enabled {
		_, defaultSelector, err := config.GetSelectorGroupsFromConfig(configPath)
		if err != nil {
			debuglog.WarnLog("NewAPIService: Failed to get selector groups: %v", err)
			apiSvc.scopes[ScopeLocal].SelectedClashGroup = "proxy-out" // Default fallback
		} else {
			apiSvc.scopes[ScopeLocal].SelectedClashGroup = defaultSelector
			debuglog.DebugLog("NewAPIService: Initialized SelectedClashGroup: %s", defaultSelector)
		}
	}

	return apiSvc, nil
}

// stateLocked возвращает состояние активной области для ЧТЕНИЯ.
//
// Годится под RLock: обе области создаёт конструктор, поэтому обращение к
// карте здесь не мутирующее. Ленивое создание было бы записью под RLock, то
// есть гонкой на ровном месте — для записи есть mutableStateLocked.
//
// Пустое состояние возвращается только для APIService, собранного литералом
// (так делают тесты): читать из него можно, ронять приложение — нет.
func (apiSvc *APIService) stateLocked() *proxyScopeState {
	if st := apiSvc.scopes[apiSvc.scope]; st != nil {
		return st
	}
	return newProxyScopeState()
}

// mutableStateLocked возвращает состояние активной области для ЗАПИСИ,
// создавая его при необходимости. Вызывать только под StateMutex.Lock().
func (apiSvc *APIService) mutableStateLocked() *proxyScopeState {
	if apiSvc.scopes == nil {
		apiSvc.scopes = make(map[ProxyScope]*proxyScopeState, 2)
	}
	st := apiSvc.scopes[apiSvc.scope]
	if st == nil {
		st = newProxyScopeState()
		apiSvc.scopes[apiSvc.scope] = st
	}
	return st
}

// SetProxyScope переключает активную область (SPEC 098).
//
// Вызывается при смене вкладки: дальше все геттеры и сеттеры адресуют
// состояние ЭТОЙ области. Данные предыдущей остаются нетронутыми — вернувшись
// на вкладку, пользователь видит ровно то, что там было.
func (apiSvc *APIService) SetProxyScope(scope ProxyScope) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.scope = scope
}

// ProxyScopeOf возвращает активную область.
func (apiSvc *APIService) ProxyScopeOf() ProxyScope {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	return apiSvc.scope
}

// SelectedClashGroupIn / SetSelectedClashGroupIn адресуют КОНКРЕТНУЮ область,
// а не активную.
//
// Нужны конструкторам панелей: обе строятся на старте, когда активна
// ScopeLocal, и «активные» аксессоры записали бы группу удалённой машины в
// local-состояние, оставив remote пустым.
func (apiSvc *APIService) SelectedClashGroupIn(scope ProxyScope) string {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	if st := apiSvc.scopes[scope]; st != nil {
		return st.SelectedClashGroup
	}
	return ""
}

func (apiSvc *APIService) SetSelectedClashGroupIn(scope ProxyScope, group string) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	if apiSvc.scopes == nil {
		apiSvc.scopes = make(map[ProxyScope]*proxyScopeState, 2)
	}
	st := apiSvc.scopes[scope]
	if st == nil {
		st = newProxyScopeState()
		apiSvc.scopes[scope] = st
	}
	st.SelectedClashGroup = group
}

// ResetScope очищает состояние области — список, выбор и ошибки пинга.
//
// Нужен, когда область перестала быть осмысленной: машина отключена или
// снят её транспорт. Показывать при этом прежние узлы значило бы выдавать
// данные машины, с которой разговора уже нет.
func (apiSvc *APIService) ResetScope(scope ProxyScope) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	if apiSvc.scopes == nil {
		apiSvc.scopes = make(map[ProxyScope]*proxyScopeState, 2)
	}
	apiSvc.scopes[scope] = newProxyScopeState()
}

// SetProxiesList safely sets the proxies list with mutex protection.
func (apiSvc *APIService) SetProxiesList(proxies []api.ProxyInfo) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.mutableStateLocked().ProxiesList = proxies
}

// GetProxiesList safely gets a copy of the proxies list with mutex protection.
func (apiSvc *APIService) GetProxiesList() []api.ProxyInfo {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	// Return a copy to prevent external modifications
	result := make([]api.ProxyInfo, len(apiSvc.stateLocked().ProxiesList))
	copy(result, apiSvc.stateLocked().ProxiesList)
	return result
}

// SetActiveProxyName safely sets the active proxy name with mutex protection.
func (apiSvc *APIService) SetActiveProxyName(name string) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.mutableStateLocked().ActiveProxyName = name
}

// GetActiveProxyName safely gets the active proxy name with mutex protection.
func (apiSvc *APIService) GetActiveProxyName() string {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	return apiSvc.stateLocked().ActiveProxyName
}

// SetSelectedIndex safely sets the selected index with mutex protection.
func (apiSvc *APIService) SetSelectedIndex(index int) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.mutableStateLocked().SelectedIndex = index
}

// GetSelectedIndex safely gets the selected index with mutex protection.
func (apiSvc *APIService) GetSelectedIndex() int {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	return apiSvc.stateLocked().SelectedIndex
}

// SetLastSelectedProxyForGroup safely sets the last selected proxy name for a selector group with mutex protection.
func (apiSvc *APIService) SetLastSelectedProxyForGroup(group, name string) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	if apiSvc.mutableStateLocked().LastSelectedProxyByGroup == nil {
		apiSvc.mutableStateLocked().LastSelectedProxyByGroup = make(map[string]string)
	}
	apiSvc.mutableStateLocked().LastSelectedProxyByGroup[group] = name
}

// GetLastSelectedProxyForGroup safely gets the last selected proxy name for a selector group with mutex protection.
func (apiSvc *APIService) GetLastSelectedProxyForGroup(group string) string {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	if apiSvc.stateLocked().LastSelectedProxyByGroup == nil {
		return ""
	}
	return apiSvc.stateLocked().LastSelectedProxyByGroup[group]
}

// SetLastPingError stores the last ping error message for a proxy (for tooltip when button shows "Error").
//
// Wraps the measurement store so the legacy Fyne UI keeps working unchanged.
// It records only the error half; callers that have a real delay should use
// SetMeasurement instead, which is what keeps a measured value from being lost.
func (apiSvc *APIService) SetLastPingError(proxyName, errMsg string) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	if apiSvc.mutableStateLocked().LastPingError == nil {
		apiSvc.mutableStateLocked().LastPingError = make(map[string]string)
	}
	if errMsg == "" {
		delete(apiSvc.mutableStateLocked().LastPingError, proxyName)
	} else {
		apiSvc.mutableStateLocked().LastPingError[proxyName] = errMsg
	}
}

// SetMeasurement records the authoritative runtime result for a proxy.
//
// This is the write the latency flow is built on: the transport returns a
// number, the backend stores it here, and later reads overlay it. Nothing is
// discarded in the hope that the core will echo it back.
func (apiSvc *APIService) SetMeasurement(proxyName string, m ProxyMeasurementState) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.setMeasurementLocked(apiSvc.mutableStateLocked(), proxyName, m)
}

// RecordMeasurementIfNewer stores a measurement unless a NEWER one already holds the slot.
//
// The check and the write are one critical section, because a compare in the caller followed
// by a write here would be a TOCTOU: two replies arriving together would both see "no newer
// result" and both write, and the loser of the race would land last and win anyway.
//
// Ordering is by `Generation`, which is the identity of the REQUEST rather than of its
// arrival. Within one generation a later write wins, so a retry of the same request still
// refreshes the value.
//
// Returns false when the result was discarded as superseded, so the caller can say so.
func (apiSvc *APIService) RecordMeasurementIfNewer(proxyName string, m ProxyMeasurementState) bool {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()

	st := apiSvc.mutableStateLocked()
	if prev, seen := st.Measurements[proxyName]; seen && prev.Generation > m.Generation {
		// A HAND TEST IS PROTECTED FROM A SCHEDULED RUN OF THE SAME VINTAGE, NOT FOREVER.
		//
		// Hand-test generations sit far above every group-run id, so a raw comparison makes a
		// group run unable to EVER update a node the user once tested by hand. That is not
		// protection, it is a frozen readout: the node keeps showing the delay from a test the
		// user ran minutes or hours ago, including after the node has died, and — verified —
		// the "escape hatch" an earlier comment cited does not exist, because
		// `ClearMeasurements` has no caller in Go, Swift or the IPC surface.
		//
		// The intent was narrower and is worth stating: a group run that was ALREADY IN FLIGHT
		// when the user pressed "test" must not overwrite their fresh result. Once the
		// hand test is no longer fresh it is history, and a later group run reporting it is
		// strictly better information. So the hand-test advantage EXPIRES.
		if !handTestAdvantageExpired(prev, m) {
			return false
		}
	}
	apiSvc.setMeasurementLocked(st, proxyName, m)
	return true
}

// handTestAdvantageExpiresAfter is how long a hand test outranks a later group run.
//
// Long enough to cover every group run that could have been in flight when the user pressed
// "test" — a full group test is bounded by its own timeout and the per-node budget — and short
// enough that a stale number cannot masquerade as a current one for a whole session.
const handTestAdvantageExpiresAfter = 5 * time.Minute

// handTestAdvantageExpired reports whether a stored hand-test result has given up its
// precedence over the incoming measurement.
//
// Two conditions release it, and both are needed:
//
//   - the incoming result is a GROUP run (a low generation) and the stored one is a HAND test
//     (a high one) — the only pair where the generation spaces are not comparable as ages;
//   - and the stored result is older than handTestAdvantageExpiresAfter.
//
// `MeasuredAt` is the age, and a zero value means "unknown", which expires immediately: an
// absent timestamp cannot be shown to be fresh, and guessing "fresh" is what produces the
// permanently frozen row. Within the same space the plain comparison already orders correctly,
// so this function is not consulted.
func handTestAdvantageExpired(prev, incoming ProxyMeasurementState) bool {
	prevIsHandTest := prev.Generation >= singleTestGenerationFloor
	incomingIsGroupRun := incoming.Generation < singleTestGenerationFloor
	if !prevIsHandTest || !incomingIsGroupRun {
		return false
	}
	if prev.MeasuredAt.IsZero() {
		return true
	}
	return time.Since(prev.MeasuredAt) > handTestAdvantageExpiresAfter
}

// singleTestGenerationFloor is the low bound of the hand-test generation space.
//
// It mirrors `backend/service`'s `singleTestGenerationBase`. The constant is duplicated rather
// than imported because this package is the lower layer and is what must keep the two spaces
// ordered; a test in `core/services` pins the value, so lowering it in the scheduler cannot
// silently make a group id look like a hand test.
const singleTestGenerationFloor = uint64(1) << 62

// setMeasurementLocked applies a measurement to an already-locked state.
//
// Extracted so the guarded and unguarded entry points cannot drift apart: the carry-forward
// and error-map rules below are the substance of a measurement, and two copies of them would
// eventually disagree.
func (apiSvc *APIService) setMeasurementLocked(state *proxyScopeState, proxyName string, m ProxyMeasurementState) {
	if state.Measurements == nil {
		state.Measurements = make(map[string]ProxyMeasurementState)
	}
	if state.LastPingError == nil {
		state.LastPingError = make(map[string]string)
	}
	// Carry forward the last known good value: a node that measured 42 ms and
	// then timed out still has a meaningful last-success, but its CURRENT delay
	// is no longer 42 ms.
	prev, seen := state.Measurements[proxyName]
	if seen && prev.EverSucceeded {
		m.EverSucceeded = true
		m.LastSuccessDelay = prev.LastSuccessDelay
	}
	if m.Status == MeasurementSuccess {
		m.EverSucceeded = true
		m.LastSuccessDelay = m.Delay
	}
	if m.LastSuccessDelay == 0 && !m.EverSucceeded {
		m.LastSuccessDelay = -1
	}
	state.Measurements[proxyName] = m
	if m.Error == "" {
		delete(state.LastPingError, proxyName)
	} else {
		state.LastPingError[proxyName] = m.Error
	}
}

// GetMeasurement returns the recorded measurement for a proxy.
func (apiSvc *APIService) GetMeasurement(proxyName string) (ProxyMeasurementState, bool) {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	m, ok := apiSvc.stateLocked().Measurements[proxyName]
	return m, ok
}

// GetMeasurements returns a copy of the whole measurement map.
func (apiSvc *APIService) GetMeasurements() map[string]ProxyMeasurementState {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	src := apiSvc.stateLocked().Measurements
	out := make(map[string]ProxyMeasurementState, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// ClearMeasurements drops all recorded latency results for this scope.
//
// Called when the cache would be a lie rather than a stale truth: a config swap,
// a changed remote target, or an engine identity change means the old numbers
// describe nodes that may no longer exist.
func (apiSvc *APIService) ClearMeasurements() {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	st := apiSvc.mutableStateLocked()
	st.Measurements = make(map[string]ProxyMeasurementState)
	// THE ERROR MAP IS THE OTHER HALF OF THE SAME VIEW, SO IT IS CLEARED HERE TOO.
	//
	// `SetMeasurement` writes both, the UI reads both, and this function exists to discard
	// the results of a run. Clearing only the measurements left every row with no delay but
	// its previous error text still attached — the data gone and the complaint about it
	// present, which reads as "these nodes are broken" rather than "these results were
	// cleared". The user's only recourse would be to re-run the test they just dismissed.
	st.LastPingError = make(map[string]string)
}

// GetLastPingError returns the last ping error message for a proxy, or empty string.
func (apiSvc *APIService) GetLastPingError(proxyName string) string {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	if apiSvc.stateLocked().LastPingError == nil {
		return ""
	}
	return apiSvc.stateLocked().LastPingError[proxyName]
}

// GetSelectedClashGroup safely gets the selected Clash group.
func (apiSvc *APIService) GetSelectedClashGroup() string {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	return apiSvc.stateLocked().SelectedClashGroup
}

// SetSelectedClashGroup safely sets the selected Clash group.
func (apiSvc *APIService) SetSelectedClashGroup(group string) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.mutableStateLocked().SelectedClashGroup = group
}

// GetClashAPIConfig safely gets Clash API configuration.
func (apiSvc *APIService) GetClashAPIConfig() (baseURL, token string, enabled bool) {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	return apiSvc.BaseURL, apiSvc.Token, apiSvc.Enabled
}

// ReloadClashAPIConfig reloads Clash API configuration from config.json file.
// This should be called when config might have changed (e.g., after wizard updates).
func (apiSvc *APIService) ReloadClashAPIConfig() error {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()

	debuglog.InfoLog("ReloadClashAPIConfig: Reloading Clash API configuration from config.json...")

	// Load Clash API configuration from config.json
	cfg, err := api.LoadClashAPIConfig(apiSvc.ConfigPath)
	apiSvc.APIState = cfg.State
	apiSvc.APIDetail = cfg.Detail
	if err != nil {
		debuglog.WarnLog("ReloadClashAPIConfig: Clash API config error: %v", err)
	}
	if cfg.Enabled() {
		oldEnabled := apiSvc.Enabled
		apiSvc.BaseURL = cfg.BaseURL
		apiSvc.Token = cfg.Token
		apiSvc.Enabled = true
		debuglog.InfoLog("ReloadClashAPIConfig: Successfully reloaded - BaseURL: %s, auth: %v, Enabled: %v (was %v)",
			cfg.BaseURL, cfg.RequiresAuth(), true, oldEnabled)
	} else {
		debuglog.WarnLog("ReloadClashAPIConfig: Clash API unavailable (%s): %s", cfg.State, cfg.Detail)
		apiSvc.BaseURL = ""
		apiSvc.Token = ""
		apiSvc.Enabled = false
		// Локальный config.json описывает ЛОКАЛЬНОЕ ядро — правим только его
		// область. Через активную область перезагрузка, сделанная пока открыта
		// вкладка Remote, затёрла бы группу удалённой машины.
		apiSvc.scopes[ScopeLocal].SelectedClashGroup = ""
		if err != nil {
			return fmt.Errorf("failed to reload Clash API config: %w", err)
		}
	}

	// Reload SelectedClashGroup from config if API is enabled
	if apiSvc.Enabled {
		_, defaultSelector, err := config.GetSelectorGroupsFromConfig(apiSvc.ConfigPath)
		if err != nil {
			debuglog.WarnLog("ReloadClashAPIConfig: Failed to get selector groups: %v", err)
			// Keep existing SelectedClashGroup if we can't read new one
		} else {
			apiSvc.scopes[ScopeLocal].SelectedClashGroup = defaultSelector
			debuglog.DebugLog("ReloadClashAPIConfig: Updated SelectedClashGroup: %s", defaultSelector)
		}
	}

	return nil
}

// AutoLoadProxies attempts to load proxies across 14 retry attempts with
// escalating back-off (1,3,3, 5×5, 10×4, 15×2 seconds). Runs at most one
// instance at a time; the in-progress flag is always cleared on exit — the
// goroutine's defer covers every path, including the currentGroup=="" early
// return that previously leaked it (wedging auto-load until restart).
func (apiSvc *APIService) AutoLoadProxies(ctx context.Context) {
	// clearInProgress гасит флаг ТОЛЬКО если нас не вытеснил новый запуск —
	// иначе выходящий старый цикл сбросил бы флаг работающего нового.
	var myGenRef *uint64
	clearInProgress := func() {
		apiSvc.AutoLoadMutex.Lock()
		if myGenRef == nil || apiSvc.AutoLoadGeneration == *myGenRef {
			apiSvc.AutoLoadInProgress = false
		}
		apiSvc.AutoLoadMutex.Unlock()
	}

	// Новый запуск ВЫТЕСНЯЕТ предыдущий, а не отбрасывается сам.
	//
	// Раньше здесь стоял ранний return по AutoLoadInProgress, и смена группы
	// во время retry-цикла (до ~100 с) просто игнорировалась: пользователь
	// выбирал другую группу, а список продолжал жить от старой. Поколение
	// решает и это, и обратную гонку — поздний ответ вытесненного цикла.
	apiSvc.AutoLoadMutex.Lock()
	apiSvc.AutoLoadGeneration++
	myGen := apiSvc.AutoLoadGeneration
	apiSvc.AutoLoadInProgress = true
	apiSvc.AutoLoadMutex.Unlock()
	myGenRef = &myGen

	// isStale сообщает, вытеснен ли этот запуск более новым.
	isStale := func() bool {
		apiSvc.AutoLoadMutex.Lock()
		defer apiSvc.AutoLoadMutex.Unlock()
		return apiSvc.AutoLoadGeneration != myGen
	}

	if _, err := apiSvc.wireTransport(); err != nil {
		clearInProgress()
		debuglog.DebugLog("AutoLoadProxies: no proxy transport available (%v), skipping", err)
		return
	}

	selectedGroup := apiSvc.GetSelectedClashGroup()
	if selectedGroup == "" {
		clearInProgress()
		debuglog.DebugLog("AutoLoadProxies: No group selected, skipping")
		return
	}

	intervals := []time.Duration{1, 3, 3, 5, 5, 5, 5, 5, 10, 10, 10, 10, 15, 15}

	go func() {
		// Always release the in-progress flag, whichever path exits.
		defer clearInProgress()

		for attempt, interval := range intervals {
			// Check if context is cancelled
			select {
			case <-ctx.Done():
				debuglog.DebugLog("AutoLoadProxies: Stopped (context cancelled)")
				return
			default:
			}

			// Нас вытеснил более новый запуск (пользователь сменил группу) —
			// дальше работать бессмысленно, результат всё равно устарел.
			if isStale() {
				debuglog.DebugLog("AutoLoadProxies: Superseded by a newer run, stopping")
				return
			}

			if attempt > 0 {
				if err := ctxutil.SleepWithContext(ctx, interval*time.Second); err != nil {
					debuglog.DebugLog("AutoLoadProxies: Stopped during wait (context cancelled)")
					return
				}
			}

			// Раньше здесь стоял skip по RunningStateIsRunning(), и при любом
			// ядре, поднятом НЕ лаунчером, автозагрузка не выполнялась:
			// tracked PID = -1, флаг всегда false. Так ломалось переключение
			// групп и с чужим sing-box'ом (remote endpoint, SPEC 064), и с
			// локальным ядром, запущенным вручную, — API отвечал, а список
			// оставался от предыдущей группы.
			//
			// Отдельная проверка не нужна: недостижимый endpoint отсеется
			// ошибкой самого запроса ниже и уйдёт в следующий retry — тот же
			// результат, но без ложного пропуска.
			debuglog.DebugLog("AutoLoadProxies: Attempt %d/%d to load proxies for group '%s'", attempt+1, len(intervals), selectedGroup)

			// Get current group (it might have changed)
			currentGroup := apiSvc.GetSelectedClashGroup()

			if currentGroup == "" {
				debuglog.DebugLog("AutoLoadProxies: Group cleared, stopping attempts")
				return
			}

			if platform.IsSleeping() {
				debuglog.DebugLog("AutoLoadProxies: Skipping attempt - system sleeping")
				continue
			}

			// Транспорт строим на каждой попытке: endpoint/override могли
			// смениться пока retry-цикл спал.
			transport, terr := apiSvc.wireTransport()
			if terr != nil {
				debuglog.DebugLog("AutoLoadProxies: transport unavailable (%v)", terr)
				continue
			}

			// Try to load proxies
			proxies, now, err := transport.GroupProxies(currentGroup)
			if err != nil {
				if errors.Is(err, api.ErrPlatformInterrupt) {
					debuglog.DebugLog("AutoLoadProxies: Aborted (platform interrupt/sleep)")
				} else {
					debuglog.DebugLog("AutoLoadProxies: Attempt %d failed: %v", attempt+1, err)
				}
				continue
			}

			// Success - update proxies list.
			//
			// Поколение сверяем ЕЩЁ РАЗ перед записью: запрос летел по сети, и
			// за это время пользователь мог сменить группу. Без проверки
			// поздний ответ старого цикла затирал список уже выбранной группы.
			if isStale() {
				debuglog.DebugLog("AutoLoadProxies: Result for group '%s' dropped — superseded", currentGroup)
				return
			}
			// The GUI port owns thread marshalling: the headless backend
			// applies this directly, the Fyne frontend hops to its UI thread
			// inside the port implementation.
			if isStale() {
				return
			}
			apiSvc.SetProxiesList(proxies)
			apiSvc.SetActiveProxyName(now)

			if apiSvc.OnProxiesUpdated != nil {
				apiSvc.OnProxiesUpdated()
			}

			// Проверяем, есть ли сохраненный прокси для текущей группы, и переключаемся на него, если он отличается от текущего
			// Делаем это после обновления UI, чтобы не блокировать
			lastSelected := apiSvc.GetLastSelectedProxyForGroup(currentGroup)
			if lastSelected != "" && lastSelected != now {
				// Проверяем, что сохраненный прокси существует в списке
				proxyExists := false
				for _, proxy := range proxies {
					if proxy.Name == lastSelected {
						proxyExists = true
						break
					}
				}
				if proxyExists {
					debuglog.DebugLog("AutoLoadProxies: Switching to saved proxy '%s' (current: '%s') for group '%s'", lastSelected, now, currentGroup)
					// Переключаемся на сохраненный прокси (мы уже в goroutine, дополнительная не нужна)
					if err := apiSvc.SwitchProxy(currentGroup, lastSelected); err != nil {
						debuglog.WarnLog("AutoLoadProxies: Failed to switch to saved proxy '%s' for group '%s': %v", lastSelected, currentGroup, err)
						// Не критично, продолжаем с текущим прокси
					}
				}
			}

			debuglog.InfoLog("AutoLoadProxies: Successfully loaded %d proxies for group '%s' on attempt %d", len(proxies), currentGroup, attempt+1)
			return // Success, stop retrying
		}

		debuglog.WarnLog("AutoLoadProxies: All %d attempts failed", len(intervals))
	}()
}

// SwitchProxy switches to the specified proxy in the selected group.
func (apiSvc *APIService) SwitchProxy(group, proxyName string) error {
	transport, err := apiSvc.wireTransport()
	if err != nil {
		return err
	}

	if err := transport.SwitchProxy(group, proxyName); err != nil {
		debuglog.WarnLog("SwitchProxy: group=%q → %q failed: %v", group, proxyName, err)
		return fmt.Errorf("failed to switch proxy: %w", err)
	}
	// Логируем факт и адресата: без этого нельзя отличить «лаунчер не послал
	// команду» от «ядро её приняло, но соединения не разорвало» — а лечение у
	// этих случаев разное.
	debuglog.InfoLog("SwitchProxy: group=%q → %q", group, proxyName)

	apiSvc.SetActiveProxyName(proxyName)
	// Сохраняем последний выбранный прокси для текущей группы для автоматического переключения при следующем старте
	apiSvc.SetLastSelectedProxyForGroup(group, proxyName)

	// Notify about proxy switch
	if apiSvc.OnProxySwitched != nil {
		apiSvc.OnProxySwitched()
	}

	return nil
}
