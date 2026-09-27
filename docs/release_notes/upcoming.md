# Upcoming release — черновик

Сюда складываем пункты, которые войдут в следующий релиз. Перед релизом переносим в `X-Y-Z.md` и очищаем этот файл.

## EN
### Highlights
- **Quitting the launcher no longer disconnects the VPN** in Daemon mode. The GUI and the core are now fully decoupled: close the JiejieBox window and the tunnel stays up — node selection, subscription cache and fake-ip state are all preserved.
- **Relaunching re-attaches** to the running core instead of restarting it. The launcher discovers the live daemon, restores the server list, logs and traffic, and shows Running — without touching the working core.
- **"Stop VPN" and "Quit Launcher" are now clearly different actions.** Quit only closes the window; use Stop VPN to actually disconnect.

### Technical / Internal
- `persistentCoreBackend` capability + `AppController.CorePersistsAfterAppExit()`; `LegacyBackend` answers `false` (never detaches), `DaemonBackend` answers `!DaemonStopVPNOnExit`.
- `EnsureVPNRunning()` replaces the blind start on `-start`: it probes the daemon (`/admin/status`) so an already-running core is never re-applied. An unreachable daemon reports "unknown" and does not veto auto-start.
- Daemon settings tab now shows a positive **"Keep VPN running after quitting the launcher"** checkbox plus an explanatory hint. The stored field is unchanged (`daemon_stop_vpn_on_exit`), so no settings migration is needed.
- Classic mode is untouched: it still stops the launcher-owned core on exit and waits for it.
- Forced-exit watchdog wording now distinguishes a real classic orphan (warning) from a daemon core that is *supposed* to outlive the GUI (INFO).

## RU
### Основное
- **Выход из лаунчера больше не разрывает VPN** в daemon-режиме. GUI и ядро полностью развязаны: закройте окно JiejieBox — туннель останется поднятым, вместе с выбранным узлом, кэшем подписки и fake-ip.
- **Повторный запуск присоединяется** к работающему ядру, а не перезапускает его. Лаунчер находит живой демон, восстанавливает список серверов, логи и трафик и показывает Running — не трогая работающее ядро.
- **«Stop VPN» и «Quit Launcher» — теперь разные действия.** Quit только закрывает окно; чтобы действительно отключиться, нужен Stop VPN.

### Техническое / Внутреннее
- Возможность `persistentCoreBackend` + `AppController.CorePersistsAfterAppExit()`; `LegacyBackend` отвечает `false` (никогда не отсоединяется), `DaemonBackend` — `!DaemonStopVPNOnExit`.
- `EnsureVPNRunning()` вместо слепого старта по `-start`: состояние спрашивается у демона (`/admin/status`), поэтому работающее ядро не получает лишний apply. Недоступный демон даёт «неизвестно» и не блокирует автозапуск.
- В daemon-вкладке настроек — положительный чекбокс **«Keep VPN running after quitting the launcher»** с пояснением. Хранимое поле не изменилось (`daemon_stop_vpn_on_exit`), миграция настроек не нужна.
- Classic-режим не затронут: ядро по-прежнему принадлежит лаунчеру, гасится при выходе, и выход его дожидается.
- Формулировка сторожевого таймера принудительного выхода различает настоящий classic-orphan (warning) и ядро демона, которое и *должно* пережить GUI (INFO).
