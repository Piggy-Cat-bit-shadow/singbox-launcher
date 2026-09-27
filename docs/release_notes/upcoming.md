# Upcoming release — черновик

Сюда складываем пункты, которые войдут в следующий релиз. Перед релизом переносим в `X-Y-Z.md` и очищаем этот файл.

## EN
### Highlights
- **A real visual redesign, not a re-skin.** The launcher now has its own application theme (light and dark palettes, macOS-like greys, #007AFF accent) instead of Fyne's defaults — which is why earlier layout work still looked like a stock toolkit. Every standard control inherits it.
- **Flat navigation.** The sidebar is one level of grouped items (Home / Network / Tools, About pinned at the bottom) with a soft rounded highlight on the selected item. The tree — and the admin-panel accent bar — are gone; detail navigation moved into the pages themselves.
- **A Home dashboard.** Instead of a proxy list beside a service panel, the app opens on a summary: connection state and the Start/Stop action, core and config details, and cards that lead to Proxies, Remote and Traffic. The proxy list gets the full page width where it belongs.
- **Content is no longer stretched edge to edge.** Pages keep a readable column (860 units, centred on wide windows) instead of cards spanning the whole display.
- **Settings and Diagnostics are grouped by meaning** into cards rather than a long form divided by rules; destructive actions are set apart.

### Technical / Internal
- SPEC 145. `ui/design.Theme` implements `fyne.Theme` with explicit light/dark palettes and the app's radii; `Font()`/`Icon()` delegate to the default theme (swapping fonts would break metrics on Windows/Linux). Installed from `main.go` — `core/uiservice` is L2 and must not import `ui/` (L7), so the stock `SetTheme` call there was removed rather than left to overwrite it.
- `RouteID` separates presentation routes from business domains: `selectSection` runs only when the domain actually changes, so Home → Proxies → Traffic neither re-applies scope/transport nor re-polls.
- New primitives: `MaxWidthLayout` (constrains and centres content, never its `MinSize`), `Card`/`ClickableCard`/`CardRow`, `SegmentedNav`, `StatusBadge`, primary/secondary/ghost/icon actions, `tintResource` (recolours `currentColor` SVGs so one icon serves normal and selected states).
- Navigation semantics preserved mechanically: the entire body of `app.tabs.OnSelected` moved verbatim into `App.selectSection` (`ui/navigation.go`). The sidebar and the retained (now off-screen) `AppTabs` both call that single method, so there is exactly one implementation of scope → panel activation → transport → refresh, in the original order.
- Pages are constructed once and swapped through a content host, so switching sections keeps scroll position, field contents and expanded sections.
- Collapsed sidebar parents keep the selected highlight while their child is active, and clicking a parent only expands it — it never changes the page.
- The core status indicator moved from the emoji tab title (▶️/⏸️) to a sidebar footer that projects `AppController.RunningState` — no second copy of the state.
- All sizes are Fyne logical units. No `canvas.Scale()`, no Retina multiplier, no `runtime.GOOS` branch for DPI, and `FYNE_SCALE` is never set.
- `docs/ARCHITECTURE.md` updated (L6/L7 rows, VpnStateChanged subscriber).

## RU
### Основное
- **Настоящий визуальный редизайн, а не перекраска.** У лаунчера появилась своя тема приложения (светлая и тёмная палитры, macOS-подобные серые, акцент #007AFF) вместо дефолтной Fyne — именно поэтому прошлые правки вёрстки всё равно выглядели как «стандартный тулкит». Тема наследуется всеми стандартными контролами.
- **Плоская навигация.** Сайдбар — один уровень пунктов, сгруппированных заголовками (Главная / Сеть / Инструменты, «О программе» прижат внизу), выбранный пункт подсвечен мягкой скруглённой заливкой. Дерево и полоса-индикатор в стиле админки убраны; детализация переехала на сами страницы.
- **Дашборд на главной.** Вместо списка узлов рядом со служебной панелью приложение открывается сводкой: состояние и кнопка Start/Stop, сведения о ядре и конфиге, карточки-переходы к Прокси, Remote и Трафику. Список узлов получает всю ширину страницы, где он и нужен.
- **Контент больше не растянут от края до края.** Страницы держат читаемую колонку (860 units, по центру на широком окне) вместо карточек на весь экран.
- **Настройки и Диагностика сгруппированы по смыслу** в карточки, а не в длинную форму с линиями; опасные действия вынесены отдельно.

### Техническое / Внутреннее
- SPEC 144. Новый пакет `ui/design`: `metrics.go` (4-й ритм отступов, радиусы, геометрия сайдбара, доли сплита, размеры окна), `typography.go` (пять уровней текста), `colors.go` (semantic-цвета из активной темы), `sidebar.go`, `page.go` (PageHeader, SectionCard, StatusBadge).
- Семантика навигации сохранена механически: тело `app.tabs.OnSelected` целиком переехало в `App.selectSection` (`ui/navigation.go`). Сайдбар и оставленный (уже невидимый) `AppTabs` зовут один и тот же метод, поэтому реализация «область → активация панели → транспорт → refresh» ровно одна и в исходном порядке.
- Страницы создаются один раз и подменяются через content host: переключение разделов сохраняет позицию скролла, введённый текст и раскрытые секции.
- Свёрнутый родитель в сайдбаре сохраняет подсветку, пока активен его подпункт; клик по родителю только разворачивает его и никогда не меняет страницу.
- Индикатор состояния ядра переехал из emoji-заголовка вкладки (▶️/⏸️) в нижнюю строку сайдбара и проецирует `AppController.RunningState` — второй копии состояния нет.
- Все размеры — в Fyne logical units. Без `canvas.Scale()`, без множителя Retina, без ветвлений по `runtime.GOOS` для DPI; `FYNE_SCALE` не выставляется.
- Обновлён `docs/ARCHITECTURE.md` (строки L6/L7, подписчик VpnStateChanged).
