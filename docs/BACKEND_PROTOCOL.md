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

### Version policy

`protocol_version` is bumped only for a **breaking** change to a request,
response or event shape.

The two import methods (`import_core_file`, `import_subscription_file`), the
`core_import` / `local_subscription_import` capabilities and the
`input_kind` / `can_refresh` / `filename` fields on `SubscriptionDTO` were added
**without** a bump: they are purely additive. New methods were previously
`unknown_method`, and every new field is either optional in the JSON or absent
from an older backend — the Swift DTOs declare the capabilities as `Bool?` and
the subscription fields as `String?`/`Bool?`, each with a default that means
"this feature does not exist here". A client therefore still asks the
capability block before offering either control, and an older backend that omits
the keys simply never shows them.

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
{"event": "core_state_changed", "seq": 12, "session": "9f3c…", "payload": { ... }}
```

- `seq` is **monotonic across all events** *within one backend process*.
- `session` identifies that process. It is minted once per backend instance and
  stamped on every event and every snapshot.

> **`seq` is only meaningful together with `session`.** The number is a per-process
> counter, but a client's high-water mark outlives any one process. Without the
> session, a restarted backend counts from 1 while the client still holds e.g. 137, so
> **every** event from the new process compares as stale and is dropped: the UI freezes
> until the new backend happens to emit more events than the old one ever did.
>
> A client must therefore scope its mark to the session and re-baseline when the
> session changes — and must **drop** events from a session it is not following. The
> two mistakes here are opposite (dropping the live backend's events, applying the dead
> one's) and both come from the same ambiguity.

- The snapshot carries `snapshot_seq` **and** `session`: the sequence, at the moment it
  was taken, of the session that took it. The sequence and the snapshot's contents are
  captured in one critical section, so the number is a real version point rather than a
  timestamp for content that had not been read yet.
- Events that race the snapshot are buffered and replayed after it, filtered against
  `snapshot_seq`. Applying an event first and the snapshot on top is a **rollback**: the
  snapshot was composed at an earlier sequence and describes the older state, and the
  event has been consumed, so the UI stays wrong until something unrelated happens.

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
| `list_subscriptions` | — | `{subscriptions: [SubscriptionDTO]}` | Configured sources |
| `add_subscription` | `name`, `url` | `SubscriptionDTO` | URL required; blank name derives from the host |
| `update_subscription` | `id`, `name`, `url`, `enabled?` | `SubscriptionDTO` | Omitted fields are left unchanged |
| `remove_subscription` | `id` | `{subscriptions: [...]}` | Returns the remaining list |
| `set_subscription_enabled` | `id`, `enabled` | `SubscriptionDTO` | |
| `refresh_subscription` | `id` | `SubscriptionDTO` | Fetches one source; marks config stale. Refused with `not_refreshable` for a local snapshot |
| `import_core_file` | `path` | `CoreImportResult` | Install a user-selected sing-box binary as the Data core. Gated by the `core_import` capability |
| `import_subscription_file` | `path` | `SubscriptionImportResult` | Store a local file's nodes as a new source. Gated by the `local_subscription_import` capability |
| `get_daemon_status` | — | `DaemonStatusDTO` | Setup state; contains no secret |
| `daemon_install` | — | `DaemonCommandResult` | Command for the user to run |
| `daemon_start` | — | `DaemonCommandResult` | Loads the installed service |
| `daemon_repair` | — | `DaemonCommandResult` | Fresh pairing invite |
| `daemon_uninstall` | `purge` | `DaemonCommandResult` | Removes the service |
| `pair_daemon` | `invite` | `DaemonStatusDTO` | Enrols from a pasted invite |
| `unpair_daemon` | — | `DaemonStatusDTO` | Drops the local pairing |
| `set_daemon_keep_running` | `enabled` | `DaemonStatusDTO` | Positive phrasing of the exit policy |

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
| `subscriptions_changed` | `null` | The source list was edited |
| `daemon_changed` | `null` | Daemon setup state changed |

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
 "binary_exists": true, "config_exists": true, "config_stale": false,
 "config_rebuildable": false,
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

// SubscriptionDTO
{"id": "01M3…", "name": "provider.example", "url": "https://…",
 "enabled": true, "node_count": 128, "max_nodes": 0,
 "profile_title": "My Airport", "support_url": "https://…",
 "last_attempt": "2026-09-27T15:00:00Z", "last_success": "…",
 "last_status": "ok", "last_error": "", "http_status_code": 200,
 "nodes_fetched": 128}

// DaemonStatusDTO
{"supported": true, "service": "not_installed|unsafe|stale|not_running|process_stale|ok",
 "installed": false, "paired": false, "reachable": false, "ready": false,
 "active_mode": false, "core_supports_lxd": true,
 "needs_install": false, "needs_start": false,
 "address": "127.0.0.1:19091", "fingerprint": "ab12…", "core_status": "idle",
 "daemon_version": "…", "running_version": "…", "launcher_version": "…",
 "persists_after_quit": true, "error": ""}

// DaemonCommandResult
{"operation": "install", "command": "sudo '/…/sing-box' lxd --service=install",
 "available": true, "message": "Run the command in Terminal…",
 "needs_admin": true, "follow_up": "pair", "status": {…}}

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

### `config_rebuildable` decides whether Reload is offered

A rebuild replays the wizard state, so it is only valid for a config the launcher
itself built. `config_rebuildable` reports ownership, determined by an explicit
marker written beside config.json when the launcher builds it — **not** by
whether a state file exists, because the subscription manager creates one the
first time a source is added and that would misattribute an external config.

| Case | Value |
|---|---|
| No config.json | `true` — a rebuild is how the file comes into existence |
| config.json + our marker | `true` — we built it |
| config.json, no marker | `false` — somebody else owns this file |

The frontend uses this to choose its affordance (Reload, or Open Config with an
explanation). The backend enforces the same rule independently: hiding a button
is presentation, not a security boundary.

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
| `proxy_list_failed` | yes | The Clash API could not be read. NOT used for an engine that cannot list proxies — that is reported as `supported: false` in a successful `ProxyList` |
| `switch_failed` | yes | The core refused the switch |
| `rebuild_failed` | yes | Config rebuild failed |
| `update_failed` | yes | Subscription refresh failed |
| `interrupted` | yes | The system went to sleep mid-request |
| `persist_failed` | yes | State changed but could not be saved |
| `state_unreadable` | yes | state.json exists but could not be parsed |
| `duplicate` | no | That subscription URL is already configured |
| `not_found` | no | No subscription with that id |
| `refresh_failed` | yes | The provider fetch failed; the source is kept |
| `save_failed` | yes | Could not write state.json |
| `bad_invite` | no | The pasted invite is not address#fingerprint#code |
| `pair_failed` | yes | Enrolment with the service failed |
| `unpair_failed` | yes | Could not remove the local pairing |
| `bad_path` | no | An import was called without a usable path |
| `file_not_found` | no | The selected import file does not exist |
| `not_regular_file` | no | The selected path is a directory or device |
| `file_too_large` | no | Over the core cap (256 MiB) or the shared subscription cap (10 MiB) |
| `wrong_architecture` | no | The core candidate has no slice for this machine |
| `invalid_core` | no | The candidate runs but is not a sing-box core |
| `unsupported` | no | Core import is not implemented on this platform |
| `core_busy` | yes | Stop the core before replacing it |
| `core_override_active` | no | `SINGBOX_LAUNCHER_CORE` overrides the Data core |
| `already_installed` | no | The selected core is already the installed one |
| `core_config_incompatible` | no | The candidate core cannot parse the current config; the installed core is left unchanged |
| `install_failed` | yes | The staged copy or the final replace failed |
| `decode_failed` | no | The subscription file could not be decoded at all |
| `unsupported_format` | no | Decoded, but is not a recognisable subscription |
| `parse_failed` | no | Recognised, but yielded no parsable material |
| `no_nodes` | no | Parsed, but contains no usable proxy nodes |
| `not_refreshable` | no | This source has no provider to refresh from (a local snapshot) |

`recoverable` describes whether retrying after the stated condition can help —
it is advice for the UI, not a guarantee.

---

## 6. Lifecycle rules the frontend must respect

1. **Subscribe before snapshot.** Events that race the snapshot are buffered and
   replayed on top of it, filtered by `snapshot_seq` — not lost, and not applied
   underneath it. Both the sequence and the session matter (see §1).
2. **Quit never pre-stops the core.** The frontend sends `shutdown` and lets the
   backend run its graceful-exit policy. In classic mode that stops the core; in
   daemon mode with keep-running enabled it deliberately leaves it. Calling
   `stop_core` first would break daemon persistence.
3. **Never optimistically flip state.** Every command's result is re-read from
   the core, so a switch that silently failed does not look applied.
4. **Operations that can half-succeed report it.** `MaintenanceResult.ok` is the
   OVERALL verdict, derived from both phases rather than assumed from the first.
   `refresh_ok` and `rebuild_ok` are reported separately, because they fail
   independently and the difference is what the user needs: a refresh that worked with
   a rebuild that failed means the node list moved while the **running** config did
   not — the most misleading outcome available, since everything the user can see says
   it worked. `rebuild_error` carries the reason and `config_stale` says whether
   config.json still lags the state.
5. **Editing sources never rebuilds.** Subscriptions and Daemon are the surfaces
   a menu bar actually touches, and both follow the product rule that rebuilding
   the config is the user's decision: `config_stale` is reported, and the UI
   offers Reload. `config_stale` is derived from the core's dirty markers **and**
   from comparing `state.json` against `config.json`, so it survives a restart.
6. **Daemon setup and activation are separate.** Setup returns a command for the
   user to run; only a status reporting `installed && paired && reachable` may be
   activated. Conflating the two is what made the engine switch look like a hang.
7. **Every request is bounded by the client.** Per-method timeouts (8 s reads,
   15 s daemon status, 20 s engine commands, 120 s network work) mean a lost
   response becomes an error instead of a permanently pending button.
