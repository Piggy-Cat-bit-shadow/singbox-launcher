# TASKS 144 — UI modernization

## Phase 0 — Audit
- [x] Инвентарь root-страниц и их подразделов из исходников
- [x] Карта побочных эффектов `OnSelected`
- [x] Замер проблем размера окна (`560` vs комментарий `1000`)
- [x] SPEC/PLAN заведены

## Phase 1 — Design system
- [ ] `ui/design/metrics.go` — spacing/radius/size токены
- [ ] `ui/design/typography.go` — уровни шрифта
- [ ] `ui/design/colors.go` — semantic-цвета (light/dark)
- [ ] `ui/icons/` — SVG-набор навигации и шевронов

## Phase 2 — Shell
- [ ] `Sidebar` / `SidebarItem` компоненты
- [ ] `AppPage` (заголовок + тело)
- [ ] `App.selectSection(id)` — вынос тела `OnSelected` без изменений
- [ ] Подключить shell в `NewApp`

## Phase 3 — Submenu
- [ ] Раскрытие/сворачивание без смены активной страницы
- [ ] Selected у свёрнутого родителя
- [ ] Навигация по подпунктам

## Phase 4 — Local
- [ ] Overview / Proxies / Traffic
- [ ] Пропорции колонок вместо 395/165

## Phase 5 — Remote
- [ ] Machines / Proxies

## Phase 6 — Settings / Diagnostics / Help
- [ ] Settings: Language / Connection / Subscriptions / Storage
- [ ] Diagnostics: Logs & Maintenance / Network / Renderer
- [ ] Help: About

## Phase 7 — Dialogs
- [ ] Единые ширина/отступы

## Phase 8 — Wizard visuals
- [ ] Только presentation

## Phase 9 — Cleanup + docs
- [ ] Удалить emoji-навигацию и мёртвые константы
- [ ] `docs/ARCHITECTURE.md`, `docs/release_notes/upcoming.md`
- [ ] IMPLEMENTATION_REPORT.md
