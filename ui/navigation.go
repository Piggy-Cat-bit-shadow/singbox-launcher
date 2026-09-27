package ui

import (
	"singbox-launcher/core/services"
)

// File navigation.go — выбор раздела главного окна (SPEC 144).
//
// **Почему это отдельный файл.** До SPEC 144 вся логика переключения
// страниц жила прямо в `app.tabs.OnSelected`. Пока навигация была
// горизонтальным таб-стрипом, это работало; но она не сводится к смене
// картинки. Смена раздела — это исполнение последовательности побочных
// эффектов: область API, владелец разделяемых слотов UIService, транспорт
// движка, активность авто-обновления и поллинга, перечитывание настроек и,
// наконец, обновление списка.
//
// Переносить это в новый сайдбар «по мотивам» нельзя: порядок шагов значим
// (комментарии в исходном OnSelected объясняют, почему область ставится
// раньше снятия транспорта, и почему состояния узлов обновляются уже после
// смены транспорта). Поэтому тело обработчика переехало сюда **дословно**,
// а `AppTabs` и новый сайдбар вызывают один и тот же `selectSection`.
// Второй реализации навигации не существует — это главное требование
// SPEC 144 §86.
//
// `selectSection` не создаёт и не уничтожает виджеты: он только приводит
// состояние приложения в соответствие выбранному разделу, ровно как раньше
// делал обработчик вкладки.

// SectionID — идентификатор раздела верхнего уровня.
type SectionID string

const (
	// SectionLocal — своё ядро.
	SectionLocal SectionID = "local"
	// SectionRemote — удалённые машины.
	SectionRemote SectionID = "remote"
	// SectionDiagnostics — логи и обслуживание.
	SectionDiagnostics SectionID = "diagnostics"
	// SectionSettings — настройки лаунчера.
	SectionSettings SectionID = "settings"
	// SectionHelp — о программе.
	SectionHelp SectionID = "help"
)

// RouteID — presentation-маршрут (SPEC 145).
//
// **Зачем отдельный тип.** Раздел (`SectionID`) — это бизнес-домен: он решает,
// с каким ядром идёт разговор (Local/Remote), и смена домена влечёт
// переключение scope, транспорта и владельца слотов UIService. Маршрут — это
// только то, что видит пользователь в навигации.
//
// Разделение позволяет показывать Home / Proxies / Traffic как три разных
// пункта, оставаясь внутри одного домена: переключение между ними НЕ
// переисполняет побочные эффекты домена (иначе — лишние запросы, мигание
// списка и повторный опрос узлов на ровном месте).
type RouteID string

const (
	// RouteHome — дашборд локального ядра.
	RouteHome RouteID = "home"
	// RouteProxies — список узлов локального ядра.
	RouteProxies RouteID = "proxies"
	// RouteTraffic — профилировщик трафика.
	RouteTraffic RouteID = "traffic"
	// RouteRemote — удалённые машины.
	RouteRemote RouteID = "remote"
	// RouteDiagnostics — логи и обслуживание.
	RouteDiagnostics RouteID = "diagnostics"
	// RouteSettings — настройки лаунчера.
	RouteSettings RouteID = "settings"
	// RouteAbout — о программе.
	RouteAbout RouteID = "about"
)

// routeDomain возвращает бизнес-домен маршрута.
//
// Пустая строка означает «домен не меняется»: Diagnostics, Settings и About
// не должны трогать ни scope, ни транспорт — они лишь показывают свою
// страницу поверх текущего состояния.
func routeDomain(r RouteID) SectionID {
	switch r {
	case RouteHome, RouteProxies, RouteTraffic:
		return SectionLocal
	case RouteRemote:
		return SectionRemote
	default:
		return ""
	}
}

// Подразделы внутри страниц.
//
// Каждый из них соответствует реальному блоку разметки соответствующей
// страницы (см. PLAN 144, инвентарь): пункты существуют потому, что
// содержимое есть, — а не наоборот. Подраздел не является отдельной
// страницей: он открывает ту же страницу, где живёт его блок.
const (
	sectionLocalOverview SectionID = "local.overview"
	sectionLocalProxies  SectionID = "local.proxies"
	sectionLocalTraffic  SectionID = "local.traffic"

	sectionRemoteMachines SectionID = "remote.machines"
	sectionRemoteProxies  SectionID = "remote.proxies"

	sectionSettingsConnection    SectionID = "settings.connection"
	sectionSettingsSubscriptions SectionID = "settings.subscriptions"
	sectionSettingsLanguage      SectionID = "settings.language"
	sectionSettingsStorage       SectionID = "settings.storage"
)

// sectionPage возвращает страницу, которой принадлежит раздел (включая
// подразделы).
func sectionPage(id SectionID) SectionID {
	switch id {
	case sectionLocalOverview, sectionLocalProxies, sectionLocalTraffic:
		return SectionLocal
	case sectionRemoteMachines, sectionRemoteProxies:
		return SectionRemote
	case sectionSettingsConnection, sectionSettingsSubscriptions,
		sectionSettingsLanguage, sectionSettingsStorage:
		return SectionSettings
	default:
		return id
	}
}

// selectSection — смена БИЗНЕС-домена: с каким ядром идёт разговор.
//
// Отвечает только за scope, владельца слотов панелей и транспорт. Порядок
// шагов значим и унаследован от `app.tabs.OnSelected`: сначала область, потом
// активация панели и транспорт, иначе обновление списка уйдёт не в ту область.
//
// Видимость страниц (какие опросы активны, нужно ли перечитать Settings)
// сюда НЕ входит — этим занимается applyRouteVisibility. Смешивать нельзя:
// Diagnostics/Settings/About не меняют домен, но обязаны выключать опросы
// скрытых страниц.
func (a *App) selectSection(id SectionID) {
	controller := a.core

	switch id {
	case SectionLocal:
		// Порядок важен: сначала область, потом снятие транспорта. Оба
		// шага дёргают обновление списка, и оно должно писать уже в
		// local-состояние, а не в remote.
		if controller.APIService != nil {
			controller.APIService.SetProxyScope(services.ScopeLocal)
		}
		a.localPanel.Activate(controller)
		// Транспорт МАШИНЫ не снимаем: соединение — состояние самой машины,
		// а не вкладки. Рвать его при взгляде на своё ядро значит заставлять
		// жать Connect после каждого переключения. Связь разрывает только
		// явный Disconnect (или удаление машины).
		//
		// Чтобы Local при этом говорил со СВОИМ ядром, ставим его транспорт:
		// SetTransport(nil) означал бы «никакого», а в lxd-режиме Clash HTTP
		// нет — панель падала бы в «connection refused» на 9190.
		controller.RestoreOwnTransport()
	case SectionRemote:
		if controller.APIService != nil {
			controller.APIService.SetProxyScope(services.ScopeRemote)
		}
		// Возвращаем транспорт выбранной машины: пока смотрели Local, его
		// место занимал транспорт своего движка. Само соединение никуда не
		// девалось — снимает его только явный Disconnect.
		ReapplyLxdRemoteTransport(controller)
		a.remotePanel.Activate(controller)
	}

}

// applyRouteVisibility — слой ВИДИМОСТИ: что должно работать на текущей
// странице, независимо от бизнес-домена.
//
// Вызывается на каждый переход маршрута, включая переходы между страницами
// без домена (Diagnostics / Settings / About) — именно этот случай раньше
// выпадал: routeDomain возвращал "", selectSection не вызывался, и опросы
// скрытых страниц продолжали работать, а Settings не перечитывал Storage.
//
// Транспорт здесь не трогается: соединение с удалённой машиной — состояние
// машины, а не страницы.
func (a *App) applyRouteVisibility(route RouteID) {
	controller := a.core

	// Опросы узлов нужны только там, где список узлов виден.
	localListVisible := route == RouteProxies
	remoteListVisible := route == RouteRemote

	a.localPanel.EndpointPoll().SetTabActive(localListVisible)
	a.remotePanel.EndpointPoll().SetTabActive(remoteListVisible)
	// Автообновление удалённого списка — только на его странице.
	a.remotePanel.AutoRefresh().SetTabActive(remoteListVisible)

	switch route {
	case RouteProxies:
		a.localPanel.RefreshEndpointStates(controller)
	case RouteRemote:
		a.remotePanel.RefreshEndpointStates(controller)
	case RouteSettings:
		// Пути раздела Storage: ядро могли скачать, версия могла стать
		// известной — перечитываем при входе.
		if a.refreshSettings != nil {
			a.refreshSettings()
		}
	}

	// Обновляем список только там, где есть с кем разговаривать. На Remote
	// собеседник появляется лишь после выбора машины: без него запрос уходил
	// с пустой группой. Пустой список до выбора — честное состояние.
	needRefresh := route == RouteProxies
	if route == RouteRemote {
		_, _, hasMachine := GetLxdRemoteOverride()
		needRefresh = hasMachine
	}
	if needRefresh && controller.UIService != nil && controller.UIService.RefreshAPIFunc != nil {
		controller.UIService.RefreshAPIFunc()
	}
}
