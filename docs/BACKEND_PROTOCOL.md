# Backend Protocol — JiejieBox menu bar app

**Language**: English

The JiejieBox macOS app is two processes:

```
JiejieBox.app/Contents/MacOS/JiejieBox     SwiftUI menu bar frontend
JiejieBox.app/Contents/Helpers/jiejiebox-backend   Go backend (no GUI toolkit)
```

The frontend owns presentation only. The backend owns **all** business state:
whether the core is running, which proxy is selected, which settings are stored.
They speak a line-delimited JSON protocol over the helper's stdin/stdout.

This document is the contract. `backend/service/contract_test.go` pins the same
shapes by raw JSON key, because a rename here breaks the Swift client at runtime
with no compile error on either side.

---

## 1. Transport

- **Framing**: one JSON object per line (`\n`-terminated), both directions.
- **Streams**: requests go to the backend's stdin; responses and events come
  back on stdout as separate lines. stderr carries backend logs.
- **Correlation**: every request has an `id` (a **string**); the response echoes
  it. A non-string `id` is rejected with `bad_request`.
- **Malformed frames do not kill the loop**: a bad line produces an `error`
  response and the backend keeps serving.

### Request

```json
{"id": "7", "method": "start_core", "params": {}}
```

`params` is optional and carries method-specific values (strings, booleans,
numbers, lists).

### Response

```json
{"id": "7", "result": { ... }}
{"id": "8", "error": {"code": "mode_locked", "message": "...", "recoverable": true}}
```

Exactly one of `result` / `error` is present.

### Event

```json
{"event": "core_state_changed", "seq": 12, "payload": { ... }}
```

- `seq` is **monotonic** across all events.
- The snapshot carries `snapshot_seq`: the event sequence at the moment it was
  taken. A client must discard any event whose `seq` is `<= snapshot_seq`,
  which is what makes "subscribe before snapshot" race-free.

---

## 2. Methods

| Method | Params | Result | Notes |
|---|---|---|---|
| `handshake` | — | `HandshakeResult` | Version + capabilities |
| `get_app_snapshot` | — | `AppSnapshot` | Complete initial state |
| `subscribe` | — | `{subscribed: true}` | Acknowledge; events follow |
| `start_core` | — | `CoreState` | Start sing-box |
| `stop_core` | — | `CoreState` | Stop sing-box |
| `restart_core` | — | `CoreState` | Kill and let the supervisor restart |
| `shutdown` | — | `{shutting_down: true}` | Backend exits; **core fate is the backend's decision** |
| `set_core_mode` | `mode` | `AppSnapshot` | `classic` / `daemon` |
| `set_auto_ping` | `enabled` | `SettingsState` | |
| `set_auto_update_subscriptions` | `enabled` | `SettingsState` | |
| `get_proxy_groups` | — | `ProxyList` | Selector groups from config |
| `get_proxies` | `group` | `ProxyList` | Empty group = config default |
| `switch_proxy` | `group`, `name` | `ProxyList` | Re-read after switching |
| `test_proxy` | `group`, `name` | `ProxyList` | Measure one node |
| `test_proxy_group` | `group` | `ProxyList` | Measure all nodes, sequential |
| `reload_config` | — | `MaintenanceResult` | Forced full rebuild |
| `update_subscriptions` | — | `MaintenanceResult` | Refresh all sources |

Unknown methods return `unknown_method`.

---

## 3. Events

| Event | Payload | Meaning |
|---|---|---|
| `handshake_ready` | `HandshakeResult` | Backend initialised |
| `core_state_changed` | `CoreState` | Core transitioned |
| `settings_changed` | `SettingsState` | A business setting changed |
| `proxies_changed` | `{reason}` | Config rebuilt / subs refreshed |
| `proxy_selection_changed` | `{group, name}` | A group switched node |
| `traffic_rate` | `TrafficRate` | ~1 Hz speed sample |
| `log_line` | `{line}` | Backend log line |
| `error` | `BackendError` | Non-fatal problem |
| `shutting_down` | `null` | Backend is exiting |

### `core_state_changed` is a real transition, not a command echo

The backend subscribes to the core's typed `VpnStateChanged` event bus, so the
frontend is notified when the core **changes state for any reason** — including
a crash, an external kill, or the crash handler's supervisor restart. It is not
emitted only after a `start_core`/`stop_core` call.

This distinction matters: with command-only emission, a core that died on its
own would leave the menu bar showing "Connected" indefinitely.
`RunningState.Set` de-duplicates no-op transitions, so a poll loop does not
produce a flood of events.

---

## 4. Types

```jsonc
// HandshakeResult
{"protocol_version": 1, "backend_version": "…", "pid": 123,
 "capabilities": {"daemon": true, "elevation": true, "remote": true,
                  "traffic": true, "configurator": true}}

// AppSnapshot
{"snapshot_seq": 0, "handshake": {…}, "core": {…}, "settings": {…}}

// CoreState
{"state": "stopped|starting|running|stopping|error",
 "binary_exists": true, "config_exists": true,
 "core_version": "1.15.0", "backend": "classic|daemon",
 "error_message": "…"}          // omitted when empty

// SettingsState
{"language": "en", "core_backend_mode": "classic",
 "auto_ping_after_connect": true, "auto_update_subscriptions": true,
 "data_dir": "/Users/…/Library/Application Support/singbox-launcher",
 "config_path": "…/bin/config.json",
 "logs_dir": "/Users/…/Library/Logs/singbox-launcher"}

// ProxyList
{"groups": [{"name": "proxy-out", "display_name": "proxy-out",
             "type": "Selector", "selected": "Node A",
             "selected_display": "Node A", "count": 3}],
 "proxies": [{"name": "Node A", "display_name": "Node A", "type": "VLESS",
              "delay": 42, "group": "proxy-out", "selected": true,
              "last_error": "…"}],   // omitted when empty
 "group": "proxy-out",
 "available": true}

// TrafficRate
{"up": 2048, "down": 102400, "total_up": 500000, "total_down": 9000000,
 "at_unix_ms": 1700000000000}

// MaintenanceResult
{"ok": true, "message": "5 nodes from 2 sources.",
 "total_sources": 2, "succeeded_sources": 2, "failed_sources": 0,
 "nodes_count": 5, "core_skips": ["1 vless node(s) skipped: …"]}

// Error
{"code": "mode_locked", "message": "…", "recoverable": true}
```

### `delay: -1` means "never measured"

It is **not** 0 ms. Zero is a legitimate reading, and conflating the two would
show an untested node as instant. The UI renders `-1` as "—".

### `available: false` is not an error

`ProxyList.available` is false when no Clash API endpoint is reachable — the
normal state while the core is stopped. It lets the UI say "start the core
first" instead of showing a broken empty list.

---

## 5. Error codes

| Code | Recoverable | Meaning |
|---|---|---|
| `bad_request` | no | Malformed request (e.g. blank proxy name) |
| `bad_mode` | no | `set_core_mode` got something other than classic/daemon |
| `unknown_method` | no | Method not implemented |
| `not_ready` | yes | Backend not initialised |
| `mode_locked` | yes | Core is running; stop it before switching engines |
| `core_not_running` | yes | Start the core before switching/testing proxies |
| `config_unreadable` | yes | config.json missing or unparsable |
| `proxy_list_failed` | yes | The Clash API could not be read |
| `switch_failed` | yes | The core refused the switch |
| `rebuild_failed` | yes | Config rebuild failed |
| `update_failed` | yes | Subscription refresh failed |
| `interrupted` | yes | The system went to sleep mid-request |
| `persist_failed` | yes | State changed but could not be saved |

`recoverable` describes whether retrying after the stated condition can help —
it is advice for the UI, not a guarantee.

---

## 6. Lifecycle rules the frontend must respect

1. **Subscribe before snapshot.** Events that race the snapshot are filtered by
   `snapshot_seq`, not lost.
2. **Quit never pre-stops the core.** The frontend sends `shutdown` and lets the
   backend run its graceful-exit policy. In classic mode that stops the core; in
   daemon mode with keep-running enabled it deliberately leaves it. Calling
   `stop_core` first would break daemon persistence.
3. **Never optimistically flip state.** Every command's result is re-read from
   the core, so a switch that silently failed does not look applied.
4. **Operations that can half-succeed report it.** `MaintenanceResult.ok` is
   false when a refresh ran but every source failed — a case that returns no
   error yet changed nothing.
