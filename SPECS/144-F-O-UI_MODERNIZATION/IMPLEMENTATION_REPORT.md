# IMPLEMENTATION REPORT — SPEC 144 UI modernization

Дата: реализация волны 1 (Phase 0–6, 9). Статус: собран и проверен локально,
ожидает визуальной приёмки владельцем на реальных экранах/DPI.

## Что сделано

| Phase | Содержание | Статус |
|---|---|---|
| 0 | Инвентарь страниц и подразделов; карта побочных эффектов `OnSelected`; замер drift размера окна; SPEC/PLAN/TASKS | готово |
| 1 | `ui/design` (metrics/typography/colors) + SVG-набор навигации | готово |
| 2 | Sidebar + content host + `App.selectSection` | готово |
| 3 | Раскрытие/сворачивание, selected у свёрнутого родителя, навигация по подпунктам | готово |
| 4 | Local: шапка страницы, пропорции колонок вместо 395/165 | готово (структурно) |
| 5 | Remote: шапка, подпункты Machines/Proxies | готово (структурно) |
| 6 | Settings / Diagnostics / Help: шапки, карточки Help, emoji → иконки темы | готово |
| 7–8 | Диалоги и визуал визарда | не входило в эту волну |
| 9 | Cleanup emoji-навигации, docs, отчёт | готово |

## Ключевое решение: навигация не переписана

Тело `app.tabs.OnSelected` перенесено **дословно** в `App.selectSection`
(`ui/navigation.go`). Сайдбар зовёт `showSection` → `selectSection`;
`AppTabs` оставлен и зовёт тот же метод. Реализация одна.

Порядок сохранён: scope → `Activate` → transport → `SetTabActive` →
`RefreshEndpointStates` → `RefreshAPIFunc` (с прежним гейтом «есть ли с кем
говорить»).

Подраздел (`settings.connection` и т.п.) открывает страницу-владельца, но
побочные эффекты исполняются только при смене СТРАНИЦЫ — переход
Local → Local.Proxies не перезагружает список.

## Проверки (выполнены фактически)

```
go build ./...                      → ok
gofmt -l ui/ main.go                → пусто
go test ./ui/ -count=1              → ok
go run ./tools/l10n/l10n_check --strict  → 0 warnings, 0 hard fails
go run ./tools/l10n/hardcoded_check      → 0 sites
go run ./tools/paths_guard               → 0 findings
go run ./tools/win7guard                 → 631 files, 0 constructs
./build/package_macos.sh arm64           → JiejieBox.app собран
./build/check_macos_artifact.sh … arm64  → ALL CHECKS PASSED
```

Полный прогон тестов, линтер и Win7-сборка — в CI (по CONSTITUTION §8.1
локально не запускаются).

## Что осталось для владельца

Визуальная приёмка на реальных экранах: масштабы 0.8–2.0, узкое окно,
русская локализация, очень длинный список узлов. Автоматически эти вещи не
проверяются — UI-вёрстка тестами не покрывается (CONSTITUTION §8.1).

## Риски и ограничения

- `AppTabs` оставлен в коде как программный путь; в визуальном дереве его
  нет. Удаление — отдельная волна, чтобы не потерять совместимость
  (`updateClashAPITabState` вызывается из нескольких мест).
- Подпункты пока не прокручивают страницу к своей секции: они открывают
  страницу-владельца и подсвечиваются. Прокрутка к якорю — Phase 4+.
- Внутренняя вёрстка Local/Remote (карточки dashboard, строки списка) в этой
  волне приведена к системе только по токенам и шапке; глубокая
  переработка строк списка — следующая волна.
