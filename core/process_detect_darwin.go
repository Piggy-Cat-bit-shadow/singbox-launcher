//go:build darwin

package core

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"singbox-launcher/internal/platform"
)

// findSingboxRunProcessDarwin ищет запущенный sing-box по ПОЛНОЙ командной
// строке (pgrep -f), тем же паттерном, которым privileged-путь его убивает:
// `sing-box run|start-singbox-privileged`. В отличие от скана по имени
// бинаря, не срабатывает на демон `sing-box lxd` и на `sing-box check`.
//
// Возвращает (found, pid, err); err != nil — pgrep недоступен, caller делает
// fallback на имя-ориентированный скан.
func findSingboxRunProcessDarwin() (bool, int, error) {
	out, err := exec.Command("pgrep", "-f", platform.PrivilegedPkillPattern).Output()
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

// findOwnPrivilegedCorePID возвращает PID ядра, которое запустил ЭТОТ
// лаунчер в прошлой сессии (по pid-файлу), или -1. Такое ядро не «чужой
// sing-box»: его не предлагают убить, а считают уже работающим. Побочный
// эффект: stale pid-файл (процесса нет) даёт -1 и не мешает.
func findOwnPrivilegedCorePID(pidFile string) int {
	pids := readPrivilegedPidFile(pidFile)
	if len(pids) == 0 {
		return -1
	}
	// Последняя строка — PID ядра, предыдущая — PID шелла-обёртки; живым
	// достаточно любого: обёртка живёт ровно столько, сколько ядро.
	for i := len(pids) - 1; i >= 0; i-- {
		if processAlive(pids[i]) {
			return pids[i]
		}
	}
	return -1
}
