package ui

import "testing"

// TestPendingSettledReleasesOnStateChange — кнопка Start/Stop обязана
// возвращаться в рабочее состояние, как только состояние ядра сменилось.
//
// Регрессионный тест на дефект «после Start кнопка Stop не нажимается»:
// beginCoreOp гасит кнопку на время операции, а снимала блокировку только
// ветка таймаута (12 с). На успешном пути кнопка оставалась disabled —
// подпись менялась на «Stop», обработчик подменялся, но нажать было нельзя.
//
// Проверяется чистое решение: RunningState.Set тянет UpdateUI и требует
// собранного приложения, а фиксировать нужно именно условие разблокировки.
func TestPendingSettledReleasesOnStateChange(t *testing.T) {
	// Ждём Start, ядро ещё не поднялось — держим кнопку погашенной.
	// Это защита от двойного нажатия, а не дефект: так же ведёт себя панель Core.
	if pendingSettled(true, true, false) {
		t.Fatal("button released while the core has not reached the awaited state")
	}
	// Ядро поднялось — отпускаем немедленно, не дожидаясь таймаута.
	if !pendingSettled(true, true, true) {
		t.Fatal("button stayed disabled after the core started; Stop would be unclickable")
	}

	// Симметрично для Stop.
	if pendingSettled(true, false, true) {
		t.Fatal("button released before the core stopped")
	}
	if !pendingSettled(true, false, false) {
		t.Fatal("button stayed disabled after the core stopped")
	}

	// В покое кнопка всегда доступна.
	if !pendingSettled(false, false, false) || !pendingSettled(false, true, false) {
		t.Fatal("idle page reported a pending operation; the button would stay disabled")
	}
}

// TestPendingDoneWithoutControllerDoesNotHang — без контроллера ждать нечего:
// кнопка обязана вернуться, а не остаться мёртвой навсегда.
func TestPendingDoneWithoutControllerDoesNotHang(t *testing.T) {
	h := &HomePage{awaiting: true, awaitingFor: true}
	if !h.pendingDone() {
		t.Fatal("pendingDone blocked without a controller; the button would never come back")
	}
	if h.awaiting {
		t.Fatal("awaiting flag not cleared")
	}
}

// TestIdlePageReportsNoPendingOp — в покое кнопка всегда доступна.
func TestIdlePageReportsNoPendingOp(t *testing.T) {
	h := &HomePage{}
	if !h.pendingDone() {
		t.Fatal("idle page reported a pending operation; the button would stay disabled")
	}
}
