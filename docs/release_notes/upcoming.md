# Upcoming release — черновик

Сюда складываем пункты, которые войдут в следующий релиз. Перед релизом переносим в `X-Y-Z.md` и очищаем этот файл.

## EN
### Highlights
- **Your own sing-box core now works with Daemon (lxd) mode.** The launcher no longer audits the core's version, tag or release name before letting you install the service. A custom build such as `1.15.0-jiejie-masquerade.5`, `custom-build`, `unknown` or an unreadable version gets the same Install / Update / Pair / Start as a numbered `sing-box-lx` release.
- **The daemon page always shows the install command.** The yellow "not a numbered sing-box-lx release … Update the core first: Download/Reinstall v1.14.2-lx.4" notice is gone, and nothing on that page downloads or substitutes an upstream core — installation uses the core the launcher already ships.
- **A core/service version difference is status, not an error.** It is reported alongside the command that aligns the service copy with the launcher core, and never blocks anything.
- **Quitting the launcher no longer disconnects the VPN** in Daemon mode. The GUI and the core are now fully decoupled: close the JiejieBox window and the tunnel stays up — node selection, subscription cache and fake-ip state are all preserved.
- **Relaunching re-attaches** to the running core instead of restarting it. The launcher discovers the live daemon, restores the server list, logs and traffic, and shows Running — without touching the working core.
- **"Stop VPN" and "Quit Launcher" are now clearly different actions.** Quit only closes the window; use Stop VPN to actually disconnect.

### Technical / Internal
- Removed the core-provenance gate: `serviceCoreGate` is now always nil, `InstallSupported()` no longer consults the version, and `parseCoreBuild`/`compareCoreBuilds`/`minCoreForRootOwnedService` are deleted. The `core_too_old` service state, its blocking UI branch and its locale strings are gone.
- **Nothing protective was weakened.** The root-owned copy in `/Library/PrivilegedHelperTools` (root:wheel 0755, sudo, canonical LaunchDaemon plist, `.install.json` sidecar), mTLS with certificate-fingerprint pinning, pairing, path escaping and command quoting are all unchanged. What was removed is the *pre-flight opinion* about a core, not the isolation around it.
- Capability is now settled by execution: a core without the subcommand answers `unknown command "lxd"` and that genuine error reaches the user instead of a synthesised "unsupported release".
- `persistentCoreBackend` capability + `AppController.CorePersistsAfterAppExit()`; `LegacyBackend` answers `false` (never detaches), `DaemonBackend` answers `!DaemonStopVPNOnExit`.
- `EnsureVPNRunning()` replaces the blind start on `-start`: it probes the daemon (`/admin/status`) so an already-running core is never re-applied. An unreachable daemon reports "unknown" and does not veto auto-start.
- Daemon settings tab shows a positive **"Keep VPN running after quitting the launcher"** checkbox plus an explanatory hint. The stored field is unchanged (`daemon_stop_vpn_on_exit`), so no settings migration is needed.
- Classic mode is untouched: it still stops the launcher-owned core on exit and waits for it.
- Forced-exit watchdog wording now distinguishes a real classic orphan (warning) from a daemon core that is *supposed* to outlive the GUI (INFO).

## RU
### Основное
- **Своё ядро sing-box теперь работает с Daemon (lxd).** Лаунчер больше не проверяет версию, тег и имя релиза ядра, прежде чем разрешить установку службы. Кастомная сборка вроде `1.15.0-jiejie-masquerade.5`, `custom-build`, `unknown` или нечитаемая версия получает те же Install / Update / Pair / Start, что и нумерованный релиз `sing-box-lx`.
- **Страница Daemon всегда показывает команду установки.** Жёлтое уведомление «not a numbered sing-box-lx release … Update the core first: Download/Reinstall v1.14.2-lx.4» удалено, и ничто на этой странице не скачивает и не подменяет upstream-ядро — установка идёт ядром из комплекта лаунчера.
- **Расхождение версий ядра и службы — статус, а не ошибка.** Оно показывается вместе с командой, которая приводит копию службы к ядру лаунчера, и ничего не блокирует.
- **Выход из лаунчера больше не разрывает VPN** в daemon-режиме. GUI и ядро полностью развязаны: закройте окно JiejieBox — туннель останется поднятым, вместе с выбранным узлом, кэшем подписки и fake-ip.
- **Повторный запуск присоединяется** к работающему ядру, а не перезапускает его. Лаунчер находит живой демон, восстанавливает список серверов, логи и трафик и показывает Running — не трогая работающее ядро.
- **«Stop VPN» и «Quit Launcher» — теперь разные действия.** Quit только закрывает окно; чтобы действительно отключиться, нужен Stop VPN.

### Техническое / Внутреннее
- Гейт происхождения ядра удалён: `serviceCoreGate` теперь всегда nil, `InstallSupported()` не смотрит на версию, а `parseCoreBuild`/`compareCoreBuilds`/`minCoreForRootOwnedService` удалены. Состояние службы `core_too_old`, его блокирующая ветка UI и её тексты локализации убраны.
- **Ничего защитного не ослаблено.** Root-owned копия в `/Library/PrivilegedHelperTools` (root:wheel 0755, sudo, канонический plist LaunchDaemon, сайдкар `.install.json`), mTLS с пином отпечатка сертификата, сопряжение, экранирование путей и квотирование команд — без изменений. Убрано *предварительное мнение* о ядре, а не изоляция вокруг него.
- Способность определяется запуском: ядро без сабкоманды отвечает `unknown command "lxd"`, и до пользователя доходит именно эта настоящая ошибка вместо синтетического «unsupported release».
- Возможность `persistentCoreBackend` + `AppController.CorePersistsAfterAppExit()`; `LegacyBackend` отвечает `false` (никогда не отсоединяется), `DaemonBackend` — `!DaemonStopVPNOnExit`.
- `EnsureVPNRunning()` вместо слепого старта по `-start`: состояние спрашивается у демона (`/admin/status`), поэтому работающее ядро не получает лишний apply. Недоступный демон даёт «неизвестно» и не блокирует автозапуск.
- В daemon-вкладке настроек — положительный чекбокс **«Keep VPN running after quitting the launcher»** с пояснением. Хранимое поле не изменилось (`daemon_stop_vpn_on_exit`), миграция настроек не нужна.
- Classic-режим не затронут: ядро по-прежнему принадлежит лаунчеру, гасится при выходе, и выход его дожидается.
- Формулировка сторожевого таймера принудительного выхода различает настоящий classic-orphan (warning) и ядро демона, которое и *должно* пережить GUI (INFO).
