package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/locale"
	"singbox-launcher/ui/design"
)

// Файл pages.go — новые экраны оболочки (SPEC 145).
//
// **Ключевой принцип.** Ни один экран здесь не создаёт второй источник истины
// и не переписывает бизнес-вызовы. Каждая страница либо напрямую переиспользует
// уже существующий объект (список прокси, панель машин, профилировщик), либо
// проецирует данные контроллера, полученные через те же функции, что и раньше.
//
// Это важно, потому что SPEC 145 переставляет UI, а не переписывает логику:
// кнопки Start/Stop остаются вызовами core.StartSingBoxProcess /
// core.StopSingBoxProcess, выбор узла — тем же колбэком панели, а не новым.

// buildProxiesPage — страница списка узлов локального ядра.
//
// Правая колонка старого dashboard'а сюда НЕ переезжает: список получает всю
// ширину. Раньше он делил экран с панелью управления в пропорции 62/38, из-за
// чего под имена узлов оставалось меньше места, чем под второстепенные
// сведения.
//
// Возвращается тот же объект панели, что и раньше: все его колбэки,
// виртуализация списка и состояние остаются нетронутыми.
func buildProxiesPage(panel *ProxyListPanel) fyne.CanvasObject {
	header := design.NewPageHeader(
		locale.T("Proxies"),
		locale.T("Nodes of your local sing-box core"),
		nil,
	)
	return container.NewBorder(header.Object(), nil, nil, nil,
		container.New(&design.MaxWidthLayout{Max: design.MaxContentWidth + 260}, panel.Content))
}

// buildRemotePage — страница удалённых машин.
//
// Сохраняется прежняя двухколоночная модель (список машин + список прокси
// выбранной машины), потому что она отражает реальную связь данных: прокси
// принадлежат выбранной машине. Меняется оформление, не структура.
func buildRemotePage(proxyPanel *ProxyListPanel, machines fyne.CanvasObject) fyne.CanvasObject {
	// Page-local navigation: страница показывает либо машины, либо их узлы.
	// Раньше обе колонки стояли одновременно, и список прокси невыбранной
	// машины занимал половину экрана пустотой.
	body := container.NewStack()

	machinesView := container.New(&design.MaxWidthLayout{Max: design.MaxContentWidth},
		container.NewVBox(machines))
	proxiesView := container.New(&design.MaxWidthLayout{Max: design.MaxContentWidth + 260},
		container.NewVBox(proxyPanel.Content))

	seg := design.NewSegmentedNav(
		[]string{locale.T("Machines"), locale.T("Proxies")},
		func(i int) {
			if i == 0 {
				body.Objects = []fyne.CanvasObject{machinesView}
			} else {
				body.Objects = []fyne.CanvasObject{proxiesView}
			}
			body.Refresh()
		},
	)
	// По умолчанию — машины: без выбранной машины список узлов пуст, и
	// показывать пустоту первым экраном нельзя.
	body.Objects = []fyne.CanvasObject{machinesView}

	header := design.NewPageHeader(
		locale.T("Remote"),
		locale.T("Manage the sing-box cores on your other machines"),
		seg,
	)
	return container.NewBorder(header.Object(), nil, nil, nil, body)
}

// buildAboutPage — страница «О программе».
//
// Раньше Help был одной строкой с версией и двумя ссылками в подвале,
// набранными emoji-подписями. Теперь — компактные строки, как в системных
// настройках: заголовок, описание, действие.
func buildAboutPage(ac *core.AppController) fyne.CanvasObject {
	header := design.NewPageHeader(
		locale.T("About"),
		locale.T("About this build and where to find us"),
		nil,
	)
	content := CreateHelpTab(ac)
	return container.NewBorder(header.Object(), nil, nil, nil,
		design.ConstrainContent(content, design.MaxContentWidth))
}

// sectionScroll оборачивает содержимое раздела в прокрутку с полями.
func sectionScroll(content fyne.CanvasObject) fyne.CanvasObject {
	return container.NewVScroll(design.ConstrainContent(content, design.MaxContentWidth))
}

// vbox — короткий хелпер: вертикальный список с зазорами карточек.
func vbox(items ...fyne.CanvasObject) *fyne.Container {
	return container.NewVBox(items...)
}

// labelMuted — приглушённая подпись (для сводок и метаданных).
func labelMuted(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Importance = widget.LowImportance
	return l
}
