# SPEC 145 — Fyne Visual Overhaul (presentation redesign)

Тип: Feature. Статус: Open.
Предыдущая волна: SPEC 144 (sidebar shell + design tokens) — принята, CI зелёный на `ee7f01b3`.

## Проблема

SPEC 144 заменила навигацию, но не изменила **визуальный язык**. Приложение
по-прежнему построено на дефолтной теме Fyne: белый фон, стандартные кнопки,
стандартные checkbox/entry, а «сайдбар» — это список строк с accent-полосой
слева, то есть паттерн веб-админки, а не macOS-утилиты. Пользователь
описывает результат как «просто заменили вкладки на сайдбар» — что верно.

Причина не в layout, а в том, что **не переопределена тема**. Пока
`fyne.Theme` делегирует цвета Fyne, любой layout будет выглядеть как Fyne.

## Дефекты SPEC 144, подтверждённые в коде (не предположения)

| # | Дефект | Где |
|---|---|---|
| 1 | Accent-полоса рисуется как полновысотный слой `Stack` без ограничения ширины → заливает строку | `ui/design/sidebar.go` `newSidebarRow` |
| 2 | Подпись пункта с `TextTruncateEllipsis` при жёстком `Sidebar.MinSize` → «…» вместо текста | `ui/design/sidebar.go:321`, `:256` |
| 3 | Accent-полоса слева — запрещённый паттерном стиль | `metrics.go` `NavSelectedIndicator` |
| 4 | Двухуровневое дерево в сайдбаре (бинарно «Local/Remote + дети») | `ui/app.go` `navigationEntries` |

Все четыре устраняются в этой волне: Sidebar переписывается целиком.

## Что меняется

Presentation-слой (L6/L7). Бизнес-слой L0–L4 не трогается.

### R1. Глобальная тема

`design.Theme` реализует `fyne.Theme`: `Color()` и `Size()` переопределены,
`Font()`/`Icon()` делегируют дефолтной теме (шрифты и иконки темы не
подменяем). Палитры light/dark — из семантических токенов с явными
hex-значениями, а не производные от Fyne-цветов.

Устанавливается один раз в `main.go`.

### R2. Flat navigation

Сайдбар — плоский список с группами. Никакого постоянного дерева.

```
HOME      Home
NETWORK   Proxies | Remote | Traffic
TOOLS     Diagnostics | Settings
          About
```

Детализация уезжает в page-local navigation (segmented control).

### R3. PresentationRoute

Маршрут — presentation-понятие; домен (`SectionLocal`/`SectionRemote`)
выводится из него. Переключение маршрута внутри одного домена **не**
повторяет `selectSection` (иначе flicker и лишние запросы).

```
RouteHome, RouteProxies, RouteTraffic  → domain SectionLocal
RouteRemote                            → domain SectionRemote
RouteDiagnostics/Settings/About        → domain-neutral (не меняет scope)
```

### R4. Ограничение ширины контента

`MaxWidthLayout`: при `available <= max` — заполняет, при `available > max` —
использует max и центрирует. **Не** становится частью `MinSize`.

### R5. Доменные инварианты (жёстко)

- `RouteRemote` → `selectSection(SectionRemote)` ровно один раз при смене домена.
- Вход в Local-домен из Remote → `selectSection(SectionLocal)` ровно один раз.
- Внутри домена (Home→Proxies→Traffic) `Activate`/`RestoreOwnTransport`/`Refresh`
  **не повторяются**.
- `SectionLocal` / `SectionRemote` / `selectSection` не удаляются и не
  дублируются.

## Карта действий (обязательна к 1:1 переносу)

| Старый контрол | Старый callback | Новый контрол |
|---|---|---|
| `startButton` | `beginPendingOp(...); core.StartSingBoxProcess()` | Home hero primary (Start) |
| `stopButton` | `beginPendingOp(...); core.StopSingBoxProcess()` | Home hero primary (Stop) |
| `restartButton` | `restartButton.OnTapped` (split: rebuild / rebuild+restart) | Home toolbar icon button |
| `updateConfigButton` | `doRefreshOnly` | Home/Proxies toolbar + Settings |
| `wizardButton` | открыть Configurator | Sidebar/Settings entry |
| `configStatusLabel` | открыть config | Home Runtime row / Settings |
| `templateDownloadButton` | скачать шаблон | Settings → Config |
| `downloadButton` | скачать ядро | Settings → Core / Diagnostics |
| `stateSelect` | выбор сохранённого состояния | Home Runtime card |
| `singboxHelpBtn`, `wintunHelpBtn` | справка | рядом с соответствующим контролом |
| Diagnostics: log window, logs/config folder, clean rule-sets, kill, profiler, STUN, IP check | как есть | Diagnostics cards |
| Help: Telegram, GitHub | открыть URL | About rows |

Ни один callback не переписывается: новые контролы вызывают те же
`core.*` / `tab.*` пути.

## Вне scope

- Смена темы/фреймворка, self-drawn titlebar, blur/vibrancy, анимации.
- Правки L0–L4, схемы состояния, протокола демона.
- Удаление `AppTabs` (остаётся off-screen как compatibility).

## Критерии приёмки

- `go build ./...` на каждом этапе.
- Тема реально применена (светлая/тёмная переключаются, кастомные canvas
  объекты перекрашиваются в `Refresh`).
- Доменные инварианты R5 подтверждены статически.
- Ни один callback из карты действий не потерян.
- Существующие named-тесты `./ui/`, `./internal/locale/` проходят.
