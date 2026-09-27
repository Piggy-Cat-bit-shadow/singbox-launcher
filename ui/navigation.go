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

// selectSection — единая точка выбора раздела.
//
// Тело метода — механически перенесённое содержимое `app.tabs.OnSelected`.
// Правки логики здесь допустимы только вместе с пониманием, что тот же путь
// исполняется и при переключении через сайдбар, и при программной навигации.
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
	case SectionSettings:
		// Пути раздела Storage (SPEC 135 §4.1): ядро могли скачать, версия
		// ядра могла стать известной — перечитываем при входе.
		if a.refreshSettings != nil {
			a.refreshSettings()
		}
	}

	// Авто-обновление списка узлов идёт только на видимой странице Remote:
	// опрашивать машину, пока пользователь смотрит на Local, незачем.
	a.remotePanel.AutoRefresh().SetTabActive(id == SectionRemote)
	// Состояния WG/AWG-узлов — у панели на экране; после смены транспорта
	// выше, чтобы опрос шёл в ядро своей области.
	a.localPanel.EndpointPoll().SetTabActive(id == SectionLocal)
	a.remotePanel.EndpointPoll().SetTabActive(id == SectionRemote)
	switch id {
	case SectionLocal:
		a.localPanel.RefreshEndpointStates(controller)
	case SectionRemote:
		a.remotePanel.RefreshEndpointStates(controller)
	}

	// Обновляем список только там, где есть с кем разговаривать.
	//
	// На Local это локальное ядро — оно есть всегда (RefreshAPIFunc сам
	// no-op, если ядро не запущено). На Remote собеседник появляется
	// только после выбора машины: без него запрос уходил с пустой группой
	// и возвращал «Daemon: group "" not found». Пустой список до выбора —
	// это честное состояние, а не сбой.
	needRefresh := id == SectionLocal
	if id == SectionRemote {
		_, _, hasMachine := GetLxdRemoteOverride()
		needRefresh = hasMachine
	}
	if needRefresh && controller.UIService != nil && controller.UIService.RefreshAPIFunc != nil {
		controller.UIService.RefreshAPIFunc()
	}
}
