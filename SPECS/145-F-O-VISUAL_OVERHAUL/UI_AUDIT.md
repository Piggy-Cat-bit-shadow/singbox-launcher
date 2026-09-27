# UI CONVERGENCE AUDIT — SPEC 145 baseline `f9f79c12`

Аудит проведён ДО правок. Все утверждения ниже проверены по коду (`grep`/
чтение файлов), а не выведены из скриншотов.

## A. Route tree

| Route | Domain (`routeDomain`) | CanvasObject сейчас | Источник |
|---|---|---|---|
| `RouteHome` | `SectionLocal` | `homePage.Object()` → `*fyne.Container` | `ui/home.go` |
| `RouteProxies` | `SectionLocal` | `buildProxiesPage(localPanel)` | `ui/pages.go` |
| `RouteTraffic` | `SectionLocal` | `buildTrafficPage(controller)` | `ui/traffic_page.go` |
| `RouteRemote` | `SectionRemote` | **`CreateRemoteTab(controller)` (legacy HSplit)** | `ui/local_remote_tabs.go:153` |
| `RouteDiagnostics` | `""` | `pageWithHeaderScroll(...)` | `ui/app.go:122` |
| `RouteSettings` | `""` | `pageWithHeaderScroll(...)` | `ui/app.go:120` |
| `RouteAbout` | `""` | `buildAboutPage(controller)` | `ui/pages.go` |

## B. Детерминированные дефекты (подтверждены кодом)

### B1. `RouteRemote` не использует современную страницу
`ui/pages.go` содержит `buildRemotePage(proxyPanel, machines)`, реализующую
page-local navigation `Machines | Proxies`. **Функция не вызывается ниоткуда** —
`grep -rn buildRemotePage ui/` даёт только её определение. `RouteRemote`
по-прежнему получает результат `CreateRemoteTab()` — `container.NewHSplit(
proxyPanel.Content, withColumnWidth(machines, rightColumnWidth))`.
Это architectural drift: современная страница написана и не подключена.

### B2. Lifecycle: `refreshSettings` не исполняется
`refreshSettings()` вызывается внутри `selectSection` в ветке
`case SectionSettings`. Но `routeDomain(RouteSettings) == ""`, а `showRoute`
вызывает `selectSection` только при `domain != ""`:
```
if domain != "" && a.currentSection != domain { ... selectSection(domain) }
```
Следствие: переход Remote → Settings (и Home → Settings) **не** перечитывает
раздел Storage. Дефект функциональный, не визуальный.

### B3. Lifecycle: polling не связан с видимой страницей
`SetTabActive` для `AutoRefresh`/`EndpointPoll` ставится внутри
`selectSection`, то есть только при смене домена. Уход на
Diagnostics/Settings/About (домен `""`) **не выключает** опрос удалённых
узлов, а возврат из них в Remote — не переключает владельца слотов корректно:
домен не менялся, значит `selectSection` не вызывался.

### B4. `MaxContentWidth` не действует на Home
`h.root = container.NewVBox(...)` кладётся в `contentHost` напрямую, без
`design.ConstrainContent`. Токен `MaxContentWidth` для Home не работает.

### B5. Токены объявлены, но не используются
Проверено по каждому токену (`grep` вне `metrics.go`):

| Токен | Использований | Вывод |
|---|---|---|
| `NavIconSize` | 0 | не действует |
| `NavIconGap` | 0 | не действует |
| `NavItemGap` | 0 | не действует |
| `NavSectionGap` | 0 | не действует |
| `SidebarTopGap` | 0 | не действует |
| `RadiusHero`, `RadiusDialog` | 0 | не действуют |
| `RowHeight`, `ToolbarHeight` | 0 | не действуют |
| `SectionGap`, `BlockGap` | 0 | не действуют |
| `SpaceL/XL/2XL/3XL/4XL/5XL` | 0 | лестница не используется |
| `DefaultWindowWidth/Height`, `MinWindowWidth/Height` | 0 | используются через функции `WindowSize()/MinWindowSize()` — **корректно** |

**Корневая причина:** сайдбар строит строку как
`container.NewBorder(nil, nil, container.NewHBox(r.icon, r.label), nil)`, а
тело — `container.NewVBox(main...)`. `HBox`/`VBox` берут промежуток из
`theme.SizeNamePadding` (у нас **8**), поэтому фактический зазор иконка↔текст
и между строками задаёт тема, а не `NavIconGap`/`NavItemGap`.

### B6. `theme.SizeNamePadding = 8` + явные `SpacerV(...)` дают двойной зазор
Home: `NewVBox(hero, SpacerV(CardGap), runtimeBox, ...)` — VBox добавляет
свой padding (8) плюс `SpacerV(CardGap)` (12) = 20 вместо заявленных 12.

### B7. Статус-бар прокси использует `ScrollBoth`
`ui/clash_api_tab.go:2008-2011`:
```go
statusScroll := container.NewScroll(status)
statusScroll.Direction = container.ScrollBoth
```
Строка статуса уезжает за край — ровно то, что видно внизу скриншота.

### B8. Жёстко заданные светлые цвета строк прокси
`ui/clash_api_tab.go:964,966`:
```go
background.FillColor = color.NRGBA{R:144,G:238,B:144,A:128} // активный
background.FillColor = color.NRGBA{R:135,G:206,B:250,A:128} // выбранный
```
В тёмной теме это светлые плашки. Семантических токенов нет.

### B9. `Proxies` использует иконку `NavLocal`, хотя есть `NavProxies`
`ui/app.go:398`: `{ID: RouteProxies, ..., Icon: icons.NavLocal}`. Ресурс
`icons.NavProxies` существует и не используется.

### B10. Иконки Traffic/Diagnostics визуально легче остальных
`nav_traffic.svg` — 4 тонкие линии; `nav_diagnostics.svg` — одна ломаная.
При 18×18 вес заметно ниже, чем у `nav_home`/`nav_settings`.

## C. Существующий `pageWithHeader` / `pageWithHeaderScroll`

Два разных хелпера в двух файлах, с разными правилами полей:
- `ui/local_remote_tabs.go:131` — `pageWithHeader` (без scroll и без max width)
- `ui/local_remote_tabs.go:~225` — `pageWithHeaderScroll` (со scroll, без max width)

Ни один не задаёт MaxWidth. Отсюда «контролы во всю ширину» в Settings.

## D. Состояние, которое нельзя дублировать

`CreateMachineListPanel(ac, proxies)` создаёт собственную stateful-панель:
`registry` (`services.NewRemoteRegistry`), `health`, `errLog`,
`connectAttempt`, `moreOpen`, goroutine heartbeat (`machine_list_panel.go:483,703`).
`CreateProxyListPanel` — тоже stateful (слоты UIService, виртуализация).

**Вывод:** современная Remote-страница обязана получить ТЕ ЖЕ объекты, а не
создавать вторые. Иначе — два heartbeat, две карты health, два connect-state.

## E. Что НЕ является дефектом (проверено)

- `container.NewAppTabs` в `ui/app.go:148` — compatibility-объект, в дерево
  не попадает; `app.content` им не перезаписывается.
- `CardRow` не кладёт прозрачный holder поверх интерактивного trailing
  (`trailingInteractive` guard).
- `ClickableCard` не имеет overlay-слоя (реализует `Tappable` сам).
- `navRowHolder` покрывает только свою строку; все 7 пунктов кликабельны
  (`TestSidebarEveryItemIsClickable`).
- Start/Stop возвращают кнопку в рабочее состояние (`pendingSettled`).

## F. План правок (порядок)

- **Phase B** — lifecycle: разделить `currentDomain` (транспорт) и
  `currentRoute` (видимость); `applyRouteVisibility(route)` для polling,
  `refreshSettings`, page-refresh. `selectSection` не трогаем.
- **Phase C** — Remote: `createRemoteParts(ac)` → современная страница +
  compatibility-обёртка на ТЕХ ЖЕ объектах. Empty state. Убрать `ScrollBoth`.
- **Phase D** — layout: `VStack/HStack(gap, ...)`, применение токенов сайдбара,
  padding темы 8→5, ограничение ширины кнопок через wrapper (не `Resize`).
- **Phase E** — `PageScaffold` с MaxWidth по типу страницы.
- **Phase F** — Proxies/Machines плотность, toolbar, empty states.
- **Phase G** — иконки (NavProxies, перерисовать traffic/diagnostics, semantic
  цвета строк).
- **Phase H** — чистка комментариев только в затронутых presentation-файлах.

## G. Вне scope

L0–L4, протокол демона, схема конфига, парсер подписок, модель реестра
remote — не трогаем. Remote connection lifetime от смены страницы не зависит.
