# PLAN 144 — UI modernization

## Phase 0 — Audit (готово)

### Инвентарь страниц (по факту исходников)

| Root | Источник | Реальные подразделы |
|---|---|---|
| Local | `CreateLocalTab` + `CreateCoreDashboardTab` | Overview / Proxies / Traffic |
| Remote | `CreateRemoteTab` + `CreateMachineListPanel` | Proxies / Machines |
| Diagnostics | `CreateDiagnosticsTab` | Logs & Maintenance / Network / Renderer |
| Settings | `BuildSettingsContent` | Language / Connection / Subscriptions / Storage |
| Help | `CreateHelpTab` | About |

Разделы выведены из фактической разметки `BuildSettingsContent`
(`langRow`, `connBlock`, `subsTitle+uaRow+subDefaultsBlock`,
`subIDTitle+subIDBlock`, `debugAPIBlock`, `storageBlock`) и
`CreateDiagnosticsTab` (`logWindowRow`/`foldersRow`/`cleanRuleSetsButton`/
`killRow`/`trafficProfilerBtn`/`mesaBtn`; `stunRow`/`ipServicesRow`).

Конфигуратор (`ui/configurator/**`) — отдельный overlay-визард, в
сайдбар не заводится.

### Карта побочных эффектов (R3)

`ui/app.go` → `app.tabs.OnSelected`. Точный состав и порядок зафиксированы
в SPEC R3. Выносится в `App.selectSection(id)` без изменения тела.

### Проблемы размеров

- `MinWindowSize = 395 + 165 = 560` (комментарий обещает 1000).
- `rightColumnWidth = 165` — узко для dashboard.
- `minSplitLayout` держит `applied`, чтобы не отменять пользовательский
  divider — поведение сохраняем.

## Phase 1 — Design system (`ui/design/`)

Новый пакет, только константы и хелперы, без бизнес-логики.

- `metrics.go` — spacing (4-й ритм), radius, размеры сайдбара/строк,
  ширины окна.
- `typography.go` — `PageTitle/Heading/Section/Body/Caption` через
  `widget.Label` с `TextStyle` и `theme.SizeName*` где применимо.
- `colors.go` — semantic-цвета поверх `theme.ColorName*` с фолбэком,
  light/dark через `theme.Variant()`.
- `ui/icons/` — расширение SVG-набора: `nav_local`, `nav_remote`,
  `nav_diagnostics`, `nav_settings`, `nav_help`, `chevron_right/down`,
  `status_dot`. Все themed (`theme.NewThemedResource`).

## Phase 2 — Shell (`ui/design/sidebar.go`, `ui/shell.go`)

- `Sidebar` как композиция (не кастомный Renderer): `VBox` из
  `SidebarItem`-ов, каждый — `container.NewStack` c фоном-hover/selected
  и accent-полосой слева.
- `SidebarItem` — обёртка над `widget.Button` c `Importance: Low` либо
  `fynewidget.HoverRow` + `Tappable`; hover/selected только перекрашивают
  уже созданные объекты (R-производительность).
- `AppPage` — заголовок + подзаголовок + тело.

## Phase 3 — Submenu

Свёртка/разворот — чисто визуальные. Свёрнутый родитель сохраняет
selected, если активен его ребёнок. Состояние — только в памяти.

## Phase 4–8 — Постранично

Local → Remote → Diagnostics → Settings → Help → Wizard visuals.
Каждый шаг — отдельная сборка.

## Phase 9 — Cleanup

Удаление emoji-навигации, `indexEmojiSep`, старых констант ширин.

## Инварианты, проверяемые после каждого шага

1. `go build ./...`
2. `AppTabs`-путь и Sidebar-путь исполняют один и тот же `selectSection`.
3. Ни один обработчик не потерян (сверка с инвентарём).
4. Нет новых связей UI → core/network в обход контроллера.
