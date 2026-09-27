# JiejieBox — установка macOS-клиента

Готовый к установке клиент: **JiejieBox**, Apple Silicon (arm64), macOS 11+.

Это устанавливаемое приложение, а не исходники и не патч. Оно ставится **рядом** с
оригинальным Singbox Launcher и не заменяет его.

---

## 1. Файл для установки

```
dist/JiejieBox-v2.3.2-16-gcc283ca0-jiejiebox-macos-arm64.zip
```

| | |
|---|---|
| Размер | 17 МБ |
| SHA256 | `3a5a887c01ee82e343f0150c3671aab6c3d8d8c7073bdd7d64ba57c042ba2d08` |
| Архитектура | Mach-O 64-bit executable **arm64** (Apple Silicon) |
| Bundle ID | `com.piggycat.jiejiebox` |
| Версия | `v2.3.2-16-gcc283ca0-jiejiebox` |
| Коммит сборки | `cc283ca08f858a65781620c7c6f01318fdea1198` |
| Подпись | **ad-hoc, НЕ нотаризовано** |

Проверить файл перед установкой:

```bash
shasum -a 256 "dist/JiejieBox-v2.3.2-16-gcc283ca0-jiejiebox-macos-arm64.zip"
# должно совпасть с SHA256 выше
```

## 2. Установка

Двойной щелчок по zip распаковывает `JiejieBox.app`. Перетащите его в
`Программы` (`/Applications`). Либо из терминала:

```bash
unzip -q "dist/JiejieBox-v2.3.2-16-gcc283ca0-jiejiebox-macos-arm64.zip" -d /tmp/jb
cp -R /tmp/jb/JiejieBox.app /Applications/
rm -rf /tmp/jb
open /Applications/JiejieBox.app
```

Скрипт умеет сделать это сам:

```bash
build/package_macos.sh arm64 --install
```

Он обновляет **только исполняемый файл**, если приложение уже стоит, и заново
подписывает бандл — данные при этом не трогаются.

### Про Gatekeeper — без обходов

Приложение подписано **ad-hoc** и **не нотаризовано**. `spctl` о нём говорит
`rejected`, и это ожидаемо: у сборки нет Developer ID.

Обход **не требуется** и не предлагается:

- zip собран локально, атрибута `com.apple.quarantine` на нём нет, поэтому
  приложение запускается обычным двойным щелчком;
- `xattr -dr`, отключение Gatekeeper и `spctl --master-disable` **не нужны** —
  если система когда-нибудь откажется запускать сборку, правильное решение —
  подписать её Developer ID и нотаризовать, а не снимать защиту.

Проверить подпись:

```bash
codesign --verify --verbose=1 /Applications/JiejieBox.app
# ожидается: valid on disk / satisfies its Designated Requirement
codesign -dv /Applications/JiejieBox.app 2>&1 | grep -E 'Identifier|Signature'
# Identifier=com.piggycat.jiejiebox, Signature=adhoc
```

## 3. Сосуществование с оригиналом

| | Оригинал | JiejieBox |
|---|---|---|
| Приложение | `/Applications/singbox-launcher.app` | `/Applications/JiejieBox.app` |
| Bundle ID | `com.singbox.launcher` | `com.piggycat.jiejiebox` |
| Имя в Dock/Finder | Singbox Launcher | JiejieBox |
| **Данные** | `~/Library/Application Support/singbox-launcher` | **то же самое** |

Разные Bundle ID и имена приложений означают разные записи в LaunchServices:
оба клиента видны системе как разные программы и не перетирают друг друга.

**Данные намеренно общие.** В этом каталоге уже лежат ваш `bin/config.json` и
ваше кастомное ядро `bin/sing-box`; переносить их нельзя. Новый клиент читает их
же, поэтому:

- **конфигурация сохраняется** — ничего не нужно переносить;
- **кастомное ядро остаётся** — берётся из `bin/sing-box`
  (в логе `Core: … (source: data)`);
- **официальное ядро не подставляется** — в этой сборке нет ядра внутри
  приложения, поэтому подменять нечего, и клиент не предлагает «Reinstall»
  для более новой кастомной версии.

Проверить, какое ядро реально используется:

```bash
/Applications/JiejieBox.app/Contents/MacOS/JiejieBox -paths
```

Ожидается строка `Core: /Users/<вы>/Library/Application Support/singbox-launcher/bin/sing-box (source: data)`
и `version: 1.15.0-jiejie-masquerade.5` в интерфейсе.

## 4. Первый запуск

Ничего вручную создавать не нужно — ни каталогов, ни файлов логов, ни root-копии.

1. **Запустите** `JiejieBox.app`.
2. **Ядро из прошлой сессии.** Если ядро уже работает (например, вы запускали
   клиент раньше), новый клиент **признаёт его своим** и показывает как
   работающее. Диалога «sing-box уже запущен, убить?» больше нет: он предлагал
   снять рабочий VPN.
3. **Root-копия ядра.** Для TUN ядро запускается под root из защищённой копии
   `/Library/PrivilegedHelperTools/sing-box-lxd`. Если копия отсутствует или
   отличается от текущего ядра, клиент покажет готовую команду — скопируйте её
   в Терминал. Для вашего ядра (в нём нет `lxd`) команда копирует файл и
   выставляет владельца и права:

   ```bash
   sudo /bin/mkdir -p '/Library/PrivilegedHelperTools' \
    && sudo /bin/cp -f "$HOME/Library/Application Support/singbox-launcher/bin/sing-box" \
         '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
    && sudo /usr/sbin/chown root:wheel '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
    && sudo /bin/chmod 0755 '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
    && sudo /bin/mv -f '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
         '/Library/PrivilegedHelperTools/sing-box-lxd'
   ```

   Состояние копии видно заранее: **Настройки → Storage**, строка `Root copy`
   (`совпадает с текущим ядром` / `не текущее ядро — синхронизируйте`).
4. **Авторизация TUN.** При первом запуске с TUN macOS один раз спросит пароль —
   это системный диалог. Пароль вводите вы; клиент его не сохраняет и не читает.
5. **Логи создаются сами.** Каталог `/Library/Logs/sing-box-lxd` (root:wheel,
   0755) и файл `classic.log` (владелец — вы, 0600) создаёт сам запуск. Каталог
   `~/Library/Logs/singbox-launcher/` тоже создаётся автоматически.
6. **Clash API.** Если в конфиге есть `experimental.clash_api`, клиент
   подключается к нему сам. При пустом `secret` он работает без заголовка
   авторизации (это допустимый режим ядра), при заданном — с ним. Секрет никуда
   не печатается и не попадает в URL или логи.

## 5. Откат

JiejieBox не изменяет оригинальное приложение, поэтому откат — это просто
удаление нового клиента:

```bash
# Закрыть клиент, если запущен
osascript -e 'tell application "JiejieBox" to quit' 2>/dev/null || true
rm -rf /Applications/JiejieBox.app
```

Оригинальный `singbox-launcher.app` и все данные остаются на месте. Если нужно
вернуться к прежней сборке JiejieBox — установите предыдущий zip тем же способом.

Данные (конфиг, ядро, состояние) откат не затрагивает: они общие и лежат вне
приложения.

## 6. Что проверено на этой машине

Проверено в реальной среде с работающим ядром:

| Проверка | Результат |
|---|---|
| Сборка и подпись | `codesign --verify` → valid; Identifier `com.piggycat.jiejiebox`; adhoc |
| Установка | zip распаковывается, приложение запускается из распакованной копии |
| Сосуществование | Bundle ID отличается от оригинала (`com.singbox.launcher`) |
| Сохранение данных | `-paths` показывает ваш каталог данных и ядро `source: data` |
| Запуск GUI | приложение стартует, окно и трей поднимаются |
| Своё ядро | признаёт ядро прошлой сессии: `adopting this launcher's own core`, диалога убийства нет |
| Clash API | `/version` → 200 с секретом, **401** без секрета |
| Список узлов | `Successfully loaded 5 proxies for group '🌍 国外流量'` |
| Версия ядра | API отвечает `sing-box 1.15.0-jiejie-masquerade.5` (кастомное) |
| Root TUN | `/bin/sh -pc` пишет в root-owned `/Library/Logs/sing-box-lxd`; создан utun |
| Трафик через TUN | публичный IPv4 получен через туннель |
| Логи | 1.69 МБ `classic.log`, каталоги и файлы созданы автоматически |
| Остановка | GUI закрывается, ядро и маршруты корректно снимаются |
| Восстановление сети | после остановки связь есть (HTTP 200, публичный IP получен) |

Сборка запускалась, останавливалась и снова запускалась; сеть после остановки
восстанавливалась.

## 7. Что НЕ проверено

- **Авторизация TUN из этого клиента (диалог пароля).** Среда сборки не может
  показать системный диалог авторизации: `AuthorizationCreate` возвращает
  `-60008` (`errAuthorizationInteractionNotAllowed`), потому что
  `SecurityAgent` недоступен в сессии `gui/501`. Реальный запуск под root
  подтверждён косвенно и по существу: работает `/bin/sh -pc`, ядро пишет в
  root-owned каталог, создан TUN, трафик идёт. Но **сам диалог пароля вы
  увидите только при запуске из GUI** — это единственный шаг, который
  выполняется вами.
- **Установка в `/Applications`.** Каталог `/Applications` недоступен на запись
  из среды сборки, поэтому установка проверена распаковкой в другое место.
  Команды выше — стандартные; скрипт `--install` обновляет только исполняемый
  файл.
- **Нотаризация.** Сборка ad-hoc и не нотаризована; Gatekeeper отвечает
  `rejected`. Обход не требуется, потому что карантина на файле нет.
- **Сборка `.dmg`.** `hdiutil`/`diskutil` в этой среде запрещены
  («операция не разрешена»), поэтому поставка — zip. Скрипт соберёт `.dmg`
  автоматически при запуске в обычной сессии macOS.
- **Naive IPv6 UDP и расхождение TUN/mixed** — не трогали, причина не найдена,
  это отдельная диагностика.

## 8. Если что-то пошло не так

| Симптом | Что делать |
|---|---|
| Ядро не берёт root-копию | Настройки → Storage, строка `Root copy`; выполните показанную команду |
| Порт 7890 занят | Клиент классифицирует это как детерминированную ошибку и **не** перезапускает ядро по кругу; освободите порт или смените его в конфиге |
| API «отключён» | Проверьте `experimental.clash_api` в `~/Library/Application Support/singbox-launcher/bin/config.json` |
| Нужно вернуть прежнее поведение | Удалите `/Applications/JiejieBox.app`; оригинал и данные не тронуты |
| Собрать заново | `build/package_macos.sh arm64` (нужны Xcode и Go) |
