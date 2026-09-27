# SWIFTUI REWRITE AUDIT — Phase 0

Baseline: `5489eb69d6af75860b8281a3dd6bac64f4c6074a` (main, clean tree).
Аудит проведён до написания кода. Все цифры получены из репозитория.

## 1. Масштаб

| Область | Файлы | LOC |
|---|---|---|
| Весь Go | 1146 | 293 364 |
| `core/` | — | 161 369 |
| `ui/` (Fyne presentation) | 60 + 158 configurator + 9 traffic | 86 802 |
| `core/debugapi/` | 19 | (HTTP-фасад, 2 файла с fyne) |

Для сравнения: типичный «один заход» по этому репозиторию — 200–2500 строк
изменений. Перенос presentation-слоя на 86 802 строки — это не одна задача.

## 2. Что уже пригодно к переиспользованию

**Чистые (без Fyne) доменные пакеты:**

- `core/config/**` — парсер, реестр, эмиссия, шаблоны
- `core/state/**` — миграции состояния
- `internal/traffic/**` — профайлер (parser, session, grpc_tracker)
- `internal/lxdclient/**`, `internal/daemonpb/**` — демон, mTLS
- `internal/platform/**` — платформенные примитивы
- `ui/configurator/business/**` — **0 файлов с fyne**
- `ui/configurator/models/**` — **0 файлов с fyne**

Последние два особенно важны: бизнес-логика визарда уже отделена от
presentation и переносится в backend без переписывания.

**Готовый headless-фасад:** `core/debugapi/` (19 файлов) уже предоставляет
HTTP-API для многих операций и почти не зависит от Fyne (2 файла). Это
разумная отправная точка для IPC-слоя, а не для переписывания с нуля.

## 3. Fyne-связанность в `core` (блокер headless)

Файлы `core/`, импортирующие `fyne.io/fyne` полностью: **3231 строка**.

| Файл | LOC | Что делает |
|---|---|---|
| `core/controller.go` | 1024 | AppController: UIService, окна, диалоги, `hasUI()`, `UpdateUI()` |
| `core/services/api_service.go` | 622 | Clash API + UI callbacks |
| `core/elevation.go` | 371 | Диалоги повышения прав |
| `core/uiservice/ui_service.go` | 353 | Fyne Application/Window/tray |
| `core/classic_privileged_windows.go` | 230 | Windows-специфичный UI |
| `core/tray_menu.go` | 212 | systray |
| `core/ui_watchdog.go` | 149 | UI-сторож |
| `core/debugapi_ui.go` | 131 | Debug API → UI |
| `core/classic_privileged_darwin.go` | 99 | macOS-диалоги |
| `core/classic_privileged_other.go` | 40 | заглушки |

Это и есть работа Phase 1: разорвать эти 10 файлов, а не «добавить флаг
headless». `AppController` — центральный узел: его нельзя «пометить»
headless, его надо разделить на доменные сервисы.

**Разрыв зависимостей не сводится к удалению импортов:** `api_service.go`
содержит и Clash-клиент, и UI-нотификации. Разделение — содержательная
работа, а не механическая.

## 4. GUI-поверхность, которую надо перенести (паритет)

По инвентарю `ui/`:

| Область | Файлов | Комментарий |
|---|---|---|
| Configurator (визард) | 158 | источники, outbounds, chain, правила, DNS, пресеты, vars, preview, validation, save, build, import/export |
| Основные страницы | ~40 | Home/Proxies/Remote/Traffic/Settings/Diagnostics/About + окна машин, редакторы, фильтры, логи |
| Traffic | 9 | профайлер UI, сессии, агрегация |
| `ui/design`, `ui/icons`, `ui/components` | ~15 | design system, SVG, виджеты |
| `internal/fynewidget` | ~18 | hover, tooltip, chip, drag-reorder, notice bar, tap-wrap |

Ключевые окна, которые обязаны продолжать работать: connection settings
(движок + демон), add/edit machine, machine resources, machine host, machine
profiler, wire log, runtime window, log viewer, servers filter, node info,
core rejected notice, configurator (все вкладки + диалоги).

## 5. Проверенные ограничения среды (важно для плана)

| Проверка | Результат |
|---|---|
| `swift --version` | Swift 6.4, Apple Swift 6.4 |
| macOS | 27.0 (build 26A428), arm64 |
| `xcrun --show-sdk-version` | 27.0 |
| `xcodebuild` | **есть, но неработоспособен**: `requires Xcode, but active developer directory is a CommandLineTools instance` |
| `/Applications/Xcode*.app` | **отсутствует** |
| `swiftc -parse` SwiftUI | ok |
| `swiftc -parse-as-library` полноценное SwiftUI-приложение | **собрано, arm64** |
| `swift build` (SPM) SwiftUI executable | **Build complete** |
| `.app` bundle + `codesign -v` | **signature VALID** |

**Вывод:** на этой машине можно собрать SwiftUI-приложение через
**Swift Package Manager** и упаковать его в подписанный `.app` вручную — это
проверено, а не предположено. Но **нельзя** создать/собрать
`JiejieBox.xcodeproj`: для `xcodebuild` нужен полный Xcode, которого нет.

Это меняет одну деталь плана и не меняет цель: вместо `.xcodeproj` —
`Package.swift` + скрипт упаковки. Пользовательский результат тот же
(`JiejieBox.app` с SwiftUI и Go-хелпером).

## 6. Главный вывод по объёму

Задача сформулирована как один заход «Phase 0 → Phase 11 с удалением Fyne».
Фактический объём:

- ~3 200 строк Go требуют разрыва зависимостей от Fyne (Phase 1);
- ~86 800 строк presentation требуют переноса в SwiftUI (Phases 2–8);
- из них **158 файлов конфигуратора** — отдельная крупная подсистема (Phase 8);
- плюс IPC-протокол, contract-тесты, меню-бар, окна, CI-переписывание.

Один непрерывный заход этого не покрывает. При этом SPEC прямо запрещает
останавливаться на «Home готов, остальное TODO» и запрещает оставлять
двойной фронтенд в продукте.

**Поэтому нужен выбор владельца** (см. вопрос в отчёте): либо длинная серия
волн с сохранением рабочего Fyne-продукта до cutover, либо сокращение
первого этапа до «headless backend + IPC + каркас SwiftUI с Home/Start/Stop»
с явно отложенными Proxies/Remote/Traffic/Settings/Configurator.

## 7. Риски

1. **Данные пользователя.** `DataDir` = `~/Library/Application Support/singbox-launcher`,
   схема `state.json`/`settings.json`, сертификаты демона и identity машин.
   Не мигрировать, не менять Bundle ID (`com.piggycat.jiejiebox`).
2. **Привилегии.** `classic_privileged_*` (darwin/windows/other) завязаны на
   диалоги Fyne. При headless-переходе авторизация должна переехать в
   frontend (Swift), а backend — принимать уже выданное право/команду.
   Это изменение модели безопасности, его нельзя делать «попутно».
3. **Отсутствие Xcode** в CI и локально: сборка только через SPM/`swiftc`.
   Если нужен именно `.xcodeproj` — требуется установка Xcode.
4. **Параллельный фронтенд.** Пока SwiftUI не покрыл паритет, Fyne-путь
   должен оставаться собираемым, иначе main ломается для пользователя.

## 8. Что предлагается сделать в этой волне

Phase 0 (этот документ) + Phase 1, начатый безопасно и проверяемо:

- вынести Fyne-независимый runtime из `AppController` в отдельный пакет;
- определить и реализовать versioned JSON IPC (newline-delimited, stdio)
  с handshake, snapshot и потоком событий;
- собрать `jiejiebox-backend` как headless-бинарь и доказать, что он
  запускается и отвечает на `GetAppSnapshot` без Fyne Application;
- **не** удалять Fyne и **не** трогать UI в этой волне — чтобы main остался
  рабочим продуктом.

Остальные фазы — отдельные волны, каждая с зелёной сборкой.
