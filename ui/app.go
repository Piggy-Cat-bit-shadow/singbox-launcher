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

// App manages the UI structure and tabs.
//
// `overlay` and `content` exist for the optional main-window click-redirect
// overlay (see `ui/wizard_overlay.go::wizardOverlayEnabled`). When the
// feature flag is off, `content == tabs` (bare passthrough) and `overlay`
// stays nil — clicks on the main window flow normally even while the
// configurator is open.
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
	// pages — содержимое страниц по идентификатору раздела.
	pages map[SectionID]fyne.CanvasObject
	// contentHost — контейнер, в который подставляется активная страница.
	// Один объект на все страницы: смена раздела не пересобирает дерево и не
	// сбрасывает состояние виджетов внутри страниц.
	contentHost *fyne.Container
	// currentSection — активный раздел. Единственный источник истины для
	// подсветки навигации; состояние ядра/машин здесь не хранится.
	currentSection SectionID
	content        fyne.CanvasObject
	// overlay is a concrete ClickRedirect component from `ui/components`.
	// nil when `wizardOverlayEnabled` is false (current default).
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
	// Каждая страница получает шапку с заголовком и подзаголовком
	// (SPEC 144): раньше заголовок нёс таб-стрип, и внутри страницы его не
	// было. Тело страницы прокручивается, шапка остаётся на месте — иначе
	// заголовок уезжал бы вместе с длинным содержимым.
	settingsPage := pageWithHeaderScroll(locale.T("Settings"),
		locale.T("Launcher preferences, subscriptions and data"), settingsContent)
	diagnosticsPage := pageWithHeaderScroll(locale.T("Diagnostics"),
		locale.T("Logs, maintenance and network checks"), CreateDiagnosticsTab(controller))
	helpPage := pageWithHeaderScroll(locale.T("Help"),
		locale.T("About this build and where to find us"), CreateHelpTab(controller))

	app.pages = map[SectionID]fyne.CanvasObject{
		SectionLocal:       localContent,
		SectionRemote:      remoteContent,
		SectionDiagnostics: diagnosticsPage,
		SectionSettings:    settingsPage,
		SectionHelp:        helpPage,
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

	// Навигация: сайдбар — основная, и он исполняет ту же логику, что
	// раньше исполнял OnSelected (selectSection в ui/navigation.go).
	app.sidebar = design.NewSidebar(app.navigationEntries(), func(id design.SidebarItemID) {
		app.navigateTo(SectionID(id))
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
			fyne.Do(refreshCoreStatus)
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
	// Первая страница — Local. Побочные эффекты её выбора исполняет
	// showSection ниже (ровно те же, что раньше исполнял OnSelected).
	app.currentSection = ""
	app.showSection(SectionLocal)

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

// navigationEntries — структура навигации (SPEC 144).
//
// Подпункты выведены из РЕАЛЬНЫХ разделов страниц, а не придуманы ради
// наполнения сайдбара: у каждой страницы ниже есть соответствующее
// содержимое. Пункты верхнего уровня ведут на страницу целиком (её первый
// подраздел), подпункты — на конкретную секцию внутри неё.
func (a *App) navigationEntries() []design.SidebarEntry {
	return []design.SidebarEntry{
		{
			ID:      design.SidebarItemID(SectionLocal),
			Title:   locale.T("Local"),
			Icon:    icons.NavLocal,
			Section: locale.T("Core"),
			Children: []design.SidebarEntry{
				{ID: design.SidebarItemID(sectionLocalOverview), Title: locale.T("Overview")},
				{ID: design.SidebarItemID(sectionLocalProxies), Title: locale.T("Proxies")},
				{ID: design.SidebarItemID(sectionLocalTraffic), Title: locale.T("Traffic")},
			},
		},
		{
			ID:    design.SidebarItemID(SectionRemote),
			Title: locale.T("Remote"),
			Icon:  icons.NavRemote,
			Children: []design.SidebarEntry{
				{ID: design.SidebarItemID(sectionRemoteMachines), Title: locale.T("Machines")},
				{ID: design.SidebarItemID(sectionRemoteProxies), Title: locale.T("Proxies")},
			},
		},
		{
			ID:    design.SidebarItemID(SectionDiagnostics),
			Title: locale.T("Diagnostics"),
			Icon:  icons.NavDiagnostics,
		},
		{
			ID:    design.SidebarItemID(SectionSettings),
			Title: locale.T("Settings"),
			Icon:  icons.NavSettings,
			Children: []design.SidebarEntry{
				{ID: design.SidebarItemID(sectionSettingsConnection), Title: locale.T("Connection")},
				{ID: design.SidebarItemID(sectionSettingsSubscriptions), Title: locale.T("Subscriptions")},
				{ID: design.SidebarItemID(sectionSettingsLanguage), Title: locale.T("Language")},
				{ID: design.SidebarItemID(sectionSettingsStorage), Title: locale.T("Storage")},
			},
		},
		{
			ID:    design.SidebarItemID(SectionHelp),
			Title: locale.T("Help"),
			Icon:  icons.NavHelp,
		},
	}
}

// navigateTo — переход по навигации.
//
// Подраздел живёт на той же странице, что и его родитель: отдельной
// страницы у «Proxies» нет, есть блок внутри Local. Поэтому подраздел
// открывает страницу-владельца, а сам остаётся подсвеченным в колонке.
func (a *App) navigateTo(id SectionID) {
	a.showSection(id)
}

// showSection переключает видимую страницу и исполняет побочные эффекты
// выбора. Единственный путь смены раздела для сайдбара.
func (a *App) showSection(id SectionID) {
	pageID := sectionPage(id)
	page, ok := a.pages[pageID]
	if !ok {
		return
	}
	// Побочные эффекты исполняются по СТРАНИЦЕ: переход Local → Local.Proxies
	// не должен заново снимать транспорт и перезагружать список — это стоило
	// бы лишнего запроса и мигания списка.
	firstVisit := a.currentSection != pageID
	a.currentSection = pageID
	if firstVisit {
		a.selectSection(pageID)
	}
	if a.contentHost != nil {
		a.contentHost.Objects = []fyne.CanvasObject{page}
		a.contentHost.Refresh()
	}
	if a.sidebar != nil {
		a.sidebar.SetSelected(design.SidebarItemID(id))
	}
}

// GetContent returns the root content for the main window (tabs alone when
// the overlay is disabled, tabs+overlay when enabled — see
// `wizardOverlayEnabled`).
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
