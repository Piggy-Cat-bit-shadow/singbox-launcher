# IMPLEMENTATION REPORT — SPEC 145 Visual Overhaul

Дата: волна 1. Статус: собрано, проверено локально, ожидает визуальной приёмки
владельцем на реальных экранах/DPI.

## Что сделано по фазам

| Phase | Содержание | Статус |
|---|---|---|
| A | Глобальная тема: `palette.go`, `theme.go`, переписанные `colors.go`, новые метрики (отступы до 48, радиусы 8–18, окно 1220×800 / мин 1000×680) | готово |
| B | Плоский сайдбар: `NavEntry`, мягкий pill вместо полосы, перенос вместо ellipsis, группы HOME/NETWORK/TOOLS, About снизу | готово |
| — | `RouteID` + `routeDomain`: домен меняется только при реальном переходе | готово |
| C | Home-дашборд: hero (состояние + Start/Stop), карточка Runtime, карточки-переходы Proxies и Remote | готово |
| D | Proxies: полная ширина, та же панель списка | готово |
| E | Remote: page-local segmented (Machines / Proxies) | готово |
| F | Traffic: страница на существующем профилировщике | готово |
| G | Settings: карточки Language / Connection / Subscriptions / Debug API / Storage | готово |
| H | Diagnostics: карточки Logs / Files / Network checks / Traffic / Renderer / Dangerous actions; About переписан | готово |
| I | Dialogs | не входило в эту волну |
| J | Wizard visual normalization | не входило в эту волну |

## Устранённые дефекты SPEC 144

1. **Accent-полоса заливала строку** — была полновысотным слоем `Stack` без
   ограничения ширины. Убрана; выбранное состояние — мягкая заливка.
2. **Подписи превращались в «…»** — `TextTruncateEllipsis` при жёстком
   `MinSize`. Теперь перенос по словам и высота строки до двух строк.
3. **`MinSize` колонки зависел от контента** — мог «прыгать». Теперь высота
   строк фиксирована константами.
4. **Двухуровневое дерево в сайдбаре** — заменено плоским списком с группами.

## Проверки (выполнены фактически)

```
go build ./...                            → ok
gofmt -l ui/ main.go core/uiservice       → пусто
go test ./ui/ ./internal/locale/          → ok
go run ./tools/l10n/l10n_check --strict   → 0 warnings, 0 hard fails
go run ./tools/l10n/hardcoded_check       → 0 sites
go run ./tools/paths_guard                → 0 findings
go run ./tools/win7guard                  → 640 files, 0 constructs
./build/package_macos.sh arm64            → JiejieBox.app собран
./build/check_macos_artifact.sh … arm64   → ALL CHECKS PASSED
<binary> -paths                            → стартует, пути резолвятся
```

## Сохранённые бизнес-инварианты

- `selectSection` — единственная реализация побочных эффектов домена; вызывается
  только при смене домена (`showRoute`), поэтому Home → Proxies → Traffic не
  повторяет scope/transport/refresh.
- Start/Stop — те же `core.StartSingBoxProcess` / `core.StopSingBoxProcess`.
- Список прокси — тот же объект `ProxyListPanel` (колбэки, виртуализация,
  состояние).
- Трафик — тот же синглтон-менеджер профилировщика; второго сборщика нет.
- `AppTabs` оставлен off-screen как compatibility; `app.content` им не
  перезаписывается.

## Ограничения Fyne, которые остались

- **Нет размытых теней.** Глубина сделана фоном/границей/радиусом. Настоящая
  тень — отдельный объект с перерисовкой и разным видом на разных ОС.
- **Нет нативных материалов** (vibrancy/acrylic) и translucent-окна.
- **Нет анимаций** переходов: SPEC прямо исключает их в этой волне.
- **Скроллбар системный**, не overlay-стиля macOS.
- **Titlebar нативный.** Self-drawn titlebar исключён SPEC (риск для traffic
  lights, DPI Windows, decorations Linux).
- **Иконки темы Fyne** для части контролов остаются «финевыми» — набор
  собственных SVG покрывает навигацию и ключевые действия, но не все.

## Что осталось владельцу

Визуальная приёмка на реальных экранах: масштабы 0.8–2.0, узкое окно,
русская локализация, длинный список узлов. UI-вёрстка тестами не покрывается
(CONSTITUTION §8.1).

Phases I (Dialogs) и J (Wizard) сознательно не входили в эту волну: они
затрагивают модальные окна и модели визарда, и их следует делать отдельным
проходом, чтобы не смешивать с основным overhaul.
