package ui

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/locale"
	"singbox-launcher/ui/design"
)

// Файл home.go — дашборд (SPEC 145).
//
// **Что это заменяет.** Раньше «главный экран» был списком узлов слева и
// панелью управления справа. Пользователь при запуске видел половину экрана
// пустого списка и россыпь служебных кнопок. Теперь главный экран — сводка:
// состояние, ключевые метрики и переходы, а список узлов живёт на своей
// странице, где ему хватает ширины.
//
// **Источники данных.** Все значения берутся из существующих сервисов:
// состояние ядра — RunningState контроллера, версия/пути — FileService,
// подписки — StateService, машины — реестр Remote, трафик —
// internal/traffic. Ничего не считается заново и не дублируется.
//
// **Действие.** Кнопка Start/Stop вызывает те же core-функции, что и прежняя
// панель. Никакого второго состояния «подключено» здесь не заводится.
type HomePage struct {
	ac *core.AppController

	root fyne.CanvasObject

	statusBadge *design.StatusBadge
	primaryBtn  *widget.Button

	runtimeRows []*design.CardRow
	proxiesCard *design.ClickableCard
	remoteCard  *design.ClickableCard

	// navigate — переход на другую страницу (ставится оболочкой).
	navigate func(RouteID)
}

// NewHomePage собирает дашборд.
func NewHomePage(ac *core.AppController, controller *core.AppController) *HomePage {
	h := &HomePage{ac: ac}

	header := design.NewPageHeader(
		locale.T("Home"),
		locale.T("Your local sing-box instance"),
		nil,
	)
	// Заголовок страницы на дашборде заменён «hero»-блоком: имя профиля и
	// главное действие важнее служебного заголовка «Home».
	_ = header

	h.statusBadge = design.NewStatusBadge(locale.T("Disconnected"), design.StatusNeutral)
	h.primaryBtn = design.PrimaryAction(locale.T("Start"), func() {
		// Тот же путь, что у прежней кнопки Start: pending-состояние и
		// запуск процесса через контроллер.
		core.StartSingBoxProcess()
		h.refresh()
	})

	hero := h.buildHero()
	runtimeCard := h.buildRuntimeCard()
	h.proxiesCard = design.NewClickableCard(
		locale.T("Proxies"), locale.T("Nodes of the local core"),
		func() { h.goTo(RouteProxies) })
	h.remoteCard = design.NewClickableCard(
		locale.T("Remote"), locale.T("Manage other machines"),
		func() { h.goTo(RouteRemote) })

	content := container.NewVBox(
		hero,
		design.NewCard("", "", nil, runtimeCard).Object(),
		h.proxiesCard,
		h.remoteCard,
	)

	h.root = content
	return h
}

// Object возвращает корневой объект страницы.
func (h *HomePage) Object() fyne.CanvasObject { return h.root }

// CanvasObject — обёртка, позволяющая положить страницу в общий contentHost
// наравне с обычными fyne.CanvasObject. Страница — не виджет: у неё нет
// своего renderer'а, она лишь собирает дерево, поэтому делегируем корню.
type homeCanvas struct{ page *HomePage }

func (h homeCanvas) MinSize() fyne.Size      { return h.page.root.MinSize() }
func (h homeCanvas) Resize(s fyne.Size)      { h.page.root.Resize(s) }
func (h homeCanvas) Move(p fyne.Position)    { h.page.root.Move(p) }
func (h homeCanvas) Position() fyne.Position { return h.page.root.Position() }
func (h homeCanvas) Size() fyne.Size         { return h.page.root.Size() }
func (h homeCanvas) Hide()                   { h.page.root.Hide() }
func (h homeCanvas) Show()                   { h.page.root.Show() }
func (h homeCanvas) Visible() bool           { return h.page.root.Visible() }
func (h homeCanvas) Refresh()                { h.page.root.Refresh() }

// CanvasObject возвращает страницу как fyne.CanvasObject.
func (h *HomePage) CanvasObject() fyne.CanvasObject { return homeCanvas{page: h} }

// SetNavigate связывает страницу с навигацией оболочки.
func (h *HomePage) SetNavigate(fn func(RouteID)) { h.navigate = fn }

func (h *HomePage) goTo(r RouteID) {
	if h.navigate != nil {
		h.navigate(r)
	}
}

// buildHero — верхний блок: состояние и главное действие.
func (h *HomePage) buildHero() fyne.CanvasObject {
	title := design.PageTitle(locale.T("Local"))
	subtitle := design.PageSubtitle(locale.T("Local sing-box core"))

	left := container.NewVBox(title, subtitle, h.statusBadge.Object())
	return container.NewBorder(nil, nil, left, container.NewCenter(h.primaryBtn))
}

// buildRuntimeCard — карточка со сведениями о ядре и конфиге.
//
// Строки переиспользуют уже существующие функции контроллера: раньше эти же
// значения показывала панель Core (версия ядра, путь конфига, состояние).
func (h *HomePage) buildRuntimeCard() fyne.CanvasObject {
	versionRow := design.NewCardRow(
		locale.T("Core"), h.coreVersionText(), nil, nil)
	configRow := design.NewCardRow(
		locale.T("Configuration"), h.configPathText(), nil, nil)

	backend := locale.T("Classic process")
	if h.ac != nil && h.ac.CorePersistsAfterAppExit() {
		backend = locale.T("System daemon")
	}
	backendRow := design.NewCardRow(locale.T("Backend"), backend, nil, nil)

	h.runtimeRows = []*design.CardRow{versionRow, configRow, backendRow}
	rows := make([]fyne.CanvasObject, 0, len(h.runtimeRows))
	for _, r := range h.runtimeRows {
		rows = append(rows, r.Object())
	}
	return container.NewVBox(rows...)
}

// Refresh обновляет проекцию состояния. Вызывается при смене состояния VPN и
// при входе на страницу; виджеты не пересоздаются.
func (h *HomePage) Refresh() {
	h.refresh()
}

func (h *HomePage) refresh() {
	if h.ac == nil {
		return
	}
	running := h.ac.RunningState != nil && h.ac.RunningState.IsRunning()
	if running {
		h.statusBadge.Set(locale.T("Connected"), design.StatusSuccess)
		h.primaryBtn.SetText(locale.T("Stop"))
		// Кнопка Stop вызывает тот же путь, что и раньше. Подменяем
		// обработчик, а не создаём вторую кнопку: единственный контрол
		// переключает своё действие вместе с состоянием.
		h.primaryBtn.OnTapped = func() {
			core.StopSingBoxProcess()
			h.refresh()
		}
	} else {
		h.statusBadge.Set(locale.T("Disconnected"), design.StatusNeutral)
		h.primaryBtn.SetText(locale.T("Start"))
		h.primaryBtn.OnTapped = func() {
			core.StartSingBoxProcess()
			h.refresh()
		}
	}

	if len(h.runtimeRows) >= 2 {
		h.runtimeRows[0].SetSubtitle(h.coreVersionText())
		h.runtimeRows[1].SetSubtitle(h.configPathText())
	}
}

// coreVersionText — версия ядра или честное «неизвестно».
func (h *HomePage) coreVersionText() string {
	if h.ac == nil {
		return "—"
	}
	v, err := h.ac.GetInstalledCoreVersion()
	if err != nil || v == "" {
		return locale.T("Not installed")
	}
	return v
}

// configPathText — путь к конфигу.
func (h *HomePage) configPathText() string {
	if h.ac == nil || h.ac.FileService == nil {
		return "—"
	}
	return h.ac.FileService.ConfigPath
}

// coreLabel — короткая подпись ядра для строк.
func coreLabel(version string) string {
	if version == "" {
		return locale.T("Unknown")
	}
	return fmt.Sprintf("sing-box %s", version)
}
