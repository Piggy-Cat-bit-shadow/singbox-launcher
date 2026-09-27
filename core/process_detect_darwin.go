//go:build darwin

package core

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

// process_detect_darwin.go — опознание «своего» ядра на macOS.
//
// Здесь две вещи, которые легко перепутать и опасно перепутать:
//
//  1. Есть ли на машине ЖИВОЕ ядро (для диалога «sing-box уже запущен»).
//  2. Принадлежит ли конкретный PID ЭТОМУ лаунчеру (его можно снимать).
//
// Второе нельзя выводить из первого. Широкий шаблон вида
// `pkill -f "sing-box run|..."` бьёт по всем sing-box на машине, включая
// чужие сборки, другой профиль пользователя и ядро, запущенное не нами, —
// а это чужой работающий VPN. Поэтому идентичность подтверждается по
// executable path, а не по подстроке командной строки.

// procInfo — то, что нам нужно знать о процессе, чтобы решить, наш он или
// нет. Отдельная структура, потому что её заполняет и продакшн-проба, и
// тесты (в песочнице `/bin/ps` может быть недоступен).
type procInfo struct {
	PID  int
	Path string // полный путь исполняемого файла; "" — не выяснили
	Args string // командная строка, если доступна
}

// psPath — абсолютный путь к ps. Системные утилиты вызываются по
// абсолютному пути, чтобы результат не зависел от PATH пользователя
// (SPEC 137 §3: root и мы сами не доверяем PATH).
const psPath = "/bin/ps"

// listProcessDetailsDarwin перечисляет процессы с путями исполняемых файлов.
//
// `ps -axo pid=,comm=` даёт полный путь для нативных программ (в отличие от
// `ps -A`, где имя усечено). Возвращает ошибку, если ps недоступен: caller
// обязан трактовать это как «не знаю», а не как «не нашёл».
func listProcessDetailsDarwin() ([]procInfo, error) {
	cmd := exec.Command(psPath, "-axo", "pid=,comm=")
	platform.PrepareCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var procs []procInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " ")
		if line == "" {
			continue
		}
		space := strings.IndexByte(line, ' ')
		if space <= 0 {
			continue
		}
		pid, convErr := strconv.Atoi(line[:space])
		if convErr != nil || pid <= 0 {
			continue
		}
		path := strings.TrimSpace(line[space+1:])
		procs = append(procs, procInfo{PID: pid, Path: path})
	}
	return procs, nil
}

// pidExecutablePath возвращает путь исполняемого файла процесса.
//
// Порядок: сначала ps (даёт путь вместе с остальным списком), затем прямое
// чтение через системный вызов. Ошибка означает «не смогли выяснить», и это
// НЕ «наш»: неопознанный процесс нельзя ни присваивать, ни убивать.
func pidExecutablePath(pid int, procs []procInfo) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	for _, p := range procs {
		if p.PID == pid {
			if p.Path == "" {
				return "", false
			}
			return p.Path, true
		}
	}
	return "", false
}

// readPrivilegedPidFile читает pid-файл привилегированного запуска и
// возвращает найденные PID. Формат — строка на PID (PID шелла, затем PID
// ядра), поэтому файл может содержать несколько.
//
// Нужен, чтобы отличить СВОЁ ядро от чужого (SPEC 144): после перезапуска
// лаунчера в памяти нет ни Cmd, ни privileged-PID, и проверка «sing-box уже
// запущен» принимала живое ядро прошлой сессии за чужой процесс и
// предлагала его убить. Для пользователя это выглядело как предложение
// снести работающий VPN при обычном открытии клиента.
func readPrivilegedPidFile(path string) []int {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(data)) {
		if pid, convErr := strconv.Atoi(field); convErr == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processAlive сообщает, живёт ли процесс с этим PID. Signal 0 не
// доставляет сигнал, а только проверяет право послать его: для своего
// процесса это и есть проверка существования.
//
// ВНИМАНИЕ: это только «PID занят». Он мог быть переиспользован другим
// процессом. Для решений об убийстве/присвоении нужен ownerMatchesPID.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// pidMatchesCoreCopy — подтверждает, что процесс по этому пути — именно
// СВОЯ root-owned копия ядра (или сам бинарь лаунчера, если копии нет).
//
// Сравниваются РАЗРЕШЁННЫЕ пути: и цель, и путь процесса прогоняются через
// EvalSymlinks, иначе сравнение обходится симлинком. Пустая цель означает,
// что мы не знаем, где наша копия, — тогда совпадения нет.
func pidMatchesCoreCopy(procPath, expectedCorePath string) bool {
	if procPath == "" || expectedCorePath == "" {
		return false
	}
	got := resolveForCompare(procPath)
	want := resolveForCompare(expectedCorePath)
	if got == "" || want == "" {
		return false
	}
	return got == want
}

// resolveForCompare — EvalSymlinks с запасным вариантом Clean: файл мог уже
// исчезнуть, но сравнить пути всё равно нужно детерминированно.
func resolveForCompare(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// coreIdentity — описание того, чем должно быть наше ядро: путь к
// защищённой копии и путь к бинарю лаунчера. Любой из них — законная
// личность «своего» процесса.
type coreIdentity struct {
	// CopyPath — root-owned копия, которую исполняет привилегированный старт.
	CopyPath string
	// LauncherCorePath — ядро лаунчера (data/bin/sing-box).
	LauncherCorePath string
}

// matches — процесс с таким executable path принадлежит нам.
func (id coreIdentity) matches(procPath string) bool {
	if pidMatchesCoreCopy(procPath, id.CopyPath) {
		return true
	}
	if pidMatchesCoreCopy(procPath, id.LauncherCorePath) {
		return true
	}
	return false
}

// ownCorePID — PID нашего ядра среди перечисленных процессов, или -1.
//
// Требуется СОВПАДЕНИЕ ОБОИХ: PID из нашего pid-файла И executable path,
// равный нашей копии/ядру. Одного лишь «PID жив» недостаточно: номера
// переиспользуются, и запись в старом pid-файле легко указывает на
// посторонний процесс (браузер, системную службу) — присвоить или убить
// его было бы ошибкой.
//
// procs == nil и psErr != nil означает «список процессов недоступен»;
// тогда ответ -1 (не знаем), а не «наш».
func ownCorePID(pidFile string, id coreIdentity, procs []procInfo, psErr error) int {
	pids := readPrivilegedPidFile(pidFile)
	if len(pids) == 0 {
		return -1
	}
	if psErr != nil {
		// Не можем подтвердить личность — не присваиваем ничего.
		debuglog.DebugLog("ownCorePID: cannot list processes (%v); refusing to claim any PID", psErr)
		return -1
	}
	for i := len(pids) - 1; i >= 0; i-- {
		pid := pids[i]
		if !processAlive(pid) {
			continue
		}
		path, ok := pidExecutablePath(pid, procs)
		if !ok {
			debuglog.DebugLog("ownCorePID: PID %d is alive but its executable path is unknown; not claiming it", pid)
			continue
		}
		if id.matches(path) {
			return pid
		}
		debuglog.WarnLog("ownCorePID: PID %d from the pid file is %q, which is not our core; treating the pid file as stale", pid, path)
	}
	return -1
}

// findSingboxRunProcessDarwin ищет ЛЮБОЙ запущенный sing-box (для диалога
// «уже запущен»). Это только обнаружение: результат нельзя использовать для
// убийства без проверки личности (см. coreIdentity).
//
// Ищем по argv через pgrep -f, потому что чужой sing-box может быть
// запущен из любого места. Возвращает (found, pid, err); err != nil —
// pgrep недоступен.
func findSingboxRunProcessDarwin() (bool, int, error) {
	out, err := exec.Command("/usr/bin/pgrep", "-f", platform.PrivilegedPkillPattern).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, -1, nil // exit 1 = совпадений нет
		}
		return false, -1, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && pid > 0 {
			return true, pid, nil
		}
	}
	return false, -1, nil
}

// findPrivilegedCopyInUserSession — только Windows (SPEC 141 §8).
func findPrivilegedCopyInUserSession() int { return -1 }
