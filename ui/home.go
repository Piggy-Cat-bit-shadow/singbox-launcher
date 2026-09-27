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
// состояние, ключевые сведения и переходы; список узлов живёт на своей
// странице, где ему хватает ширины.
//
// **Источники данных.** Состояние ядра — RunningState контроллера, версия и
// пути — FileService. Ничего не считается заново, второй копии состояния нет.
//
// **Действие.** Start/Stop идут через общий StartCoreAction/StopCoreAction
// (core_actions.go) — тот же путь, что у панели Core, вместе с обёрткой
// ожидания. «Голый» core.StartSingBoxProcess() здесь стоять не должен: без
// обёртки кнопка не даёт обратной связи и не защищена от двойного нажатия.
type HomePage struct {
	ac *core.AppController

	// root — РЕАЛЬНЫЙ корневой объект страницы. Отдаётся в contentHost как
	// есть: делегирующая обёртка без renderer'а не владеет деревом и не
	// рисуется — именно из-за неё страница была пустой.
	root fyne.CanvasObject

	statusBadge *design.StatusBadge
	primaryBtn  *widget.Button

	runtimeRows []*design.CardRow

	// pendingGen — поколение операции Start/Stop (см. core_actions.go).
	pendingGen uint64
	// awaiting — операция в полёте: кнопка погашена до смены состояния.
	awaiting bool
	// awaitingFor — какого состояния ждём (true — running).
	awaitingFor bool
	// navigate — переход на другую страницу (ставится оболочкой).
	navigate func(RouteID)
}

// NewHomePage собирает дашборд.
func NewHomePage(ac *core.AppController, controller *core.AppController) *HomePage {
	h := &HomePage{ac: ac}

	h.statusBadge = design.NewStatusBadge(locale.T("Disconnected"), design.StatusNeutral)
	h.primaryBtn = design.PrimaryAction(locale.T("Start"), func() {
		// Полная обёртка Start, а не голый вызов core: см. core_actions.go.
		StartCoreAction(h, &h.pendingGen)
		h.refresh()
	})

	hero := h.buildHero()
	runtimeCard := h.buildRuntimeCard()
	proxiesCard := design.NewClickableCard(
		locale.T("Proxies"), locale.T("Nodes of the local core"),
		func() { h.goTo(RouteProxies) })
	remoteCard := design.NewClickableCard(
		locale.T("Remote"), locale.T("Manage other machines"),
		func() { h.goTo(RouteRemote) })

	// Карточки разделены ровно одним интервалом: раньше между ними было
	// «пусто» больше, чем внутри карточек, и страница выглядела разряженной.
	h.root = container.NewVBox(
		hero,
		design.SpacerV(design.CardGap),
		design.NewCard("", "", nil, runtimeCard).Object(),
		design.SpacerV(design.CardGap),
		proxiesCard,
		design.SpacerV(design.CardGap),
		remoteCard,
	)
	return h
}

// Object возвращает корневой объект страницы: обычный *fyne.Container.
func (h *HomePage) Object() fyne.CanvasObject { return h.root }

// SetNavigate связывает страницу с навигацией оболочки.
func (h *HomePage) SetNavigate(fn func(RouteID)) { h.navigate = fn }

func (h *HomePage) goTo(r RouteID) {
	if h.navigate != nil {
		h.navigate(r)
	}
}

// --- Реализация coreActionTarget (см. core_actions.go) ---------------------

// actionButtons — кнопка, которую надо гасить на время операции.
func (h *HomePage) actionButtons() []*widget.Button {
	return []*widget.Button{h.primaryBtn}
}

// setPendingStatus — показать, что операция идёт, и запомнить ожидаемое
// состояние: по нему refresh понимает, когда кнопку можно вернуть.
func (h *HomePage) setPendingStatus(text string, wantRunning bool) {
	h.awaiting = true
	h.awaitingFor = wantRunning
	if h.statusBadge != nil {
		h.statusBadge.Set(text, design.StatusInfo)
	}
}

// releasePending — отпустить кнопку и вернуться к реальному состоянию.
func (h *HomePage) releasePending() {
	h.awaiting = false
	h.refresh()
}

// pendingDone сообщает, что операция Start/Stop завершилась и кнопку можно
// вернуть в рабочее состояние.
//
// Операция считается завершённой, когда состояние ядра перестало быть
// «начальным» для неё: после Start это running, после Stop — не running.
// Пока состояние не изменилось, кнопка остаётся погашенной (как в панели
// Core), но не дольше потолка ожидания: его снимает releasePending.
func (h *HomePage) pendingDone() bool {
	if !h.awaiting {
		return true
	}
	var running bool
	if h.ac != nil && h.ac.RunningState != nil {
		running = h.ac.RunningState.IsRunning()
	} else {
		// Контроллера нет — ждать нечего, отпускаем: кнопка не должна
		// остаться мёртвой навсегда.
		h.awaiting = false
		return true
	}
	if pendingSettled(h.awaiting, h.awaitingFor, running) {
		h.awaiting = false
	}
	return !h.awaiting
}

// pendingSettled — чистое решении о снятии блокировки.
//
// Вынесено отдельно, чтобы проверялось тестом без живого контроллера:
// RunningState.Set тянет за собой UpdateUI и требует собранного приложения,
// а сама логика тривиальна и именно её важно зафиксировать.
//
// awaiting — операция в полёте; wantRunning — какого состояния ждём;
// running — текущее состояние ядра.
func pendingSettled(awaiting, wantRunning, running bool) bool {
	if !awaiting {
		return true
	}
	return running == wantRunning
}

// buildHero — верхний блок: состояние и главное действие.
func (h *HomePage) buildHero() fyne.CanvasObject {
	title := design.PageTitle(locale.T("Local"))
	subtitle := design.PageSubtitle(locale.T("Local sing-box core"))

	// Заголовок и подзаголовок — вплотную, статус — с небольшим отступом:
	// три строки одного блока не должны разъезжаться на пол-экрана.
	head := container.NewVBox(title, subtitle)
	left := container.NewVBox(head, design.SpacerV(design.SpaceS), h.statusBadge.Object())

	return container.NewBorder(nil, nil, left, container.NewCenter(h.primaryBtn))
}

// buildRuntimeCard — сведения о ядре, конфиге и бэкенде.
func (h *HomePage) buildRuntimeCard() fyne.CanvasObject {
	versionRow := design.NewCardRow(locale.T("Core"), h.coreVersionText(), nil, nil)
	configRow := design.NewCardRow(locale.T("Configuration"), h.configPathText(), nil, nil)

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

// Refresh обновляет проекцию состояния ядра. Виджеты не пересоздаются.
func (h *HomePage) Refresh() { h.refresh() }

func (h *HomePage) refresh() {
	if h.ac == nil {
		return
	}
	running := h.ac.RunningState != nil && h.ac.RunningState.IsRunning()
	if running {
		h.statusBadge.Set(locale.T("Connected"), design.StatusSuccess)
		h.primaryBtn.SetText(locale.T("Stop"))
		h.primaryBtn.OnTapped = func() {
			StopCoreAction(h, &h.pendingGen)
			h.refresh()
		}
	} else {
		h.statusBadge.Set(locale.T("Disconnected"), design.StatusNeutral)
		h.primaryBtn.SetText(locale.T("Start"))
		h.primaryBtn.OnTapped = func() {
			StartCoreAction(h, &h.pendingGen)
			h.refresh()
		}
	}

	// Кнопку ОБЯЗАТЕЛЬНО вернуть в рабочее состояние.
	//
	// beginCoreOp гасит её на время операции (защита от двойного нажатия), а
	// снимает блокировку только releasePending — то есть по таймауту 12 с.
	// Из-за этого после успешного Start кнопка оставалась disabled: подпись
	// менялась на «Stop», обработчик подменялся, но нажать её было нельзя.
	//
	// Панель Core делает то же самое в updateRunningStatus (Enable для
	// start/stop/restart) — здесь это упустили.
	if h.pendingDone() {
		h.primaryBtn.Enable()
	}
	h.primaryBtn.Refresh()

	if len(h.runtimeRows) >= 2 {
		h.runtimeRows[0].SetSubtitle(h.coreVersionText())
		h.runtimeRows[1].SetSubtitle(h.configPathText())
	}
}

// coreVersionText — версия ядра или честное «не установлено».
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

// coreLabel — короткая подпись ядра.
func coreLabel(version string) string {
	if version == "" {
		return locale.T("Unknown")
	}
	return fmt.Sprintf("sing-box %s", version)
}
