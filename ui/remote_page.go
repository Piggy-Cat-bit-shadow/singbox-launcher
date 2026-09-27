package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/locale"
	"singbox-launcher/ui/design"
)

// remoteParts — объекты страницы Remote, созданные РОВНО ОДИН РАЗ.
//
// CreateProxyListPanel и CreateMachineListPanel — stateful: панель машин
// держит собственный registry, карты health/errLog/connectAttempt и
// heartbeat-горутины. Второй вызов создал бы второй heartbeat и вторую копию
// состояния, поэтому и современная страница, и compatibility-обёртка для
// скрытого AppTabs обязаны получать ЭТИ объекты, а не строить свои.
type remoteParts struct {
	proxyPanel *ProxyListPanel
	machines   fyne.CanvasObject
}

// createRemoteParts создаёт stateful-объекты Remote. Единственная точка
// их создания в приложении.
func createRemoteParts(ac *core.AppController, proxyPanel *ProxyListPanel) remoteParts {
	return remoteParts{
		proxyPanel: proxyPanel,
		machines:   CreateMachineListPanel(ac, proxyPanel),
	}
}

// buildRemotePage — современная страница Remote.
//
// Раньше Remote показывал обе колонки сразу (HSplit: список узлов + машины),
// из-за чего до выбора машины половина экрана была пустой, а список узлов
// стоял в disabled-состоянии. Теперь это page-local navigation
// Machines | Proxies, и по умолчанию открыт список машин.
func buildRemotePage(parts remoteParts) fyne.CanvasObject {
	body := container.NewStack()

	machinesView := design.ConstrainContent(parts.machines, design.MaxContentWidthWide)
	proxiesView := buildRemoteProxiesView(parts.proxyPanel)

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
	// По умолчанию — машины: без выбранной машины список узлов пуст.
	body.Objects = []fyne.CanvasObject{machinesView}

	header := design.NewPageHeader(
		locale.T("Remote"),
		locale.T("Manage the sing-box cores on your other machines"),
		seg,
	)
	return container.NewBorder(header.Object(), nil, nil, nil, body)
}

// buildRemoteProxiesView — вкладка «Proxies» страницы Remote.
//
// Пока машина не выбрана, показывается empty state вместо полного набора
// заблокированных кнопок: ряд серых контролов читается как «всё сломалось»,
// хотя на самом деле просто нечего показывать.
func buildRemoteProxiesView(panel *ProxyListPanel) fyne.CanvasObject {
	empty := design.NewEmptyState(
		locale.T("No machine connected"),
		locale.T("Choose a machine on the Machines tab to see its nodes."),
	)

	// Переключаем содержимое при смене активной машины. GetLxdRemoteOverride
	// — тот же источник, по которому панель понимает, с кем говорит.
	content := container.NewStack(empty.Object())
	refresh := func() {
		if _, _, hasMachine := GetLxdRemoteOverride(); hasMachine {
			content.Objects = []fyne.CanvasObject{panel.Content}
		} else {
			content.Objects = []fyne.CanvasObject{empty.Object()}
		}
		content.Refresh()
	}
	refresh()
	OnOverrideChanged(refresh)

	return design.ConstrainContentWide(content)
}

// buildLegacyRemoteShell — compatibility-обёртка для скрытого AppTabs.
//
// Визуально НЕ используется (AppTabs в дерево не попадает), но обязана
// существовать как валидный объект. Получает ТЕ ЖЕ части, что современная
// страница: второй набор stateful-компонентов не создаётся.
func buildLegacyRemoteShell(parts remoteParts) fyne.CanvasObject {
	return container.NewHSplit(
		parts.proxyPanel.Content,
		withColumnWidth(parts.machines, design.MinPaneWidthPanel),
	)
}

// buildEmptyStatePlaceholder — компактная заглушка для страницы, у которой
// нет содержимого. Используется только compatibility-путями.
func buildEmptyStatePlaceholder(text string) fyne.CanvasObject {
	l := widget.NewLabel(text)
	l.Alignment = fyne.TextAlignCenter
	return container.NewCenter(l)
}
