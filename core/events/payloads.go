package events

// Payload-структуры — конкретные типы для Event.Payload по Kind.
//
// При получении события подписчик приводит Payload к ожидаемому типу:
//
//	bus.Subscribe(events.StateChanged, func(ev events.Event) {
//	    p, ok := ev.Payload.(events.StateChangedPayload)
//	    if !ok { return } // защита от Publish с некорректным payload'ом
//	    // ... использовать p.Changed
//	})
//
// При добавлении новой константы EventKind — добавить здесь соответствующий *Payload.

// StateChangedPayload сопровождает Kind StateChanged.
type StateChangedPayload struct {
	// Changed — список «доменов» состояния, которые поменялись
	// ("proxies", "tun", "dns", "rules", "vars", ...).
	Changed []string
}

// ConfigBuiltPayload сопровождает Kind ConfigBuilt.
type ConfigBuiltPayload struct {
	// OK — true, если build + sing-box check прошли, файл записан.
	OK bool
	// Error — заполнено при OK == false.
	Error error
	// Warnings — non-fatal предупреждения от build/validate.
	Warnings []string
	// DisabledNodes — узлы, выключенные страховкой «ядро отвергло узел»
	// (SPEC 132) в ЭТОМ проходе сборки. Пусто в подавляющем большинстве
	// сборок: успешная проверка не выключает ничего.
	//
	// Едет и при OK:false: цикл мог выключить несколько узлов и упереться в
	// ошибку не про узел — выключенные при этом остаются выключенными, и
	// человек обязан узнать об этом обоими путями.
	DisabledNodes []DisabledNode
}

// DisabledNode — одна строка списка «выключено ядром» (SPEC 132 §6.1).
type DisabledNode struct {
	// SourceLabel — подпись источника ("" — не определён).
	SourceLabel string
	// Tag — финальный тег, которым узел назвало ядро.
	Tag string
	// Reason — дословный текст ядра.
	Reason string
}

// VpnStateChangedPayload сопровождает Kind VpnStateChanged.
type VpnStateChangedPayload struct {
	Running bool
	// Teardown says WHY the core went down, when it went down deliberately.
	//
	// Running==false has several causes that look identical on the wire — a user
	// stop, a restart's teardown, a crash — and they require OPPOSITE responses. A
	// listener that settles a pending stop on any false reading reports a
	// completed stop for a restart that is about to bring the core back up.
	//
	// Empty means "not a deliberate teardown" (a crash, or a plain state refresh),
	// which is the safe default: an unlabelled false never ends a stop operation.
	Teardown TeardownReason
}

// TeardownReason identifies a deliberate teardown of the core.
type TeardownReason string

const (
	// TeardownNone — no deliberate teardown: a crash, or a routine refresh.
	TeardownNone TeardownReason = ""
	// TeardownUserStop — the user asked for the core to stop, and it did.
	TeardownUserStop TeardownReason = "user_stop"
	// TeardownRestart — the core was taken down as part of a restart. The core is
	// EXPECTED back, so this must not be reported as a completed stop.
	TeardownRestart TeardownReason = "restart"
	// TeardownEngineSwitch — the core was taken down because the engine changed.
	TeardownEngineSwitch TeardownReason = "engine_switch"
	// TeardownShutdown — the application is exiting.
	TeardownShutdown TeardownReason = "shutdown"
)
