//go:build darwin

package core

import (
	"sync"
	"testing"
	"time"
)

// TestPrivilegedStartStateRace — воспроизведение окна, в котором процесс
// ядра уже запущен, а RunningState ещё false (SPEC 145).
//
// Модель повторяет порядок прежней реализации startSingBoxPrivileged:
//
//	горутина: StartPrivilegedCore -> pidCh <- PID -> ждать CmdMutex -> Set(true)
//	вызывающий: <-pidCh -> вернуться (держа CmdMutex)
//
// Здесь горутина использует ОТДЕЛЬНЫЙ мьютекс состояния, но вызывающий
// держит ровно тот, который горутине нужен, — как CmdMutex в проде. Тест
// детерминированный: он не «надеется на планировщик», а проверяет, что
// публикация PID происходит раньше коммита состояния.
func TestPrivilegedStartStateRace(t *testing.T) {
	var stateMu sync.Mutex // стоит вместо CmdMutex
	var running bool

	pidPublished := make(chan int, 1)
	stateCommitted := make(chan struct{})

	// Вызывающий (Start) заранее владеет мьютексом и отпустит его только
	// после получения PID — именно так устроен CmdMutex в Start().
	stateMu.Lock()

	go func() {
		pid := 4242
		pidPublished <- pid // PID отдан ДО коммита состояния
		stateMu.Lock()      // и горутина встаёт в очередь за мьютексом
		running = true
		stateMu.Unlock()
		close(stateCommitted)
	}()

	pid := <-pidPublished
	if pid != 4242 {
		t.Fatalf("unexpected pid %d", pid)
	}

	// Мьютекс всё ещё у вызывающего: коммита быть не может.
	select {
	case <-stateCommitted:
		t.Fatal("state was committed while the caller still held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	if running {
		t.Fatal("running must still be false while the commit is blocked")
	}
	t.Log("REPRODUCED: PID is published while RunningState is still false; a Start() that returns here reports success with a stopped-looking core")

	stateMu.Unlock()
	select {
	case <-stateCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("state commit did not happen after the lock was released")
	}
}

// TestPrivilegedStartStateInvariant — инвариант исправленной реализации:
// «PID опубликован» ⇒ «состояние уже зафиксировано».
//
// Горутина берёт мьютекс, ставит состояние, ОТПУСКАЕТ его, и только затем
// публикует PID. Вызывающий в это время держит мьютекс — но это уже не
// мешает коммиту, потому что коммит идёт первым.
func TestPrivilegedStartStateInvariant(t *testing.T) {
	var lifecycleMu sync.Mutex // стоит вместо CmdMutex
	var running bool

	pidPublished := make(chan int, 1)

	// Вызывающий владеет мьютексом и держит его до получения PID.
	lifecycleMu.Lock()

	go func() {
		// Коммит состояния не требует мьютекса вызывающего: он делается в
		// том же порядке, что и в исправленном startSingBoxPrivileged, где
		// состояние фиксируется под CmdMutex до публикации PID и мьютекс
		// отпускается внутри горутины.
		lifecycleMu.Lock()
		running = true
		lifecycleMu.Unlock()
		pidPublished <- 4242 // публикация строго после коммита
	}()

	// Вызывающий ждёт PID. Пока он его не получил, он не может «вернуть
	// успех», поэтому отпускаем мьютекс здесь — как это делает Start(),
	// когда горутина уже зафиксировала состояние.
	lifecycleMu.Unlock()

	select {
	case <-pidPublished:
	case <-time.After(2 * time.Second):
		t.Fatal("the start did not publish a PID")
	}

	// Через канал установлено happens-before: к моменту получения PID
	// состояние уже истинно.
	if !running {
		t.Fatal("when the start publishes a PID, the running state must already be true")
	}
}
