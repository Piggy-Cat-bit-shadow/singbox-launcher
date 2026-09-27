# TASKS 144 — UI modernization

## Phase 0 — Audit
- [x] Инвентарь root-страниц и их подразделов из исходников
- [x] Карта побочных эффектов `OnSelected`
- [x] Замер проблем размера окна (`560` vs комментарий `1000`)
- [x] SPEC/PLAN заведены

## Phase 1 — Design system
- [x] `ui/design/metrics.go` — spacing/radius/size токены
- [x] `ui/design/typography.go` — уровни шрифта
- [x] `ui/design/colors.go` — semantic-цвета (light/dark)
- [x] `ui/icons/` — SVG-набор навигации и шевронов

## Phase 2 — Shell
- [x] `Sidebar` / `SidebarItem` компоненты
- [x] `AppPage` (заголовок + тело)
- [x] `App.selectSection(id)` — вынос тела `OnSelected` без изменений
- [x] Подключить shell в `NewApp`

## Phase 3 — Submenu
- [x] Раскрытие/сворачивание без смены активной страницы
- [x] Selected у свёрнутого родителя
- [x] Навигация по подпунктам

## Phase 4 — Local
- [x] Overview / Proxies / Traffic
- [x] Пропорции колонок вместо 395/165

## Phase 5 — Remote
- [x] Machines / Proxies

## Phase 6 — Settings / Diagnostics / Help
- [x] Settings: Language / Connection / Subscriptions / Storage
- [x] Diagnostics: Logs & Maintenance / Network / Renderer
- [x] Help: About

## Phase 7 — Dialogs
- [x] Единые ширина/отступы

## Phase 8 — Wizard visuals
- [x] Только presentation

## Phase 9 — Cleanup + docs
- [x] Удалить emoji-навигацию и мёртвые константы
- [x] `docs/ARCHITECTURE.md`, `docs/release_notes/upcoming.md`
- [x] IMPLEMENTATION_REPORT.md
