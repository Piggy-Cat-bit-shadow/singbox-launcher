package ui

import (
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"

	"singbox-launcher/core"
	"singbox-launcher/core/events"
	"singbox-launcher/internal/locale"
	"singbox-launcher/ui/components"
	"singbox-launcher/ui/design"
	"singbox-launcher/ui/icons"
)

// App manages the UI structure, its root shell and the retained tab strip.
//
// **Root content (SPEC 144).** `content` is the sidebar shell:
// `Border(left: sidebar, center: contentHost)`. `tabs` is a legacy
// compatibility object — it still owns `OnSelected`, which now delegates to
// `selectSection`, and `updateClashAPITabState` still touches it, but it is
// never placed in the visual tree. Only `GetContent()` decides what the window
// shows, and it returns `content`.
//
// `overlay` is the optional main-window click-redirect overlay
// (see `ui/wizard_overlay.go::wizardOverlayEnabled`). With the flag off
// (default) `overlay` stays nil and `content` remains the bare sidebar shell —
// clicks flow normally even while the configurator is open. With the flag on,
// `content` becomes `Stack(sidebar shell, overlay)`.
type App struct {
	window      fyne.Window
	core        *core.AppController
	tabs        *container.AppTabs
	clashAPITab *container.TabItem
	currentTab  *container.TabItem
	// localPanel / remotePanel — независимые списки прокси двух вкладок
	// (SPEC 098). Держим ссылки, чтобы отдавать разделяемые слоты UIService
	// активной панели при переключении вкладки.
	localPanel  *ProxyListPanel
	remotePanel *ProxyListPanel
	// refreshSettings перечитывает раздел Storage при входе в Settings
	// (SPEC 135 §4.1). Живёт здесь, потому что вызывается из навигации:
	// раньше — из app.tabs.OnSelected, теперь — из selectSection.
	refreshSettings func()
	// sidebar — навигационная колонка (SPEC 144). Заменяет горизонтальный
	// таб-стрип как основную навигацию, но исполняет ТУ ЖЕ логику выбора
	// через selectSection.
	sidebar *design.Sidebar
	// pages — содержимое страниц по presentation-маршруту.
	pages map[RouteID]fyne.CanvasObject
	// contentHost — контейнер, в который подставляется активная страница.
	// Один объект на все страницы: смена раздела не пересобирает дерево и не
	// сбрасывает состояние виджетов внутри страниц.
	contentHost *fyne.Container
	// currentSection — активный БИЗНЕС-домен (не маршрут). Нужен, чтобы
	// переключение внутри домена не переисполняло побочные эффекты выбора
	// раздела, а смена домена — исполняла их ровно один раз.
	currentSection SectionID
	// content — корень окна: сайдбар + contentHost (SPEC 144). Именно его
	// возвращает GetContent; AppTabs в дерево не попадает.
	content fyne.CanvasObject
	// overlay is a concrete ClickRedirect component from `ui/components`.
	// nil when `wizardOverlayEnabled` is false (current default), in which
	// case `content` is the sidebar shell itself.
	overlay *components.ClickRedirect

	// rejected — плашка «ядро выключило N серверов» на вкладке Local
	// (SPEC 132 §6.1, core_rejected_notice.go).
	rejected *coreRejectedNotice
	// windowShown — видно ли главное окно (не в трее). Ведём сами: диалог
	// предела за скрытым окном остановил бы цикл страховки навсегда.
	windowShown     bool
	windowVisibleMu sync.Mutex
}

// NewApp creates a new App instance
func NewApp(window fyne.Window, controller *core.AppController) *App {
	app := &App{
		window: window,
		core:   controller,
	}

	// SPEC 098: вкладки Local и Remote вместо Core и Servers. Обе
	// двухколоночные — слева список прокси, справа управление; см.
	// ui/local_remote_tabs.go.
	//
	// Local создаётся первой, чтобы её callback установился (внутри живёт
	// CreateCoreDashboardTab, который регистрирует UpdateCoreStatusFunc).
	// Emoji-in-label (💡 default emoji presentation) — colour rendering
	// via OS font fallback to Apple Color Emoji, matching sibling tabs
	// (⚙️ Settings / 🔍 Diagnostics).
	// Плашка страховки (SPEC 132 §6.1) живёт НАД списком узлов вкладки Local:
	// выключенные серверы — это про состав списка, и сообщение о них должно
	// стоять там же, где человек его увидит. Контейнер отдаём пустым и
	// скрытым — до первого выключения места он не занимает.
	localContent, localPanel := CreateLocalTab(controller, app.coreRejectedBar())
	remoteContent, remotePanel := CreateRemoteTab(controller)
	_ = remoteContent
	app.localPanel, app.remotePanel = localPanel, remotePanel
	// SPEC 100 §3.8: Debug API получает Connect/Disconnect вкладки Remote.
	// Строго после создания вкладок — подписчики OnOverrideChanged уже стоят.
	RegisterOverrideAPIHooks(controller)

	// Settings — обычная страница со своим содержимым.
	//
	// Раньше она была кнопкой-подделкой: пустая вкладка, чей OnSelected
	// открывал отдельное окно и тут же откатывал выбор назад. Это стоило
	// защиты от бесконечного цикла в обработчике и делало Settings
	// единственным пунктом строки, ведущим себя не как вкладка. Теперь
	// содержимое рендерится на месте, отдельное окно удалено за
	// ненадобностью.
	settingsContent, refreshSettings := BuildSettingsContent(controller)
	app.refreshSettings = refreshSettings
	// Страницы собраны заранее и живут в a.pages: переключение маршрута лишь
	// подставляет нужную, не пересобирая дерево и не теряя состояние
	// виджетов (позиция скролла, введённый текст).
	helpPage := buildAboutPage(controller)
	settingsPage := pageWithHeaderScroll(locale.T("Settings"),
		locale.T("Launcher preferences, subscriptions and data"), settingsContent)
	diagnosticsPage := pageWithHeaderScroll(locale.T("Diagnostics"),
		locale.T("Logs, maintenance and network checks"), CreateDiagnosticsTab(controller))

	// Home, Proxies и Traffic — новые presentation-страницы (SPEC 145).
	// Proxies переиспользует ТОТ ЖЕ объект панели списка, что и раньше:
	// его колбэки, виртуализация и состояние остаются нетронутыми.
	homePage := NewHomePage(controller, controller)
	proxiesPage := buildProxiesPage(localPanel)
	trafficPage := buildTrafficPage(controller)

	app.pages = map[RouteID]fyne.CanvasObject{
		RouteHome:        homePage.Object(),
		RouteProxies:     proxiesPage,
		RouteTraffic:     trafficPage,
		RouteRemote:      remoteContent,
		RouteDiagnostics: diagnosticsPage,
		RouteSettings:    settingsPage,
		RouteAbout:       helpPage,
	}

	// Совместимость: часть кода и тестов обращается к AppTabs напрямую
	// (updateClashAPITabState). Стрип остаётся построенным, но не попадает
	// в визуальное дерево — навигацию теперь несёт сайдбар.
	coreTabItem := container.NewTabItem(locale.T("Local"), localContent)
	app.clashAPITab = container.NewTabItem(locale.T("Remote"), remoteContent)
	settingsTabItem := container.NewTabItem(locale.T("Settings"), settingsPage)
	app.tabs = container.NewAppTabs(
		coreTabItem,
		app.clashAPITab,
		container.NewTabItem(locale.T("Diagnostics"), diagnosticsPage),
		settingsTabItem,
		container.NewTabItem(locale.T("Help"), helpPage),
	)

	// Дизайн-система не импортирует набор иконок напрямую (иконки — ресурс
	// приложения). Связываем их один раз здесь: шеврон карточек и строк.
	design.SetChevronResource(icons.ChevronRight)

	// Home получает тот же путь навигации, что и сайдбар: клик по сводке
	// ведёт на страницу через showRoute, а не через отдельную логику.
	homePage.SetNavigate(app.navigateTo)

	// Навигация: сайдбар — основная, и он исполняет ту же логику, что
	// раньше исполнял OnSelected (selectSection в ui/navigation.go).
	app.sidebar = design.NewSidebar(app.navigationEntries(), func(id design.NavID) {
		app.navigateTo(RouteID(id))
	})
	// AppTabs оставлен как программный путь и как страховка совместимости:
	// его обработчик вызывает ровно тот же selectSection.
	app.tabs.OnSelected = func(item *container.TabItem) {
		app.currentTab = item
		switch item {
		case coreTabItem:
			app.selectSection(SectionLocal)
		case app.clashAPITab:
			app.selectSection(SectionRemote)
		case settingsTabItem:
			app.selectSection(SectionSettings)
		}
	}

	// Сохраняем оригинальный callback, который был установлен в CreateCoreDashboardTab
	originalUpdateCoreStatusFunc := controller.UIService.UpdateCoreStatusFunc

	// refreshCoreStatus — статус ядра в нижней строке сайдбара.
	//
	// Раньше состояние показывал emoji в заголовке вкладки Local (▶️/⏸️).
	// Emoji-индикатор зависел от системного emoji-шрифта и в сайдбаре был бы
	// чужеродным, поэтому статус переехал в отдельную строку внизу колонки:
	// точка нужного цвета плюс текст. Цвет и текст берутся из темы и локали.
	//
	// Источник истины не меняется: RunningState контроллера. Здесь только
	// проекция его значения в presentation-слой.
	refreshCoreStatus := func() {
		running := controller.RunningState != nil && controller.RunningState.IsRunning()
		if running {
			app.sidebar.SetStatus(locale.T("Connected"), design.StatusSuccess)
			return
		}
		app.sidebar.SetStatus(locale.T("Disconnected"), design.StatusNeutral)
	}

	// Регистрируем комбинированный callback для обновления состояния вкладки Servers
	// (legacy путь UpdateCoreStatusFunc — сохраняем пока на нём висят
	// другие потребители: core_dashboard_tab.updateRunningStatus, etc.)
	controller.UIService.UpdateCoreStatusFunc = func() {
		// Вызываем оригинальный callback, если он есть
		if originalUpdateCoreStatusFunc != nil {
			originalUpdateCoreStatusFunc()
		}
		// Обновляем состояние вкладки Servers
		fyne.Do(func() {
			app.updateClashAPITabState()
		})
	}

	// Динамическая иконка Core подписывается на ТИПИЗИРОВАННЫЙ
	// EventBus (SPEC 047), а не на legacy UpdateCoreStatusFunc — это
	// канонический канал для cross-tab реакций на смену состояния
	// sing-box. Тот же канал слушает auto_update / proxy-active-changed
	// логика. Subscribe идемпотентен (одна handler-регистрация на NewApp).
	if controller.EventBus != nil {
		controller.EventBus.Subscribe(events.VpnStateChanged, func(_ events.Event) {
			fyne.Do(func() {
				refreshCoreStatus()
				// Home показывает состояние ядра: обновляем его вместе со
				// статусом сайдбара, из того же события. Отдельного
				// источника истины у страницы нет.
				homePage.Refresh()
			})
		})

		// Направление, добавленное в визарде, приезжает в config.json
		// только при пересборке, а выпадашка «Selector group» читает файл
		// один раз — на подключении к API. Поэтому новая группа не
		// появлялась в списке, пока пользователь не перезапустит
		// приложение, а удалённая продолжала висеть.
		//
		// `ConfigBuilt{OK:true}` — момент, когда пересобранный config.json
		// прошёл проверку ядром и записан на диск: ровно тогда набор
		// selector-групп в файле стал новым. Берём его, а не
		// `restart_dirty_cleared`: тот публикуется только при ПЕРЕХОДЕ
		// флага из dirty, и пересборка при уже чистом флаге прошла бы мимо.
		//
		// Только Local: у remote-панели свой источник групп (gRPC к
		// демону), и локальный config.json ей не собеседник.
		controller.EventBus.Subscribe(events.ConfigBuilt, func(e events.Event) {
			payload, ok := e.Payload.(events.ConfigBuiltPayload)
			if !ok {
				return
			}
			// SPEC 132 §6.1: плашка показывается по ЛЮБОМУ исходу сборки, у
			// которого есть выключенные. При OK:false цикл мог выключить
			// несколько узлов и упереться в ошибку не про узел — человек
			// обязан узнать о выключенных и этим путём тоже (§10.4).
			if len(payload.DisabledNodes) > 0 {
				disabled := payload.DisabledNodes
				fyne.Do(func() { app.showCoreRejected(disabled) })
			}
			if !payload.OK {
				return
			}
			fyne.Do(func() {
				if app.localPanel != nil {
					app.localPanel.ReloadGroups()
				}
			})
		})
	}

	// SPEC 064: подписка на remote-override changes. Set/Clear из
	// gear-dialog'а в Servers tab → tab немедленно re-enable / re-disable.
	// Listener тонкий: только trigger UI refresh через fyne.Do.
	OnOverrideChanged(func() {
		fyne.Do(app.updateClashAPITabState)
	})

	// Собираем оболочку: навигация слева, активная страница справа.
	//
	// Страницы созданы заранее и живут в a.pages; contentHost лишь
	// подставляет нужную. Так переход не пересобирает дерево виджетов и не
	// теряет их состояние (позиция скролла, введённый текст, раскрытые
	// секции) — иначе каждое переключение раздела выглядело бы как
	// перезагрузка страницы.
	app.contentHost = container.NewStack()
	app.content = container.NewBorder(nil, nil, app.sidebar, nil, app.contentHost)
	// Первая страница — Home (маршрут локального домена). Побочные эффекты
	// выбора домена исполняет showRoute: ровно те же, что раньше исполнял
	// OnSelected, и ровно один раз.
	app.currentSection = ""
	app.showRoute(RouteHome)

	// Local открыта на старте, но её слоты UIService перетёр конструктор
	// Remote (панели строятся обе, а слот один). Возвращаем владение той
	// панели, которая реально на экране, — иначе первый же авто-пинг или
	// ResetAPIState ушёл бы в невидимый список.
	app.localPanel.Activate(controller)

	// Авто-обновление Remote: на старте открыта Local, поэтому вкладка
	// неактивна, а окно — видимо (режим -tray скроет его сам, дёрнув
	// OnWindowHidden). Тикер поднимется при первом заходе на Remote.
	app.remotePanel.AutoRefresh().SetWindowVisible(true)
	app.localPanel.EndpointPoll().SetWindowVisible(true)
	app.remotePanel.EndpointPoll().SetWindowVisible(true)
	app.localPanel.EndpointPoll().SetTabActive(true)
	app.localPanel.RefreshEndpointStates(controller)
	app.setWindowVisible(true)
	// Пока окно в трее, обновлять нечего: данные никто не видит, а запросы
	// продолжали бы будить машину.
	if controller.UIService != nil {
		prevShown := controller.UIService.OnWindowShown
		controller.UIService.OnWindowShown = func() {
			if prevShown != nil {
				prevShown()
			}
			app.remotePanel.AutoRefresh().SetWindowVisible(true)
			app.localPanel.EndpointPoll().SetWindowVisible(true)
			app.remotePanel.EndpointPoll().SetWindowVisible(true)
			// Тот же признак нужен страховке: диалог предела за скрытым окном
			// остановил бы её цикл навсегда (SPEC 132 §6.2).
			app.setWindowVisible(true)
		}
		prevHidden := controller.UIService.OnWindowHidden
		controller.UIService.OnWindowHidden = func() {
			if prevHidden != nil {
				prevHidden()
			}
			app.remotePanel.AutoRefresh().SetWindowVisible(false)
			app.localPanel.EndpointPoll().SetWindowVisible(false)
			app.remotePanel.EndpointPoll().SetWindowVisible(false)
			app.setWindowVisible(false)
		}
	}

	// Страховка «ядро отвергло узел» получает свои UI-колбэки (SPEC 132
	// волна 5): строку хода и диалог предела. До этого момента все входы
	// вели себя как фоновые — шли молча до жёсткого потолка.
	app.installCoreRejectHooks(controller)

	// Инициализируем состояние вкладки + первичный рендер статуса ядра.
	// EventBus.Subscribe не fires backfill — рендерим вручную для startup'а.
	app.updateClashAPITabState()
	refreshCoreStatus()

	// Инициализируем overlay для перенаправления кликов на визард.
	// Поведение зависит от `wizardOverlayEnabled` (см. ui/wizard_overlay.go) —
	// по дефолту выключено, главное окно работает параллельно с визардом.
	InitWizardOverlay(app, controller)

	// Main-window keyboard shortcuts for power users — matches the
	// right-click menu on the Update button (core_dashboard_tab.go).
	// Modifier is ShortcutDefault which maps to Super on macOS, Control on
	// Linux/Windows. Registered on the Canvas so they fire regardless of
	// which tab has focus, unless a text field is actively consuming input.
	app.registerShortcuts()

	return app
}

// registerShortcuts wires keyboard accelerators for the most common daily
// power-user actions: reconnect sing-box, update subscriptions.
func (a *App) registerShortcuts() {
	if a.window == nil || a.window.Canvas() == nil {
		return
	}
	reconnect := &desktop.CustomShortcut{KeyName: fyne.KeyR, Modifier: fyne.KeyModifierShortcutDefault}
	a.window.Canvas().AddShortcut(reconnect, func(fyne.Shortcut) {
		core.KillSingBoxForRestart()
	})
	updateSubs := &desktop.CustomShortcut{KeyName: fyne.KeyU, Modifier: fyne.KeyModifierShortcutDefault}
	a.window.Canvas().AddShortcut(updateSubs, func(fyne.Shortcut) {
		core.RunParserProcess()
	})
	// Cmd/Ctrl+P → ping-all. Bound to the same hook the power-resume path
	// uses (AutoPingAfterConnectFunc), so it works even when the Servers tab
	// isn't focused.
	pingAll := &desktop.CustomShortcut{KeyName: fyne.KeyP, Modifier: fyne.KeyModifierShortcutDefault}
	a.window.Canvas().AddShortcut(pingAll, func(fyne.Shortcut) {
		if a.core != nil && a.core.UIService != nil && a.core.UIService.AutoPingAfterConnectFunc != nil {
			a.core.UIService.AutoPingAfterConnectFunc()
		}
	})
}

// navigationEntries — плоская структура навигации (SPEC 145).
//
// Раньше здесь было дерево: два родителя (Local/Remote) с постоянными
// подпунктами. Визуально это читалось как админ-панель. Теперь один уровень
// пунктов, сгруппированных заголовками, а детализация уехала в page-local
// navigation (segmented control на самих страницах).
//
// Каждый пункт ведёт на реальный экран: Home/Proxies/Traffic — три
// presentation-маршрута внутри локального домена.
func (a *App) navigationEntries() []design.NavEntry {
	return []design.NavEntry{
		{ID: design.NavID(RouteHome), Title: locale.T("Home"), Icon: icons.NavHome, Section: locale.T("Home")},
		{ID: design.NavID(RouteProxies), Title: locale.T("Proxies"), Icon: icons.NavLocal, Section: locale.T("Network")},
		{ID: design.NavID(RouteRemote), Title: locale.T("Remote"), Icon: icons.NavRemote},
		{ID: design.NavID(RouteTraffic), Title: locale.T("Traffic"), Icon: icons.NavTraffic},
		{ID: design.NavID(RouteDiagnostics), Title: locale.T("Diagnostics"), Icon: icons.NavDiagnostics, Section: locale.T("Tools")},
		{ID: design.NavID(RouteSettings), Title: locale.T("Settings"), Icon: icons.NavSettings},
		{ID: design.NavID(RouteAbout), Title: locale.T("About"), Icon: icons.NavHelp, Pinned: true},
	}
}

// navigateTo — переход по навигации.
//
// Домен маршрута решает, нужно ли переисполнить побочные эффекты выбора
// раздела: Home → Proxies → Traffic остаются в одном домене и не трогают
// scope/транспорт повторно.
func (a *App) navigateTo(r RouteID) {
	a.showRoute(r)
}

// showRoute переключает видимую страницу.
func (a *App) showRoute(r RouteID) {
	page, ok := a.pages[r]
	if !ok {
		return
	}
	domain := routeDomain(r)
	if domain != "" && a.currentSection != domain {
		a.currentSection = domain
		a.selectSection(domain)
	}
	if a.contentHost != nil {
		a.contentHost.Objects = []fyne.CanvasObject{page}
		a.contentHost.Refresh()
	}
	if a.sidebar != nil {
		a.sidebar.SetSelected(design.NavID(r))
	}
}

// GetContent returns the root content for the main window: the sidebar shell
// (`content`), or sidebar shell + ClickRedirect overlay when
// `wizardOverlayEnabled` is on (see `ui/wizard_overlay.go`).
//
// The `a.tabs` fallback is unreachable in practice — NewApp always assigns
// `content` — and exists only so an App built without that assignment cannot
// return nil to SetContent. It is NOT the intended root: AppTabs is a
// compatibility object and must never be what the window shows (SPEC 144).
func (a *App) GetContent() fyne.CanvasObject {
	if a.content != nil {
		return a.content
	}
	return a.tabs
}

// updateClashAPITabState — SPEC 064 update: tab **всегда** доступна.
//
// Раньше (до SPEC 064) tab disable'илась когда локальный sing-box не запущен.
// Это создало chicken-and-egg: gear-кнопка для настройки remote-endpoint
// живёт ВНУТРИ этой вкладки, юзер не мог до неё добраться из cold-start
// состояния (local не стартован, override ещё не задан → tab disabled →
// gear недоступен → override никогда не задать).
//
// Решение: вкладка постоянно enabled. Если ни local sing-box, ни remote
// override не активны — refresh-логика покажет «Clash API offline» в
// ApiStatusLabel, но badge + gear остаются нажимаемыми, и юзер может
// настроить remote или запустить local.
//
// Функция оставлена в качестве no-op-stub: вызывается из множества мест
// в кодовой базе (UpdateCoreStatusFunc, EventBus subscriber, OnOverrideChanged
// listener). Удалять hook не имеет смысла — нет cost'а, и позволяет в
// будущем вернуть гейтинг если потребуется.
func (a *App) updateClashAPITabState() {
	if a.clashAPITab == nil || a.tabs == nil {
		return
	}
	// SPEC 064: всегда enabled. Никаких DisableItem'ов больше нет.
	a.tabs.EnableItem(a.clashAPITab)
}
