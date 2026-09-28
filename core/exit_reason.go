package core

import (
	"regexp"
	"strings"

	"singbox-launcher/internal/locale"
)

// exit_reason.go — SPEC 143: классификация причины завершения ядра.
//
// До этого решение о перезапуске принималось только по трём булевым флагам
// (stoppedByUser / restartRequested / cleanExit), и детерминированная
// ошибка — несовместимое поле конфига, отсутствие прав, занятый порт —
// выглядела как обычное падение: лаунчер трижды поднимал ядро, трижды
// получал ту же ошибку и лишь потом показывал диалог. Пользователь видел
// «ещё пытаюсь подключиться» там, где нужно было конкретное действие.
//
// Здесь — чистая классификация по тексту, который ядро уже печатает в лог.
// Разбор намеренно узкий: узор должен называть именно нашу ошибку запуска,
// а не любое упоминание сети, иначе транзиентный сбой узла будет принят за
// детерминированный и авто-восстановление отключится зря.

// exitReason — почему ядро завершилось.
type exitReason int

const (
	// exitReasonUnknown — причина не распознана. Считается транзиентной:
	// повторять с ограничением безопаснее, чем не повторять вовсе.
	exitReasonUnknown exitReason = iota

	// exitReasonConfigInvalid — конфиг не принят ядром: неизвестное поле,
	// ошибка разбора, несовместимая версия. Повтор не изменит результат.
	exitReasonConfigInvalid

	// exitReasonPermission — не хватает прав: effective root потерян,
	// TUN не создался. Повтор без смены прав бесполезен.
	exitReasonPermission

	// exitReasonPortInUse — локальный порт занят. Повтор займёт тот же
	// порт и получит ту же ошибку, пока занявший его процесс жив.
	exitReasonPortInUse

	// exitReasonMissingResource — нет нужного локального файла/каталога.
	exitReasonMissingResource

	// exitReasonAPIUnavailable — API ядра не поднялся или отверг
	// аутентификацию.
	exitReasonAPIUnavailable

	// exitReasonTransient — временная причина: сеть, загрузка ресурса,
	// недоступный узел. Повтор уместен.
	exitReasonTransient
)

// Deterministic — повтор не может изменить результат.
func (r exitReason) Deterministic() bool {
	switch r {
	case exitReasonConfigInvalid, exitReasonPermission, exitReasonPortInUse,
		exitReasonMissingResource, exitReasonAPIUnavailable:
		return true
	}
	return false
}

// String — для лога и диагностики.
func (r exitReason) String() string {
	switch r {
	case exitReasonConfigInvalid:
		return "config invalid"
	case exitReasonPermission:
		return "permission denied / not effective root"
	case exitReasonPortInUse:
		return "local listener occupied"
	case exitReasonMissingResource:
		return "missing local resource"
	case exitReasonAPIUnavailable:
		return "API unavailable"
	case exitReasonTransient:
		return "transient"
	}
	return "unknown"
}

// Узоры причин. Все — по подстрокам в нижнем регистре.
//
// Порядок важен: сначала узоры, которые ядро печатает при разборе конфига,
// затем права, затем порт. Более специфичные — раньше более общих.
var (
	reUnknownField = regexp.MustCompile(`unknown field "?[a-z_]+"?`)
	reDecodeConfig = regexp.MustCompile(`decode config|json: cannot unmarshal|invalid character`)
	// reConfigReference — the core's own dangling-reference messages.
	//
	// These MUST be matched before reMissingFile, whose bare `not found` used to
	// swallow them. `default outbound not found: proxy-out` is a CONFIG problem,
	// but it was classified as "missing local resource", so the user was told to
	// check their file paths for a fault that was in route.final. The reason is
	// still deterministic either way (so auto-restart stopped), which is why the
	// misclassification was invisible — the remedy was simply wrong.
	//
	// Matched on the phrases sing-box actually emits for an unresolved tag:
	//   default outbound not found: X / outbound not found: X
	//   dns: server not found: X / rule-set not found: X / inbound not found: X
	reConfigReference = regexp.MustCompile(`(outbound|server|rule[-_]?set|inbound|endpoint|detour|resolver)[^:\n]*not found`)
	rePermission      = regexp.MustCompile(`operation not permitted|permission denied|not permitted`)
	rePortInUse       = regexp.MustCompile(`address already in use|bind: address already`)
	// reMissingFile is intentionally NARROW.
	//
	// A bare `not found` matches a dangling reference (`outbound not found`), an
	// unknown flag and an unresolved hostname, all of which have their own, more
	// useful reasons. Only a filesystem-shaped message belongs here.
	reMissingFile  = regexp.MustCompile(`no such file or directory|open [^:]*: no such|cannot find the (file|path)|stat [^:]*: no such`)
	reAPIAuth      = regexp.MustCompile(`authentication failed|401 unauthorized|invalid secret`)
	reTransientNet = regexp.MustCompile(`no route to host|network is unreachable|i/o timeout|connection refused|temporary failure in name resolution|context deadline exceeded`)
)

// classifyExitText определяет причину завершения по тексту лога ядра.
//
// Пустой текст — exitReasonUnknown, а не «всё хорошо»: отсутствие лога не
// доказательство отсутствия ошибки.
func classifyExitText(text string) exitReason {
	if strings.TrimSpace(text) == "" {
		return exitReasonUnknown
	}
	low := strings.ToLower(text)

	switch {
	case reUnknownField.MatchString(low) || reDecodeConfig.MatchString(low):
		return exitReasonConfigInvalid
	case reConfigReference.MatchString(low):
		// A tag the config references does not exist. Deterministic, and a CONFIG
		// fault rather than a missing file — the distinction decides which remedy
		// the user is shown.
		return exitReasonConfigInvalid
	case rePortInUse.MatchString(low):
		// Порт проверяем ДО прав: `bind: address already in use` не про права.
		return exitReasonPortInUse
	case rePermission.MatchString(low):
		return exitReasonPermission
	case reAPIAuth.MatchString(low):
		return exitReasonAPIUnavailable
	case reMissingFile.MatchString(low):
		return exitReasonMissingResource
	case reTransientNet.MatchString(low):
		return exitReasonTransient
	}
	return exitReasonUnknown
}

// lastLines — последние n непустых строк текста: причина падения обычно в
// хвосте, а не в начале.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// deterministicExitText — текст для пользователя по причине.
//
// Ключ передаётся в locale.T ЛИТЕРАЛОМ в каждой ветке: статический
// проверяльщик локализации (tools/l10n/l10n_check) видит только литеральные
// ключи, а ключ, собранный в переменной, он считает неиспользуемой записью
// каталога и валит прогон с --strict. Поэтому строки живут прямо здесь, а не
// в константах, которые передавались бы в locale.T переменной.
func deterministicExitText(r exitReason) string {
	switch r {
	case exitReasonConfigInvalid:
		return locale.T("The core rejected its configuration, so restarting cannot help. The launcher has stopped auto-restart. Open the config, fix the reported field, save, then start again.")
	case exitReasonPermission:
		return locale.T("The core did not have the privileges it needs (TUN requires root). Restarting cannot help while the privileges are missing. Check that the root-owned core copy matches the current core, then start again.")
	case exitReasonPortInUse:
		return locale.T("A local port the core needs is already in use, so restarting would fail the same way. The launcher has stopped auto-restart. Free the port (another proxy client may hold it) or change the port in the config, then start again.")
	case exitReasonMissingResource:
		return locale.T("A local file or folder the core needs is missing. Restarting cannot help. Check the paths in the config and the launcher's data folder, then start again.")
	case exitReasonAPIUnavailable:
		return locale.T("The core's own API did not come up or rejected the launcher's credentials. Restarting cannot help. Check the Clash API settings (address and secret) and apply them.")
	}
	return ""
}
