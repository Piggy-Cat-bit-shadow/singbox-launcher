# SPEC 144 — UI modernization (Fyne-native)

## Тип
Feature. Статус: Open. Реализация presentation-слоя; бизнес-слой L0–L4 не трогается.

## Проблема

Внешний вид лаунчера — это стек `container.AppTabs` с emoji-заголовками
(`🌐 Remote`, `⚙️ Settings`, `🔍 Diagnostics`, `❓ Help`) и динамическим
`▶️/⏸️ Local`. Навигация горизонтальная, ёмкость ограничена шириной окна;
иерархии нет вообще. Стили размазаны по файлам: `widget.NewLabelWithStyle(...
Bold)` вместо типографики, магические отступы, круглые скобки `395/165` в
`local_remote_tabs.go` как «ширины колонок».

Замеры, подтверждающие drift:

- `MinWindowSize = leftColumnWidth + rightColumnWidth` = `395 + 165` = **560**,
  при том что комментарий рядом объявляет «1000 берётся из измеримого».
  Документация и код разошлись; окно можно сжать до непригодного.
- Правая колонка Local/Remote — **165** logical units под dashboard: версия
  ядра, адрес машины и строки действий туда не помещаются.
- Emoji в навигации зависят от системного emoji-шрифта: baseline и метрики
  разные на macOS / Windows / Linux, в 125 % scaling и Linux без emoji-шрифта
  это даёт пустые места (уже зафиксировано в `ui/icons/icons.go` для `⚡`).

## Требования

### R1. Один shell: Sidebar + Content

Главная навигация — вертикальный сайдбар слева, контент справа. Нативные
декорации окна сохраняются (самостоятельный titlebar не рисуем).

### R2. Иерархия с реальными подпунктами

Второй уровень строится **только** из фактически существующих разделов
страницы. Придумывать страницы без функциональности запрещено.

### R3. Selection semantics не меняются

`OnSelected` в `ui/app.go` — это не переключение картинки, а исполнение
последовательности побочных эффектов:

1. `APIService.SetProxyScope(ScopeLocal | ScopeRemote)`
2. `localPanel.Activate(controller)` / `remotePanel.Activate(controller)`
3. `RestoreOwnTransport()` / `ReapplyLxdRemoteTransport(controller)`
4. `refreshSettings()` (для Settings)
5. `AutoRefresh().SetTabActive(...)`, `EndpointPoll().SetTabActive(...)`
6. `RefreshEndpointStates(controller)`
7. `UIService.RefreshAPIFunc()` — с гейтом «есть ли с кем говорить»

Порядок шагов и их состав обязаны сохраниться дословно. Сайдбар вызывает
ровно тот же код через один тонкий обработчик выбора; второй реализации
навигации не появляется.

### R4. Design system вместо магических чисел

Единый источник: spacing, radius, размеры, типографика, цвета, иконки.
Все размеры — **Fyne logical units**. Запрещено: `* canvas.Scale()`,
`if retina { *2 }`, `runtime.GOOS == "darwin"` для DPI, установка `FYNE_SCALE`.

### R5. Векторные иконки вместо emoji

Навигация, toolbar и inline-иконки — SVG (`currentColor`), не emoji.

### R6. Window tokens

Единые `DefaultWindowSize` / `MinWindowSize`, выведенные из реального
содержимого, без «560». Окно не должно сжиматься ниже читаемого состояния.

### R7. Ноль изменений в поведении

Local/Remote переключение, transport, Scope, daemon, Remote, подписки,
config build, settings persistence, wizard business — без изменений.

## Критерии приёмки

- `go build ./...` проходит; затронутые пакеты собираются.
- Sidebar переключает разделы, и каждый переход исполняет полный набор
  побочных эффектов из R3 в исходном порядке.
- Свёрнутый родительский пункт не меняет активную страницу.
- Вёрстка не содержит physical-pixel предположений; масштаб 0.8–2.0
  остаётся читаемым (проверка владельцем, см. IMPLEMENTATION_REPORT).
- Ни одна страница не теряет функциональность: у всех прежних вкладок,
  кнопок и обработчиков есть доступный путь в новом UI.
- Классический режим, daemon, Remote и визард не регрессируют.

## Что вне scope

- Смена GUI-фреймворка (Fyne остаётся единственным).
- Переписывание L0–L4.
- Анимации, blur/acrylic/vibrancy, самостоятельный titlebar.
- Изменение схем хранения и миграции настроек.
- Автоматическое сворачивание сайдбара в icon-only.
