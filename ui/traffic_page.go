package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"

	"singbox-launcher/core"
	"singbox-launcher/internal/locale"
	"singbox-launcher/ui/design"
)

// Файл traffic_page.go — страница трафика (SPEC 145).
//
// **Что здесь есть и чего нет.** Страница переиспользует уже существующий
// профилировщик трафика: кнопка открывает то же самое окно, что и раньше на
// вкладке Diagnostics, через тот же синглтон-менеджер окон. Второго сборщика
// данных не создаётся — SPEC прямо запрещает дублировать traffic collector.
//
// Сводные метрики (рекордер активен/нет) читаются у того же менеджера, что и
// раньше, поэтому значение не может разойтись с реальным состоянием.
func buildTrafficPage(ac *core.AppController) fyne.CanvasObject {
	header := design.NewPageHeader(
		locale.T("Traffic"),
		locale.T("Live traffic of the local core"),
		nil,
	)

	// Строка состояния рекордера — проекция состояния профилировщика.
	statusRow := design.NewCardRow(
		locale.T("Profiler"),
		trafficRecorderState(ac),
		nil,
		nil,
	)

	openBtn := design.PrimaryAction(locale.T("Open Traffic Profiler"), func() {
		// Тот же путь, что у кнопки на Diagnostics: синглтон-окно, повторный
		// клик фокусирует уже открытое.
		mgr := trafficWindowManager(ac, func() {
			fyne.Do(func() {
				statusRow.SetSubtitle(trafficRecorderState(ac))
			})
		})
		mgr.Show()
	})

	card := design.NewCard(
		locale.T("Traffic Profiler"),
		locale.T("Per-connection breakdown, sessions and history."),
		nil,
		container.NewVBox(statusRow.Object(), design.SpacerV(design.SpaceS), openBtn),
	)

	return container.NewBorder(header.Object(), nil, nil, nil,
		sectionScroll(card.Object()))
}

// trafficRecorderState — состояние рекордера профилировщика.
//
// Источник — ТОТ ЖЕ синглтон-менеджер окон, которым пользуется кнопка на
// Diagnostics (traffic_bootstrap.go). Второй профилировщик не создаётся:
// состояние не может разойтись с реальным.
func trafficRecorderState(ac *core.AppController) string {
	if trafficManager == nil {
		// Менеджер ещё не создан — значит, окно ни разу не открывали, и
		// запись идти не может. Обращаться к нему здесь нельзя: создание
		// тянет за собой зависимости окна.
		return locale.T("Idle")
	}
	if trafficManager.IsRecording() {
		return locale.T("Recording")
	}
	return locale.T("Idle")
}
