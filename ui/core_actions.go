package ui

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
)

// Файл core_actions.go — ОБЩИЕ действия Start/Stop (SPEC 145 §16-17).
//
// **Зачем отдельный файл.** Прежняя панель Core запускала ядро не «голым»
// вызовом core.StartSingBoxProcess(): перед ним шёл beginPendingOp, который
// мгновенно гасил обе кнопки, писал «Starting…» и взводил потолок ожидания на
// случай, когда ядро не сменит состояние (config rejected, демон молчит).
//
// Когда кнопки переехали на Home, эта обёртка потерялась: там вызывался только
// core.StartSingBoxProcess(). Это тихая функциональная регрессия — кнопка не
// даёт обратной связи, а при неудачном старте остаётся «живой» и приглашает
// нажать второй раз.
//
// Здесь обёртка вынесена в одно место и используется ОБОИМИ местами: панелью
// Core и страницей Home. Копии логики нет — есть один источник.

// pendingOpTimeout — потолок ожидания смены состояния ядра после Start/Stop.
//
// Это НЕ «ожидание успеха»: на неудачном пути состояние не меняется вовсе
// (StartVPN/StopVPN показывают диалог и выходят, не трогая RunningState).
// Без потолка кнопки остались бы мёртвыми до перезапуска лаунчера.
const pendingOpTimeout = 12 * time.Second

// coreActionTarget — то, что умеет показать ожидание операции. Реализуется
// и панелью Core, и Home: обе имеют набор кнопок и подпись состояния.
type coreActionTarget interface {
	// actionButtons — кнопки, которые надо погасить на время операции.
	actionButtons() []*widget.Button
	// setPendingStatus — показать «Starting…» / «Stopping…» и запомнить,
	// какого состояния мы ждём (wantRunning: true — Start).
	setPendingStatus(text string, wantRunning bool)
	// releasePending — отпустить кнопки (по таймауту).
	releasePending()
}

// beginCoreOp — мгновенная реакция на Start/Stop: гасим кнопки и пишем, что
// операция идёт. Возврат к обычному виду — по приходу реального статуса
// (updateRunningStatus панели Core / Refresh страницы Home) либо по таймауту.
//
// Генерация операций отсекает просроченный таймаут: если состояние успело
// смениться, старый таймер ничего не трогает.
func beginCoreOp(t coreActionTarget, statusText string, wantRunning bool, gen *uint64) {
	*gen++
	myGen := *gen

	for _, b := range t.actionButtons() {
		if b != nil {
			b.Disable()
			b.Importance = widget.MediumImportance
			b.Refresh()
		}
	}
	t.setPendingStatus(statusText, wantRunning)

	go func() {
		time.Sleep(pendingOpTimeout)
		fyne.Do(func() {
			if *gen != myGen {
				return
			}
			debuglog.WarnLog("core action: state did not change within %s — releasing buttons", pendingOpTimeout)
			t.releasePending()
		})
	}()
}

// StartCoreAction — единая точка запуска ядра для UI.
//
// Вызывает ровно тот же core-путь, что и раньше, но с полной обёрткой
// ожидания. target может быть nil (например, вызывающий не имеет кнопок) —
// тогда выполняется только сам запуск.
func StartCoreAction(t coreActionTarget, gen *uint64) {
	if t != nil && gen != nil {
		beginCoreOp(t, locale.T("Starting..."), true, gen)
	}
	core.StartSingBoxProcess()
}

// StopCoreAction — единая точка остановки ядра для UI.
func StopCoreAction(t coreActionTarget, gen *uint64) {
	if t != nil && gen != nil {
		beginCoreOp(t, locale.T("Stopping..."), false, gen)
	}
	core.StopSingBoxProcess()
}

// OpenCoreSettingsAction — открыть настройки ядра (движок + сопряжение с
// демоном).
//
// **Восстановленный вход.** В прежней панели Core справа от строки состояния
// стояла кнопка-шестерёнка (connBtn, подпись «⚙»), ведущая сюда. При переезде
// на Home её потеряли: настройки движка стали доступны только через
// Diagnostics/Settings, то есть не там, где пользователь их ищет.
//
// Логика НЕ переписана: это тот же вызов OpenConnectionWindow с тем же
// пост-действием. После закрытия окна движок мог смениться classic↔daemon,
// поэтому сбрасывается состояние API, перечитывается список прокси и
// обновляется маркер канала — ровно как делала старая кнопка.
func OpenCoreSettingsAction(ac *core.AppController) {
	if ac == nil {
		return
	}
	OpenConnectionWindow(ac, func() {
		if ac.UIService != nil && ac.UIService.ResetAPIStateFunc != nil {
			ac.UIService.ResetAPIStateFunc()
		}
		if ac.UIService != nil && ac.UIService.RefreshAPIFunc != nil {
			ac.UIService.RefreshAPIFunc()
		}
		if ac.UIService != nil && ac.UIService.UpdateCoreStatusFunc != nil {
			ac.UIService.UpdateCoreStatusFunc()
		}
	})
}
