package core

import (
	"os"
	"path/filepath"

	"singbox-launcher/core/template"
	"singbox-launcher/internal/paths"
	"singbox-launcher/internal/platform"
)

// PathsInfo — блок путей раскладки (SPEC 135 §4.1) для раздела Storage,
// Copy paths и GET /debug/paths. Ядро берётся из FileService — то, что
// лаунчер реально запускает, а не свежий пересчёт цепочки. Версия ядра —
// только уже известная контроллеру: бинарь ради неё не запускается.
func (ac *AppController) PathsInfo() paths.PathsInfo {
	if ac == nil || ac.FileService == nil {
		return paths.PathsInfo{}
	}
	fs := ac.FileService
	// ONE snapshot for all four values. They are the pieces of a single resolution, and a
	// concurrent ResolveCore between two of these reads would describe a core that never
	// existed — the new path attributed to the old source, or a wintun path belonging to a
	// different core than the one reported.
	path, source, shadowed, wintun := fs.CoreResolutionFull()
	info := pathsInfo(fs.Layout, platform.CoreResolution{
		Path:     path,
		Source:   source,
		Shadowed: shadowed,
	}, wintun, ac.knownCoreVersion())
	ac.fillRootCopyStatus(&info)
	return info
}

// PathsInfoFor — лёгкая сборка блока до контроллера (флаг -paths): цепочка
// ядра и шаблона считаются заново, версии ядра нет.
func PathsInfoFor(l paths.Layout) paths.PathsInfo {
	r := platform.ResolveSingboxExecPath(l, os.Getenv)
	return pathsInfo(l, r, platform.GetWintunPathFor(filepath.Dir(r.Path)), "")
}

func pathsInfo(l paths.Layout, core platform.CoreResolution, wintun, version string) paths.PathsInfo {
	tmpl := template.ResolveTemplate(l)
	info := paths.PathsInfo{
		Layout:         l,
		CorePath:       core.Path,
		CoreSource:     core.Source,
		CoreVersion:    version,
		ShadowedCore:   core.Shadowed,
		TemplatePath:   tmpl.Path,
		TemplateSource: tmpl.Source,
		WintunPath:     wintun,
	}
	if wintun != "" {
		if st, err := os.Stat(wintun); err == nil && st.Mode().IsRegular() {
			info.WintunFound = true
		}
	}
	return info
}

// knownCoreVersion — версия ядра из сессионного кэша GetInstalledCoreVersion;
// "" если её ещё никто не спрашивал.
func (ac *AppController) knownCoreVersion() string {
	ac.installedCoreVersionCacheMu.Lock()
	defer ac.installedCoreVersionCacheMu.Unlock()
	return ac.installedCoreVersionCache
}
