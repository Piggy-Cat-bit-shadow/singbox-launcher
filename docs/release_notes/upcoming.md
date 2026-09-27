# Upcoming release — черновик

Сюда складываем пункты, которые войдут в следующий релиз. Перед релизом переносим в `X-Y-Z.md` и очищаем этот файл.

## EN
### Highlights
- **New navigation shell.** A vertical sidebar replaces the horizontal emoji tab strip. Pages are grouped with real sub-sections (Local → Overview / Proxies / Traffic, Remote → Machines / Proxies, Settings → Connection / Subscriptions / Language / Storage), so the launcher reads like a modern desktop client instead of a row of tabs.
- **Vector icons instead of emoji.** Navigation, buttons and status no longer depend on the system colour-emoji font. Emoji rendered differently on macOS, Windows and Linux — and vanished entirely on a Linux box without an emoji font. Everything is a themed SVG or a Fyne theme icon now, so it scales with the display and follows light/dark.
- **A window size that makes sense.** The minimum was 560 logical units (395 + 165) while the comment beside it promised 1000 — the window could be shrunk until the right pane collapsed. It now starts at 1180×760 and cannot go below 960×640, and the Local/Remote columns are sized by proportion with real minimums instead of two fixed numbers.
- **Consistent design system.** Spacing, radii, typography and colours come from one place (`ui/design`) rather than being scattered as magic numbers, so pages share a rhythm and a look.

### Technical / Internal
- SPEC 144. New `ui/design` package: `metrics.go` (4-unit spacing, radii, sidebar geometry, split ratios, window sizes), `typography.go` (five text levels), `colors.go` (semantic colours derived from the active theme), `sidebar.go`, `page.go` (PageHeader, SectionCard, StatusBadge).
- Navigation semantics preserved mechanically: the entire body of `app.tabs.OnSelected` moved verbatim into `App.selectSection` (`ui/navigation.go`). The sidebar and the retained (now off-screen) `AppTabs` both call that single method, so there is exactly one implementation of scope → panel activation → transport → refresh, in the original order.
- Pages are constructed once and swapped through a content host, so switching sections keeps scroll position, field contents and expanded sections.
- Collapsed sidebar parents keep the selected highlight while their child is active, and clicking a parent only expands it — it never changes the page.
- The core status indicator moved from the emoji tab title (▶️/⏸️) to a sidebar footer that projects `AppController.RunningState` — no second copy of the state.
- All sizes are Fyne logical units. No `canvas.Scale()`, no Retina multiplier, no `runtime.GOOS` branch for DPI, and `FYNE_SCALE` is never set.
- `docs/ARCHITECTURE.md` updated (L6/L7 rows, VpnStateChanged subscriber).

## RU
### Основное
- **Новая навигация.** Вертикальный сайдбар вместо горизонтальной строки вкладок с emoji. Страницы сгруппированы с реальными подразделами (Local → Overview / Proxies / Traffic, Remote → Machines / Proxies, Settings → Connection / Subscriptions / Language / Storage), и лаунчер читается как современный десктопный клиент, а не как ряд вкладок.
- **Векторные иконки вместо emoji.** Навигация, кнопки и статус больше не зависят от системного emoji-шрифта: emoji выглядели по-разному на macOS, Windows и Linux, а на Linux без такого шрифта просто исчезали. Теперь это themed-SVG или иконки темы Fyne: масштабируются вместе с экраном и следуют light/dark.
- **Осмысленный размер окна.** Минимум был 560 logical units (395 + 165), хотя комментарий рядом обещал 1000 — окно сжималось до схлопывания правой панели. Теперь старт 1180×760, ниже 960×640 не опускается, а колонки Local/Remote заданы долей с реальными минимумами вместо двух фиксированных чисел.
- **Единая дизайн-система.** Отступы, радиусы, типографика и цвета — из одного места (`ui/design`), а не россыпью магических чисел: у страниц общий ритм и общий вид.

### Техническое / Внутреннее
- SPEC 144. Новый пакет `ui/design`: `metrics.go` (4-й ритм отступов, радиусы, геометрия сайдбара, доли сплита, размеры окна), `typography.go` (пять уровней текста), `colors.go` (semantic-цвета из активной темы), `sidebar.go`, `page.go` (PageHeader, SectionCard, StatusBadge).
- Семантика навигации сохранена механически: тело `app.tabs.OnSelected` целиком переехало в `App.selectSection` (`ui/navigation.go`). Сайдбар и оставленный (уже невидимый) `AppTabs` зовут один и тот же метод, поэтому реализация «область → активация панели → транспорт → refresh» ровно одна и в исходном порядке.
- Страницы создаются один раз и подменяются через content host: переключение разделов сохраняет позицию скролла, введённый текст и раскрытые секции.
- Свёрнутый родитель в сайдбаре сохраняет подсветку, пока активен его подпункт; клик по родителю только разворачивает его и никогда не меняет страницу.
- Индикатор состояния ядра переехал из emoji-заголовка вкладки (▶️/⏸️) в нижнюю строку сайдбара и проецирует `AppController.RunningState` — второй копии состояния нет.
- Все размеры — в Fyne logical units. Без `canvas.Scale()`, без множителя Retina, без ветвлений по `runtime.GOOS` для DPI; `FYNE_SCALE` не выставляется.
- Обновлён `docs/ARCHITECTURE.md` (строки L6/L7, подписчик VpnStateChanged).
