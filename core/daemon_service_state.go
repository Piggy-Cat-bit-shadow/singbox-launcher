//go:build darwin || (windows && !386)

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/lxdclient"
)

// Классификатор состояния службы демона (SPEC 136), общая часть.
//
// Ядро на `lxd --service=install` копирует себя в защищённую копию службы и
// переводит определение службы на неё (SPEC 100 форка). Лаунчер ничего не
// копирует: он только читает определение службы, проверяет цепочку владения
// копии и сверяет sha256 копии с ядром лаунчера и с тем, что отвечает
// работающий демон. Всё читается без повышенных прав.
//
// Здесь — то, что не зависит от ОС: состояния и гейт команд по версии ядра,
// обход цепочки владения, сверка sha, вердикт по процессу, кэши sha и
// версии. Платформенное — в daemon_service_state_<os>.go: раскладка
// (systemDaemonServiceLayout, daemonServiceCorePath, сайдкар), чтение
// определения службы (inspectDaemonServiceDefinition), проверка звена
// цепочки (checkRootOwnedEntry), ключ файла для кэшей (fileHashKey,
// statHashKey) и состояние у менеджера служб (compareDaemonServiceRunning).

const (
	// daemonHashCacheCap — потолок кэша sha256: файлов в игре два-три, потолок
	// лишь не даёт кэшу расти от череды заменённых ядер.
	daemonHashCacheCap = 16
)

// DaemonServiceState — вердикт классификатора службы (SPEC 136 §4).
type DaemonServiceState string

const (
	// DaemonServiceNotInstalled — plist службы нет.
	DaemonServiceNotInstalled DaemonServiceState = "not_installed"
	// DaemonServiceUnsafe — root запускает файл, который может подменить
	// пользователь: plist не на каноническую копию, либо цепочка владения
	// копии нарушена, либо plist не разобрался.
	DaemonServiceUnsafe DaemonServiceState = "unsafe"
	// DaemonServiceStale — копия безопасна, но это не ядро лаунчера (sha
	// разные) или её нет вовсе.
	DaemonServiceStale DaemonServiceState = "stale"
	// DaemonServiceNotRunning — на диске всё в порядке (plist на безопасную
	// копию, sha совпал), но launchd службу не держит: не загружена или
	// state ≠ running. Лечится загрузкой plist (bootstrap), не
	// переустановкой. Пара к вердикту NOT RUNNING (exit 5) `lxd
	// --service=status` ядра.
	DaemonServiceNotRunning DaemonServiceState = "not_running"
	// DaemonServiceProcessStale — файл совпал, но работающий демон запущен из
	// другого образа (не перезапущен после обновления копии).
	DaemonServiceProcessStale DaemonServiceState = "process_stale"
	// DaemonServiceOK — служба запускает актуальную root-owned копию.
	DaemonServiceOK DaemonServiceState = "ok"
)

// DaemonServiceCheck — вердикт и его основания. Detail — английская причина
// для лога и Debug API; UI собирает свой текст из полей.
type DaemonServiceCheck struct {
	State DaemonServiceState
	// ServicePath — ProgramArguments[0] из plist ("" — plist не разобрался).
	ServicePath string
	Detail      string
	// CopyMissing — Stale потому, что копии нет (каталоги целы).
	CopyMissing bool
	// MismatchFile — Stale по члену набора, кроме главного бинаря (Windows,
	// SPEC 141 §6.2): имя расходящегося файла (`libcronet.dll`) или лишнего
	// файла в каталоге копии; ExtraFile — это лишний файл. Пусто — расходится
	// сам бинарь (CopySHA256/LauncherSHA256) или вердикт не Stale.
	MismatchFile string
	ExtraFile    bool
	// CopySHA256 / LauncherSHA256 — hex sha256 копии и ядра лаунчера; пусто,
	// если не считались.
	CopySHA256     string
	LauncherSHA256 string
	// CopyVersion — версия из сайдкара install.json (только показ),
	// LauncherVersion — `sing-box version` ядра лаунчера: только для показа
	// и для запасного пути ProcessStale (ядро без executable_sha256). На
	// право установить службу не влияет.
	CopyVersion     string
	LauncherVersion string
	// RunningSHA256 / RunningVersion — что отвечает работающий демон
	// (/admin/info); пусто, если не спрашивали или поля нет.
	RunningSHA256  string
	RunningVersion string
	// LaunchdState — что менеджер служб ОС говорит о службе: macOS — значение
	// `state = …` launchd или «not loaded»; Windows — CurrentState у SCM
	// (`stopped`, `start_pending`, `stop_pending`, `running`, …). Пусто, если
	// не спрашивали или спросить не удалось.
	LaunchdState string
}

// NeedsInstall — состояние лечится командой «Install or update service».
func (c DaemonServiceCheck) NeedsInstall() bool {
	switch c.State {
	case DaemonServiceUnsafe, DaemonServiceStale, DaemonServiceProcessStale:
		return true
	}
	return false
}

// NeedsBootstrap — служба установлена верно, но не запущена: лечится
// `launchctl bootstrap` (DaemonBootstrapCommand), а не install.
func (c DaemonServiceCheck) NeedsBootstrap() bool {
	return c.State == DaemonServiceNotRunning
}

// CopyUsable — plist указывает на каноническую копию, её цепочка владения
// цела и файл на месте: Uninstall и `lxd client add` можно звать через неё.
func (c DaemonServiceCheck) CopyUsable() bool {
	return c.State != DaemonServiceNotInstalled && c.State != DaemonServiceUnsafe && !c.CopyMissing
}

// InstallSupported — можно ли показывать команду install (и copy SPEC 137).
//
// Всегда true, когда платформа вообще реализует daemon-службу: этот тип
// строится только на daemon-платформах, поэтому «supported» здесь означает
// ровно «ОС умеет службу», а НЕ «ядро лаунчера нам нравится».
//
// Раньше здесь стоял гейт по версии ядра (root-owned копия требует
// ≥ minCoreForRootOwnedService с номером -lx.N). Гейт убран намеренно:
// версия ядра — не граница доверия, а лаунчер и ядро сопровождает один
// владелец. Сумеет ли конкретное ядро исполнить подкоманду `lxd` — выясняет
// сам запуск, и его ошибка показывается пользователю как есть. См.
// serviceCoreGate.
func (c DaemonServiceCheck) InstallSupported() bool {
	return true
}

// gateServiceInstall — оставлен как точка расширения классификатора.
//
// Исторически здесь вердикт, который лечит install, превращался в
// CoreTooOld, если ядро лаунчера ниже порога root-owned копии. Версионного
// гейта больше нет: любая версия ядра (в том числе пустая, `unknown`,
// `custom-build`, `1.15.0-jiejie-masquerade.5`) допускается к установке
// службы. Состояние DaemonServiceCoreTooOld не выставляется.
//
// Права и владение не ослаблены: install по-прежнему кладёт root-owned копию
// в /Library/PrivilegedHelperTools с root:wheel 0755 через sudo, а mTLS,
// пин отпечатка и парное сопряжение работают как прежде. Убран только
// предварительный «аудит происхождения» ядра.
func gateServiceInstall(_ *DaemonServiceCheck) {}

// serviceCoreGate — nil всегда: гейт версии ядра снят намеренно.
//
// Раньше это был единственный страж всех команд, исполняющих ядро лаунчера
// под sudo (install/copy службы). Он возвращал ошибку, если версия ядра не
// разбиралась как пронумерованный релиз форка (`-lx.N`) либо была ниже
// minCoreForRootOwnedService. Теперь решение принимает сам запуск: лаунчер не
// занимается аудитом происхождения ядра, он просто исполняет команду, а
// настоящая ошибка (`unknown command "lxd"`, отказ sudo, сбой записи)
// возвращается пользователю как есть.
//
// Всё, что защищает систему по существу, остаётся на месте и от версии не
// зависит: root-owned копия в /Library/PrivilegedHelperTools, root:wheel 0755,
// sudo, канонический plist LaunchDaemon, mTLS с пином отпечатка, парное
// сопряжение.
//
// Параметр сохранён, чтобы вызывающие не менялись и в логах можно было
// показать, о какой версии шла речь, если гейт когда-нибудь вернут.
func serviceCoreGate(_ string) error { return nil }

// daemonServiceLayout — где классификатор ищет службу. Прод —
// systemDaemonServiceLayout (платформенный); тест строит свою раскладку во
// временном каталоге от собственного uid.
type daemonServiceLayout struct {
	PlistPath string
	CorePath  string
	// LegacyPath — копия ранней раскладки lx.11 (<label>/ или <label>).
	LegacyPath string
	ChainRoot  string
	OwnerUID   uint32
}

// errDaemonCopyMissing — звено цепочки или сама копия отсутствует, а всё,
// что выше, root-owned: создать недостающее пользователь не может, значит
// это не дыра, а служба без бинаря (Stale).
var errDaemonCopyMissing = errors.New("root-owned copy is missing")

// checkRootOwnedChain проверяет каждое звено от root вниз до file по Lstat:
// не симлинк, владелец ownerUID, без записи для группы и остальных;
// каталоги — каталоги, file — обычный файл. Отсутствующее звено под целым
// родителем — errDaemonCopyMissing.
func checkRootOwnedChain(file, root string, ownerUID uint32) error {
	file = filepath.Clean(file)
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, filepath.Dir(file))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside %s", file, root)
	}
	chain := []string{root}
	if rel != "." {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			chain = append(chain, cur)
		}
	}
	for _, dir := range chain {
		if err := checkRootOwnedEntry(dir, ownerUID, true); err != nil {
			return err
		}
	}
	return checkRootOwnedEntry(file, ownerUID, false)
}

// compareDaemonServiceFiles — второй шаг: sha256 копии против ядра лаунчера
// (после EvalSymlinks: в DataDir бывает ссылка на dev-сборку). Работает
// только поверх OK; не посчитался любой из хэшей — вердикт не выносится.
func compareDaemonServiceFiles(c *DaemonServiceCheck, corePath, launcherCore string, hashes *fileHashCache) {
	if c.State != DaemonServiceOK {
		return
	}
	copySum, err := hashes.sum(corePath)
	if err != nil {
		debuglog.DebugLog("daemon service: hash %s: %v", corePath, err)
		return
	}
	c.CopySHA256 = copySum
	if launcherCore == "" {
		return
	}
	resolved, err := filepath.EvalSymlinks(launcherCore)
	if err != nil {
		debuglog.DebugLog("daemon service: launcher core %s: %v", launcherCore, err)
		return
	}
	launcherSum, err := hashes.sum(resolved)
	if err != nil {
		debuglog.DebugLog("daemon service: hash %s: %v", resolved, err)
		return
	}
	c.LauncherSHA256 = launcherSum
	if launcherSum != copySum {
		c.State = DaemonServiceStale
		c.Detail = fmt.Sprintf("the root-owned copy (sha256 %s) is not the launcher core %s (sha256 %s)",
			shortSHA(copySum), resolved, shortSHA(launcherSum))
		return
	}
	// Остальные члены набора (Windows: libcronet.dll) и лишние файлы в
	// каталоге копии (SPEC 141 §6.2); не посчитался хэш — не судим.
	name, extra, detail, err := daemonSetMismatch(corePath, resolved, hashes)
	if err != nil {
		debuglog.DebugLog("daemon service: copy set: %v", err)
		return
	}
	if name != "" {
		c.State = DaemonServiceStale
		c.MismatchFile = name
		c.ExtraFile = extra
		c.Detail = detail
	}
}

// classifyDaemonServiceFiles — вердикт по файлам без сети (SPEC 136 §4):
// определение службы, цепочка владения, sha копии против ядра лаунчера и
// гейт команды install по версии ядра лаунчера (launcherVersion; "" — не
// прочиталась).
func classifyDaemonServiceFiles(l daemonServiceLayout, launcherCore, launcherVersion string, hashes *fileHashCache) DaemonServiceCheck {
	check := inspectDaemonServiceDefinition(l)
	if check.CopyUsable() {
		check.CopyVersion = readDaemonServiceSidecarVersion(l.CorePath)
	}
	check.LauncherVersion = launcherVersion
	compareDaemonServiceFiles(&check, l.CorePath, launcherCore, hashes)
	gateServiceInstall(&check)
	return check
}

// compareDaemonServiceProcess — третий шаг: паспорт работающего демона.
// executable/executable_sha256 появились в lx.11; у старого ядра их нет, и
// запасной путь — версия (пустая и "unknown" вердикта не дают). Пустой
// executable_sha256 — «неизвестно» и у lx.11: хэш считается в фоне после
// старта демона, и до готовности поле пустое. ProcessStale по нему не
// выносится — судит версия.
func compareDaemonServiceProcess(c *DaemonServiceCheck, info lxdclient.InfoData, corePath string) {
	c.RunningSHA256 = info.ExecutableSHA256
	c.RunningVersion = info.Version
	if c.State != DaemonServiceOK {
		return
	}
	if info.Executable != "" && !sameServicePath(info.Executable, corePath) {
		c.State = DaemonServiceProcessStale
		c.Detail = fmt.Sprintf("the running daemon was started from %s, the service runs %s", info.Executable, corePath)
		return
	}
	if info.ExecutableSHA256 != "" { // "" — ядро до lx.11 или хэш ещё считается
		if c.CopySHA256 != "" && !strings.EqualFold(info.ExecutableSHA256, c.CopySHA256) {
			c.State = DaemonServiceProcessStale
			c.Detail = fmt.Sprintf("the running daemon (sha256 %s) is not the service binary (sha256 %s)",
				shortSHA(info.ExecutableSHA256), shortSHA(c.CopySHA256))
		}
		return
	}
	if versionComparable(info.Version) && versionComparable(c.LauncherVersion) && info.Version != c.LauncherVersion {
		c.State = DaemonServiceProcessStale
		c.Detail = fmt.Sprintf("the running daemon reports version %s, the launcher core is %s", info.Version, c.LauncherVersion)
	}
}

// versionComparable — версия, по которой можно судить: dev-сборка
// репортит "unknown".
func versionComparable(v string) bool {
	return v != "" && v != "unknown"
}

// shortSHA — первые 12 hex-символов для лога и UI.
func shortSHA(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// readDaemonServiceSidecarVersion — версия из сайдкара копии corePath
// (<копия>.install.json); "" если сайдкара нет (ядро до lx.11) или он не
// разобрался. Только показ.
func readDaemonServiceSidecarVersion(corePath string) string {
	sidecarPath := daemonServiceSidecarPath(corePath)
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return ""
	}
	var sidecar struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		debuglog.DebugLog("daemon service: %s: %v", sidecarPath, err)
		return ""
	}
	return sidecar.Version
}

// fileHashCache — sha256 файлов по ключу файла (fileHashKey платформы). Классификатор
// зовут на каждом открытии окна и перед каждым apply, а ядро весит десятки
// мегабайт.
type fileHashCache struct {
	mu       sync.Mutex
	sums     map[fileHashKey]string
	computed int // сколько раз файл читался целиком (для теста)
}

// daemonServiceHashes — кэш процесса для классификатора службы.
var daemonServiceHashes fileHashCache

// sum возвращает hex sha256 файла, из кэша при неизменном ключе. Файл,
// изменившийся во время чтения, не кешируется.
func (h *fileHashCache) sum(path string) (string, error) {
	key, err := statHashKey(path)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	cached, ok := h.sums[key]
	h.mu.Unlock()
	if ok {
		return cached, nil
	}
	sum, err := sha256File(path)
	if err != nil {
		return "", err
	}
	after, statErr := statHashKey(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.computed++
	if statErr != nil || after != key {
		return sum, nil
	}
	if h.sums == nil || len(h.sums) >= daemonHashCacheCap {
		h.sums = make(map[fileHashKey]string)
	}
	h.sums[key] = sum
	return sum, nil
}

// coreVersionCache — `sing-box version` по идентичности файла (ключ как у
// fileHashCache). Гейт команд службы зовут и из диалога после скачивания
// ядра — раньше, чем сбрасывается сессионный кэш GetInstalledCoreVersion, —
// а dev-сборки кладут руками: версия обязана быть версией файла на диске
// сейчас, а не первой за сессию.
type coreVersionCache struct {
	mu       sync.Mutex
	versions map[fileHashKey]string
}

// daemonCoreVersions — кэш процесса для гейта команд службы.
var daemonCoreVersions coreVersionCache

// version — версия ядра path, из кэша при неизменном ключе. Ошибка
// (файла нет, вывод не разобрался) не кешируется.
func (vc *coreVersionCache) version(path string) (string, error) {
	key, err := statHashKey(path)
	if err != nil {
		return "", err
	}
	vc.mu.Lock()
	cached, ok := vc.versions[key]
	vc.mu.Unlock()
	if ok {
		return cached, nil
	}
	version, err := coreVersionAt(path)
	if err != nil {
		return "", err
	}
	after, statErr := statHashKey(path)
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if statErr != nil || after != key {
		return version, nil
	}
	if vc.versions == nil || len(vc.versions) >= daemonHashCacheCap {
		vc.versions = make(map[fileHashKey]string)
	}
	vc.versions[key] = version
	return version, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
