// File rebuild_corereject.go — страховка «ядро отвергло узел → авто-выключение»
// (SPEC 132, норма contract/docs/PARSING_PRINCIPLES.md §9).
//
// # Что тут происходит
//
// Ядро проверяет конфиг ЦЕЛИКОМ и на первом же неприемлемом узле отказывается
// стартовать вовсе: одна строка в одном узле подписки оставляет без связи все
// остальные. Дублировать грамматики ядра в клиенте нельзя — копия расходится
// с ним на бампе пина и начинает резать ГОДНЫЕ узлы. Поэтому поверх дешёвых
// проверок реестра стоит общий второй эшелон: ядро назвало узел → приложение
// выключает ЭТОТ узел → пересборка → снова `check`.
//
// # Кандидат вместо «записал и проверил»
//
// До SPEC 132 `config.json` писался на диск и проверялся ПОСЛЕ: битый конфиг
// оставался лежать, и ядро могло стартовать на заведомо отвергнутом файле.
// Теперь собранный конфиг сначала становится файлом-КАНДИДАТОМ рядом с
// `config.json` (тот же каталог: `os.Rename` через границу файловых систем не
// атомарен, а относительные пути внутри конфига обязаны резолвиться так же),
// и `config.json` заменяется ТОЛЬКО тем, что ядро приняло.
//
// Инвариант: в `config.json` попадает только конфиг, принятый ядром.
//
// # Почему цикл конечен
//
// Каждый круг обязан выключить НОВЫЙ узел. Круг, на котором выключить нечего
// — ошибка не про узел, тег не сопоставился, тот же тег назван повторно, —
// цикл прерывает (PARSING_PRINCIPLES §9.5, три нормативных запрета). Плюс жёсткие пределы
// §Пределы ниже.
//
// # Чего тут НЕТ
//
// Перебором виновника не ищут: выключать узлы по очереди, пока конфиг не
// заработает, — гадание, а не диагностика. Узел не чинят, только исключают.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"singbox-launcher/internal/atomicfile"
	"strings"
	"sync"

	"crypto/sha256"
	"encoding/hex"
	"singbox-launcher/core/config"
	"singbox-launcher/core/corereject"
	"singbox-launcher/core/events"
	"singbox-launcher/core/state"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

// Пределы цикла.
const (
	// coreRejectAskAfter — сколько узлов страховка выключает молча, прежде
	// чем спросить человека (решение владельца: 10 кругов).
	coreRejectAskAfter = 10

	// coreRejectHardCap — жёсткий потолок для входов БЕЗ человека
	// (автообновление подписок, API `/action/rebuild-config`, pre-start без
	// UI). Спросить там некого, а подписка на 500 узлов с пачкой негодных
	// должна собраться сама — поэтому фоновый вход идёт дальше молча, но не
	// бесконечно: 200 кругов заведомо больше любой реальной пачки и
	// заведомо конечны.
	coreRejectHardCap = 200

	// daemonRejectStartCap — сколько узлов асинхронный путь демона
	// (ApplyError 422 / FATAL) выключает за один заход Start/Restart.
	// У цикла есть coreRejectHardCap; у потока статусов своего потолка не
	// было — без него пара «применили → FATAL → выключили → применили»
	// крутилась бы бесконечно.
	daemonRejectStartCap = 10 //nolint:unused // читается только в backend_daemon_darwin.go (//go:build darwin)
)

// candidateConfigName — имя файла-кандидата. НЕ `.tmp`: так зовётся
// промежуточный файл обычной атомарной записи (atomicWriteConfig), и путать
// их нельзя — уборка одного снесла бы другой.
const candidateConfigSuffix = ".candidate"

// CoreRejectedNode — узел, выключенный страховкой: что показать человеку.
type CoreRejectedNode struct {
	// SourceLabel — подпись источника, которому узел принадлежит ("" — не
	// определён).
	SourceLabel string
	// Tag — ФИНАЛЬНЫЙ тег, которым узел назвало ядро.
	Tag string
	// Reason — дословный текст ядра.
	Reason string
}

// coreRejectDecider — вопрос человеку на пределе: продолжать ли проверку.
//
// Интерфейсом-колбэком, а не диалогом внутри: цикл живёт в общей функции
// сборки и о UI не знает, а входов у него пять, и у трёх из них спрашивать
// некого. nil = «фоновый вход»: идём дальше молча до жёсткого потолка.
type coreRejectDecider func(disabled int) (keepChecking bool)

// coreRejectProgress — ход цикла для строки состояния («Checking servers…
// (%d disabled)»). nil — никто не смотрит.
type coreRejectProgress func(disabled int)

// NodeDisabler — «выключатель» узла: узкий шов между циклом и хранилищем.
//
// Две реализации, цикл о разнице не знает: savedStateDisabler над
// сохранённым state и адаптер черновика Конфигуратора
// (`FindNodeByLink(m, link).SetCoreRejected(reason)`).
type NodeDisabler interface {
	// Disable выключает узел по ссылке и записывает причину.
	// false = узел не найден либо уже выключен этой же причиной — цикл на
	// таком круге обязан остановиться.
	Disable(link state.NodeLink, reason string) bool
	// Commit закрепляет выключения (для state — запись state.json).
	Commit() error
	// Label — подпись источника узла, для списка выключенных.
	Label(link state.NodeLink) string
}

// savedStateDisabler — реализация над СОХРАНЁННЫМ состоянием.
//
// Держит загруженный state в памяти весь проход: круг цикла правит его и
// пересобирает конфиг из него же, а на диск он уходит один раз — иначе
// десять кругов означали бы десять перезаписей `state.json`, каждая со своим
// окном на сбой.
type savedStateDisabler struct {
	s    *state.State
	path string
	// rejections accumulates (link → reason) as the loop learns which nodes the core
	// refuses.
	//
	// It records IDENTITIES rather than mutating `s` and saving it, because `s` is the
	// state loaded when the BUILD began and a build is not instantaneous: it renders, runs
	// `sing-box check`, and may loop several rounds. Saving that snapshot back at the end
	// reverted everything the user changed in the window — a subscription URL, an
	// enable/disable, a newly added source, DNS, rules, vars — under the guise of
	// recording which nodes the core rejected. The user's edit was simply gone, with no
	// error anywhere.
	rejections []coreRejection
	// dirty — были ли выключения: без них файл не трогаем вовсе.
	dirty bool
}

// coreRejection is one node the core refused, held as an identity so it can be applied
// to whatever state is on disk at commit time.
type coreRejection struct {
	link   state.NodeLink
	reason string
}

func (d *savedStateDisabler) Disable(link state.NodeLink, reason string) bool {
	// The in-memory state is still mutated, because the LOOP re-renders the config from
	// it and must see the node as disabled on the next round. What changes is that this
	// mutation is no longer treated as the thing that gets persisted.
	node := findStateNodeByLink(d.s, link)
	if node == nil {
		return false
	}
	if !node.SetCoreRejected(reason) {
		return false
	}
	d.rejections = append(d.rejections, coreRejection{link: link, reason: reason})
	d.dirty = true
	return true
}

// Commit applies the recorded rejections to the CURRENT state on disk.
//
// LOAD LATEST → APPLY BY IDENTITY → ATOMIC REPLACE. Never "save the snapshot the build
// started from". The node is located by the same stable identity the loop used (source
// ID plus tag), so a reordered, renamed or extended source list is handled correctly:
// only the `core_rejected` field of the named nodes is touched, and every other field —
// including ones the user changed while the build ran — is left as the user left it.
//
// A node that no longer exists is skipped rather than recreated: the user deleted it, and
// resurrecting it to record a rejection would undo their deletion to store a fact about a
// node that is gone.
func (d *savedStateDisabler) Commit() error {
	if !d.dirty {
		return nil
	}
	// Re-read so the merge sees concurrent edits. A read failure is fatal here rather
	// than a fallback to saving the stale snapshot: the whole point is that the stale
	// snapshot must never be written.
	latest, err := state.Load(d.path)
	if err != nil {
		return fmt.Errorf("core reject: cannot reload state to record rejections: %w", err)
	}
	applied := 0
	for _, rej := range d.rejections {
		node := findStateNodeByLink(latest, rej.link)
		if node == nil {
			debuglog.DebugLog("core reject: node %q in source %q no longer exists; its "+
				"rejection is not recorded", rej.link.Tag, rej.link.FolderID)
			continue
		}
		if node.SetCoreRejected(rej.reason) {
			applied++
		}
	}
	if applied == 0 {
		// Nothing to write. Saving anyway would rewrite the file for no reason and
		// widen the window for no benefit.
		return nil
	}
	if err := latest.Save(d.path); err != nil {
		return fmt.Errorf("core reject: save state with rejections: %w", err)
	}
	return nil
}

func (d *savedStateDisabler) Label(link state.NodeLink) string {
	if d.s == nil {
		return ""
	}
	for i := range d.s.Sources {
		src := &d.s.Sources[i]
		if link.FolderID == "" {
			if src.Kind == state.SourceKindServer && src.NodeTagOrLabel() == link.Tag {
				return sourceLabelOf(src)
			}
			continue
		}
		if src.ID == link.FolderID {
			return sourceLabelOf(src)
		}
	}
	return ""
}

func sourceLabelOf(src *state.Source) string {
	if s := strings.TrimSpace(src.Label); s != "" {
		return s
	}
	return strings.TrimSpace(src.Tag)
}

// findStateNodeByLink — узел состояния по ссылке {FolderID, сырой тег}.
//
// Зеркалит `wizardmodels.FindNodeByLink` (та же адресация, другой носитель):
// корневой узел адресуется тем именем, под которым его знает конфиг
// (NodeTagOrLabel) — ровно так его кладёт в канон проекция состояния.
func findStateNodeByLink(s *state.State, link state.NodeLink) *state.Node {
	if s == nil || link.Tag == "" {
		return nil
	}
	for i := range s.Sources {
		src := &s.Sources[i]
		if link.FolderID == "" {
			switch src.Kind {
			case state.SourceKindServer, state.SourceKindChain, state.SourceKindAuto:
				if src.NodeTagOrLabel() == link.Tag {
					return &src.Node
				}
			}
			continue
		}
		if src.ID != link.FolderID {
			continue
		}
		for j := range src.Nodes {
			if src.Nodes[j].Tag == link.Tag {
				return &src.Nodes[j]
			}
		}
	}
	return nil
}

// candidatePath — путь файла-кандидата РЯДОМ с config.json.
func candidatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), filepath.Base(configPath)+candidateConfigSuffix)
}

// writeCandidate пишет кандидата. Уборку делает вызывающий через defer:
// временные файлы не копятся ни при ошибке, ни при панике.
func writeCandidate(configPath string, data []byte) (string, error) {
	p := candidatePath(configPath)
	if err := os.WriteFile(p, data, platform.DefaultFileMode); err != nil {
		return "", fmt.Errorf("write candidate: %w", err)
	}
	return p, nil
}

// promoteCandidate — атомарная замена config.json кандидатом.
//
// `os.Rename` в пределах одного каталога атомарен: в худшем случае (сбой,
// потеря питания) на диске остаётся целый предыдущий config.json, и
// работающее ядро продолжает на нём.
func promoteCandidate(candidate, configPath string) error {
	if err := os.Rename(candidate, configPath); err != nil {
		return fmt.Errorf("promote candidate: %w", err)
	}
	return nil
}

// buildRevisionPath is where the digest of the last promoted config lives.
//
// A sibling of config.json rather than a field inside state.json, because the two
// files are written by different subsystems and a reader that has one must not
// need to parse the other's schema.
func buildRevisionPath(configPath string) string {
	return configPath + ".rev"
}

// writeBuildRevision records the digest of the config bytes that were promoted.
//
// Written through a temp file and a rename, like the config it describes. A
// truncate-in-place write interrupted partway leaves a malformed digest, which reads
// as "no revision recorded" and silently drops the staleness check back to the legacy
// mtime comparison — a self-inflicted degradation, in the same transaction as the
// config, avoidable by using the same discipline the config already uses.
func writeBuildRevision(configPath string, data []byte) error {
	path := buildRevisionPath(configPath)
	// Through the shared atomic writer, like the config it describes. This marker's whole
	// job is to be TRUSTWORTHY about the config's content, so a marker that can be left
	// half-written — or blended with a concurrent writer's — defeats its own purpose: a
	// torn digest matches nothing and reports the config as unverifiable.
	return atomicfile.Write(path, []byte(configContentDigest(data)), platform.DefaultFileMode)
}

// readBuildRevision returns the recorded digest, or "" when none was recorded.
//
// An empty result is NOT "unchanged": it means this install predates the marker,
// and the caller must fall back to the older check rather than assume freshness.
func readBuildRevision(configPath string) string {
	b, err := os.ReadFile(buildRevisionPath(configPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// configRevision returns the digest of the config currently on disk, or "" when it
// cannot be read.
//
// Reading the CONTENT rather than trusting the marker is what closes the gap the
// marker alone would leave: a config edited by hand, or by an older launcher, has
// no matching marker, and comparing recorded-against-recorded would call that
// pair equal while the bytes on disk had changed.
func configRevision(configPath string) string {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	return configContentDigest(b)
}

// configContentDigest is THE identity of a config's bytes.
//
// There were three implementations of "hash this config" — one here, one in
// `backend/service`, and one inline in the provenance marker — which is exactly the drift
// that makes a comparison report divergence where there is none: two digests of the same
// bytes disagree about nothing observable, and the only way to find out is to compare them.
// One function, so agreement is by construction.
func configContentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ConfigIsStaleVersusRecorded reports whether config.json differs from the config
// the last build promoted.
//
// Returns (stale, known). `known` is false when no revision was ever recorded, so
// the caller can fall back to the legacy mtime comparison instead of guessing.
func ConfigIsStaleVersusRecorded(configPath string) (stale bool, known bool) {
	recorded := readBuildRevision(configPath)
	if recorded == "" {
		return false, false
	}
	current := configRevision(configPath)
	if current == "" {
		// The config is unreadable: that is a problem for the caller to report,
		// not a freshness verdict.
		return false, false
	}
	return recorded != current, true
}

// removeCandidate — уборка за собой, best-effort.
func removeCandidate(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		debuglog.WarnLog("corereject: candidate %s not removed: %v", path, err)
	}
}

// checkFunc — проверка конфига ядром. Переменной, а не прямым вызовом:
// тест цикла подставляет сюда сценарий отказов вместо запуска `sing-box`
// (один комплексный тест на цикл — политика проверок кампании).
//
// Возврат nil = конфиг принят ЛИБО проверять было нечем (бинаря нет,
// graceful skip): различие видно вызывающему по checkAvailable.
type checkFunc func(singboxPath, configPath string) error

// coreRejectLoop — вход цикла страховки.
//
// rebuild(s) пересобирает конфиг из ПЕРЕДАННОГО состояния и отдаёт готовый
// JSON вместе с картой «финальный тег → узел состояния». Круг цикла = один
// её вызов после выключения очередного узла.
type coreRejectLoop struct {
	check      checkFunc
	singbox    string
	configPath string
	disabler   NodeDisabler
	decide     coreRejectDecider
	progress   coreRejectProgress
	// noPromote — не заменять config.json (превью Final: кандидат только
	// для check, боевой файл не трогаем).
	noPromote bool
	// forCheck — вид конфига, который уходит на check, если проверять сам
	// конфиг нельзя: у конфига удалённой машины rule_set[].path ведёт в ЕЁ
	// файловую систему, и локальное ядро такой файл не откроет. На check
	// идёт вид с нашими путями, в бой — исходный конфиг. nil = проверяется
	// сам конфиг.
	forCheck func([]byte) []byte
}

// coreRejectOutcome — итог прохода.
type coreRejectOutcome struct {
	// Promoted — config.json заменён принятым конфигом.
	Promoted bool
	// Disabled — узлы, выключенные этим проходом, в порядке выключения.
	Disabled []CoreRejectedNode
	// CheckErr — последний отказ ядра, если цикл кончился отказом. nil при
	// Promoted.
	CheckErr error
	// StoppedByHuman — человек нажал Stop в диалоге предела.
	StoppedByHuman bool
	// Rounds — сколько раз собирался конфиг (диагностика).
	Rounds int
}

// buildRound — результат одной сборки: готовый конфиг и карта тегов.
type buildRound struct {
	ConfigJSON []byte
	// NodeLinks — финальный тег → узел состояния. Строит СБОРКА, которая эти
	// теги и выдала: пересчитать путь снаружи нельзя (тег-политика с
	// переменными и суффикс уникализации раскрываются только эмиссией) —
	// PARSING_PRINCIPLES §9.3.
	NodeLinks map[string]state.NodeLink
}

// run прогоняет цикл: проверить кандидата → при отказе, назвавшем узел,
// выключить его и пересобрать → повторить.
//
// first — уже собранный конфиг первого круга (сборку делает вызывающий: у
// него на руках весь конвейер отчёта). rebuild зовётся только со второго
// круга, после выключения узла.
func (l *coreRejectLoop) run(first buildRound, rebuild func() (buildRound, error)) (coreRejectOutcome, error) {
	var out coreRejectOutcome
	round := first
	limit := coreRejectAskAfter
	unlimited := false
	// Теги, уже названные ядром: повторно названный тег прекращает
	// автоматику (PARSING_PRINCIPLES §9.5 запрет 3) — иначе цикл перестаёт быть конечным.
	named := make(map[string]bool, 8)

	for {
		out.Rounds++
		checkJSON := round.ConfigJSON
		if l.forCheck != nil {
			checkJSON = l.forCheck(round.ConfigJSON)
		}
		candidate, err := writeCandidate(l.configPath, checkJSON)
		if err != nil {
			return out, err
		}
		checkErr := l.check(l.singbox, candidate)
		if checkErr == nil {
			// Принят (или проверять было нечем — graceful skip, §5А SPEC 132:
			// страховка тогда не действует, запись идёт как раньше).
			if l.noPromote {
				removeCandidate(candidate)
				out.Promoted = true
				return out, nil
			}
			if l.forCheck != nil {
				// Проверялся вид с подменёнными путями — в бой идёт исходный.
				if _, err := writeCandidate(l.configPath, round.ConfigJSON); err != nil {
					removeCandidate(candidate)
					return out, err
				}
			}
			if err := promoteCandidate(candidate, l.configPath); err != nil {
				removeCandidate(candidate)
				return out, err
			}
			// Record a CONTENT identity for the bytes just promoted.
			//
			// This is what makes "is the config stale?" answerable. The old check
			// compared mtimes with a one-second tolerance, so a config edited
			// 500 ms after it was built looked fresh — the launcher never offered
			// the rebuild, and nothing else could notice, because mtime carries no
			// information about WHAT was written.
			//
			// Best-effort: a failure to write the marker must not fail a build that
			// already succeeded. A missing revision degrades the staleness check to
			// the legacy mtime comparison, which is exactly the behaviour before
			// this marker existed.
			if err := writeBuildRevision(l.configPath, round.ConfigJSON); err != nil {
				debuglog.WarnLog("coreRejectLoop: config written but its revision marker was not: %v", err)
			}
			// Record provenance in the same transaction as the promotion, so no
			// config can reach disk undescribed. See configPromotionHook.
			noteConfigPromoted(l.configPath, round.ConfigJSON)
			out.Promoted = true
			return out, nil
		}
		// Отказ: кандидат больше не нужен ни в одной ветке ниже.
		removeCandidate(candidate)
		out.CheckErr = checkErr

		rej, named2, stop := l.nextVictim(checkErr, round.NodeLinks, named)
		if stop {
			return out, nil
		}
		named = named2

		// Предел: спросить человека. Фоновый вход (decide == nil) идёт
		// дальше молча — до жёсткого потолка.
		if !unlimited && len(out.Disabled) >= limit {
			if l.decide == nil {
				if len(out.Disabled) >= coreRejectHardCap {
					debuglog.WarnLog("corereject: hard cap of %d disabled nodes reached — stopping", coreRejectHardCap)
					return out, nil
				}
				// Потолок ещё не достигнут: продолжаем, следующий вопрос —
				// через тот же шаг.
				limit += coreRejectAskAfter
			} else if l.decide(len(out.Disabled)) {
				unlimited = true // Keep checking снимает предел до конца прохода
			} else {
				out.StoppedByHuman = true
				return out, nil
			}
		}

		if !l.disabler.Disable(rej.link, rej.reason) {
			// Узел не нашёлся либо уже выключен этой же причиной: круг не
			// выключил НИЧЕГО нового, и цикл обязан остановиться.
			debuglog.WarnLog("corereject: tag %q named again but nothing was disabled — stopping", rej.tag)
			return out, nil
		}
		out.Disabled = append(out.Disabled, CoreRejectedNode{
			SourceLabel: l.disabler.Label(rej.link),
			Tag:         rej.tag,
			Reason:      rej.reason,
		})
		debuglog.WarnLog("corereject: the core rejected node %q — turned off (%s)", rej.tag, rej.reason)
		if l.progress != nil {
			l.progress(len(out.Disabled))
		}

		next, err := rebuild()
		if err != nil {
			return out, err
		}
		round = next
	}
}

type coreRejectVictim struct {
	link   state.NodeLink
	tag    string
	reason string
}

// nextVictim разбирает отказ ядра и находит узел, который он называет.
//
// stop=true — автоматики нет: ошибка не про узел, тег без узла, или тот же
// тег назван повторно. Во всех трёх случаях config.json НЕ заменяется, и
// вызывающий ведёт себя как раньше (диалог, ConfigStale остаётся).
func (l *coreRejectLoop) nextVictim(
	checkErr error,
	links map[string]state.NodeLink,
	named map[string]bool,
) (coreRejectVictim, map[string]bool, bool) {
	tags := make(map[string]bool, len(links))
	for tag := range links {
		tags[tag] = true
	}
	rej, ok := corereject.Parse(checkErr.Error(), corereject.TagsOf(tags))
	if !ok {
		debuglog.InfoLog("corereject: the core error does not name a node of ours — no automatic action")
		return coreRejectVictim{}, named, true
	}
	if named[rej.Tag] {
		debuglog.WarnLog("corereject: tag %q named twice — stopping (PARSING_PRINCIPLES §9.5)", rej.Tag)
		return coreRejectVictim{}, named, true
	}
	link, has := links[rej.Tag]
	if !has || link.Tag == "" {
		// Невозможно после Parse (он сопоставляет по тем же тегам), но
		// полагаться на это молча нельзя.
		return coreRejectVictim{}, named, true
	}
	named[rej.Tag] = true
	return coreRejectVictim{link: link, tag: rej.Tag, reason: strings.TrimSpace(rej.Text)}, named, false
}

// coreRejectCheck — боевая проверка кандидата. Отдельной функцией, чтобы
// цикл держал её переменной поля и тест подставлял сценарий вместо запуска
// ядра.
func coreRejectCheck(singboxPath, configPath string) error {
	return validateConfigViaSingBox(singboxPath, configPath)
}

// stateNodeLinks — карта «финальный тег → узел состояния» из результата
// эмиссии (SPEC 132, PARSING_PRINCIPLES §9.3).
//
// Перевод формы, а не пересчёт: карту строит сама эмиссия, потому что только
// она знает раскрытую тег-политику и суффикс глобальной уникализации.
func stateNodeLinks(res *config.OutboundGenerationResult) map[string]state.NodeLink {
	if res == nil || len(res.NodeLinks) == 0 {
		return nil
	}
	out := make(map[string]state.NodeLink, len(res.NodeLinks))
	for tag, link := range res.NodeLinks {
		out[tag] = state.NodeLink{FolderID: link.FolderID, Tag: link.Tag}
	}
	return out
}

// coreRejectedPayload — список выключенных в форму события.
func coreRejectedPayload(in []CoreRejectedNode) []events.DisabledNode {
	if len(in) == 0 {
		return nil
	}
	out := make([]events.DisabledNode, 0, len(in))
	for _, n := range in {
		out = append(out, events.DisabledNode{
			SourceLabel: n.SourceLabel, Tag: n.Tag, Reason: n.Reason,
		})
	}
	return out
}

// coreRejectDecider — кого спрашивать на пределе.
//
// Колбэк ставит UI (волна 5, `ui/core_rejected_notice.go`), и гейт «есть ли
// кого спрашивать» стоит ВНУТРИ него: цикл один на все входы, и различить
// нажатие Start от ночного автообновления подписок он не может — а UI может,
// по видимости главного окна.
//
// nil (UI не поставил колбэк вовсе — headless-сборка, тесты, ранний старт до
// NewApp) = поведение фонового входа: идём дальше молча до жёсткого потолка
// coreRejectHardCap. Решение по умолчанию для фоновых входов остаётся таким и
// после волны UI: подписка на 500 узлов с пачкой негодных обязана собраться
// ночью сама, а спросить там некого (§10.4 SPEC 132).
func (ac *AppController) coreRejectDecider() coreRejectDecider {
	if ac == nil {
		return nil
	}
	ac.coreRejectHooksMu.Lock()
	defer ac.coreRejectHooksMu.Unlock()
	return ac.coreRejectDecideHook
}

// coreRejectProgress — строка состояния «Checking servers… (%d disabled)»
// (§6.3 SPEC 132). Ставит UI; nil — никто не смотрит.
func (ac *AppController) coreRejectProgress() coreRejectProgress {
	if ac == nil {
		return nil
	}
	ac.coreRejectHooksMu.Lock()
	defer ac.coreRejectHooksMu.Unlock()
	return ac.coreRejectProgressHook
}

// SetCoreRejectDecider отдаёт циклу вопрос человеку на пределе (§6.2).
//
// Зовётся из UI один раз на сборке приложения. Под мьютексом: ставится из
// UI-потока, читается из фоновой горутины сборки.
func (ac *AppController) SetCoreRejectDecider(fn func(disabled int) bool) {
	if ac == nil {
		return
	}
	ac.coreRejectHooksMu.Lock()
	defer ac.coreRejectHooksMu.Unlock()
	if fn == nil {
		ac.coreRejectDecideHook = nil
		return
	}
	ac.coreRejectDecideHook = coreRejectDecider(fn)
}

// SetCoreRejectProgress отдаёт циклу колбэк хода (§6.3).
func (ac *AppController) SetCoreRejectProgress(fn func(disabled int)) {
	if ac == nil {
		return
	}
	ac.coreRejectHooksMu.Lock()
	defer ac.coreRejectHooksMu.Unlock()
	if fn == nil {
		ac.coreRejectProgressHook = nil
		return
	}
	ac.coreRejectProgressHook = coreRejectProgress(fn)
}

// RejectLoopInput — вход общего цикла для черновика Конфигуратора
// (Final и remote-Save). Боевой rebuild собирает loop сам.
type RejectLoopInput struct {
	SingboxPath string
	ConfigPath  string
	NoPromote   bool
	FirstJSON   []byte
	FirstLinks  map[string]state.NodeLink
	Rebuild     func() ([]byte, map[string]state.NodeLink, error)
	Disabler    NodeDisabler
	Decide      func(disabled int) bool
	Progress    func(disabled int)
	// ForCheck — см. coreRejectLoop.forCheck; nil = проверяется сам конфиг.
	ForCheck func([]byte) []byte
}

// RejectLoopResult — итог прохода для вызывающего вне core.
type RejectLoopResult struct {
	AcceptedJSON   []byte
	Disabled       []CoreRejectedNode
	Promoted       bool
	CheckErr       error
	StoppedByHuman bool
}

// RunRejectLoop гоняет тот же цикл, что и боевая сборка, с чужим выключателем.
func RunRejectLoop(in RejectLoopInput) (RejectLoopResult, error) {
	if in.Disabler == nil {
		return RejectLoopResult{}, fmt.Errorf("reject loop: no disabler")
	}
	if in.ConfigPath == "" {
		return RejectLoopResult{}, fmt.Errorf("reject loop: empty config path")
	}
	loop := &coreRejectLoop{
		check:      coreRejectCheck,
		singbox:    in.SingboxPath,
		configPath: in.ConfigPath,
		disabler:   in.Disabler,
		noPromote:  in.NoPromote,
		forCheck:   in.ForCheck,
	}
	if in.Decide != nil {
		loop.decide = coreRejectDecider(in.Decide)
	}
	if in.Progress != nil {
		loop.progress = coreRejectProgress(in.Progress)
	}
	lastJSON := in.FirstJSON
	rebuild := func() (buildRound, error) {
		if in.Rebuild == nil {
			return buildRound{}, fmt.Errorf("reject loop: rebuild not provided")
		}
		next, links, err := in.Rebuild()
		if err != nil {
			return buildRound{}, err
		}
		lastJSON = next
		return buildRound{ConfigJSON: next, NodeLinks: links}, nil
	}
	out, err := loop.run(buildRound{ConfigJSON: in.FirstJSON, NodeLinks: in.FirstLinks}, rebuild)
	return RejectLoopResult{
		AcceptedJSON:   lastJSON,
		Disabled:       out.Disabled,
		Promoted:       out.Promoted,
		CheckErr:       out.CheckErr,
		StoppedByHuman: out.StoppedByHuman,
	}, err
}

// CoreRejectDecideFn — колбэк предела, который поставил UI; nil = фоновый вход.
func (ac *AppController) CoreRejectDecideFn() func(int) bool {
	d := ac.coreRejectDecider()
	if d == nil {
		return nil
	}
	return func(n int) bool { return d(n) }
}

// CoreRejectProgressFn — колбэк хода, который поставил UI; nil = никто не смотрит.
func (ac *AppController) CoreRejectProgressFn() func(int) {
	p := ac.coreRejectProgress()
	if p == nil {
		return nil
	}
	return func(n int) { p(n) }
}

// DisableNodeNamedByCore выключает в СОХРАНЁННОМ состоянии узел, который
// назвала ошибка реального старта демона. false = тег не сопоставлен,
// ошибка не про узел, или записать не удалось — вызывающий ведёт себя как
// раньше (лог, сообщение).
func (ac *AppController) DisableNodeNamedByCore(errText string) bool {
	if ac == nil || ac.FileService == nil {
		return false
	}
	return ac.disableNodeNamedByCoreAt(errText, platform.GetWizardStatePath(ac.FileService.Layout.Data))
}

func (ac *AppController) disableNodeNamedByCoreAt(errText, statePath string) bool {
	if ac == nil || ac.FileService == nil || strings.TrimSpace(errText) == "" || statePath == "" {
		return false
	}
	errText = stripANSI(errText)
	s, err := state.Load(statePath)
	if err != nil {
		debuglog.WarnLog("corereject: load state for start reject: %v", err)
		return false
	}
	td, _, terr := ac.loadTemplateForBuild(ac.FileService.Layout)
	if terr != nil {
		debuglog.WarnLog("corereject: template for start reject: %v", terr)
		td = nil
	}
	_, parserRes, snapErr := buildSnapshotFromState(s, ac.FileService.Layout, nil, td)
	if snapErr != nil {
		debuglog.WarnLog("corereject: snapshot for start reject: %v", snapErr)
		return false
	}
	links := stateNodeLinks(parserRes)
	tags := make(map[string]bool, len(links))
	for tag := range links {
		tags[tag] = true
	}
	rej, ok := corereject.Parse(errText, corereject.TagsOf(tags))
	if !ok {
		return false
	}
	link, has := links[rej.Tag]
	if !has || link.Tag == "" {
		return false
	}
	d := &savedStateDisabler{s: s, path: statePath}
	if !d.Disable(link, strings.TrimSpace(rej.Text)) {
		return false
	}
	if err := d.Commit(); err != nil {
		debuglog.ErrorLog("corereject: start-reject disable not saved: %v", err)
		return false
	}
	debuglog.WarnLog("corereject: start named %q — turned off (%s)", rej.Tag, rej.Text)
	return true
}

// ReadBuildRevisionForService reports the generator revision recorded beside a
// config, for layers above core that record provenance.
//
// `known` is false when the install predates the marker: an empty revision is not
// evidence of anything, and a caller recording provenance must be able to say
// "unknown" rather than record an empty string as if it were a fact.
func ReadBuildRevisionForService(configPath string) (string, bool) {
	if configPath == "" {
		return "", false
	}
	rev := readBuildRevision(configPath)
	return rev, rev != ""
}

// configPromotionHook is invoked after a config is promoted, with the path and the
// exact bytes promoted.
//
// WHY A HOOK RATHER THAN ANOTHER CALLER-SIDE CALL. Provenance was recorded only in
// the rebuild paths that the service layer happened to remember (the maintenance
// reload and the explicit adoption). Every other writer — a classic start's pre-start
// rebuild, a daemon start, a restart's forced rebuild, the auto-rebuild after a
// subscription update, and the first start on a fresh install — promoted a config
// and recorded nothing. The fresh-install case was the sharpest: no config and no
// marker reads as "go ahead", the build creates config.json, still no marker, and
// the NEXT ownership question sees a config with no marker and answers UNKNOWN. The
// launcher disowned the file it had just created.
//
// Patching each caller is how that gap appeared in the first place, and every future
// writer would have to remember again. Recording at the PROMOTION POINT makes the
// config write and its provenance one transaction: there is no way to promote a
// config without describing it.
//
// The hook is a package variable set once at wiring time; it is read on the build
// path, which is serialised by buildMu, and never nil-checked on the hot path.
var (
	configPromotionHookMu  sync.Mutex
	configPromotionHooks   = map[uint64]func(configPath string, promoted []byte){}
	configPromotionHookSeq uint64
)

// SetConfigPromotionHook installs the callback invoked after every config promotion.
//
// Called once at startup by the layer that owns provenance. A nil hook means no
// recording, which is the correct behaviour for a Go-only build with no IPC layer.
func SetConfigPromotionHook(fn func(configPath string, promoted []byte)) {
	configPromotionHookMu.Lock()
	defer configPromotionHookMu.Unlock()
	if fn == nil {
		delete(configPromotionHooks, configPromotionHookSeq)
		return
	}
	// A MAP OF HOOKS, NOT A SLOT. A single package-level slot means the last install
	// wins: a second backend in the same process silently steals the hook, and the
	// first one's promotions are then either recorded against the wrong config path or
	// dropped by the path guard. Production has one backend today, which is exactly
	// why this is worth fixing now — the failure is invisible until the day it is not,
	// and every hook is handed the config path so it can decide for itself whether the
	// promotion is its business.
	//
	// Returns nothing, so callers cannot unregister selectively; a hook lives for the
	// process. That matches how it is installed (once, from New).
	configPromotionHookSeq++
	configPromotionHooks[configPromotionHookSeq] = fn
}

// noteConfigPromoted tells every hook about a promotion.
func noteConfigPromoted(configPath string, promoted []byte) {
	configPromotionHookMu.Lock()
	hooks := make([]func(string, []byte), 0, len(configPromotionHooks))
	for _, fn := range configPromotionHooks {
		hooks = append(hooks, fn)
	}
	configPromotionHookMu.Unlock()
	// Called OUTSIDE the lock: a hook reads the config and writes a marker, and
	// holding the registry while it does would serialise every promotion behind the
	// slowest hook for no reason.
	for _, fn := range hooks {
		fn(configPath, promoted)
	}
}

// NoteConfigPromotedForTest exercises the promotion announcement directly, so an
// upper layer's hook can be tested without running a full build.
func NoteConfigPromotedForTest(configPath string, promoted []byte) {
	noteConfigPromoted(configPath, promoted)
}
