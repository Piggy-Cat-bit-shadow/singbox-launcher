# Proxy / daemon audit — why `GetGroups` failed, and what else was wrong

**Language**: English

Audit of the reported failure in daemon mode, the capability model it exposed,
and the surrounding proxy/core flows. Every claim is marked with how it was
established:

- **read** — traced in the code;
- **test** — pinned by a named Go test that fails if it regresses;
- **IPC** — exercised against the packaged backend helper over the real protocol.

---

## 1. Root cause

### The reported error

```
cannot read the proxies of group "🌍 国外流量":
daemon GetGroups: rpc error: code = Unimplemented
desc = unknown method GetGroups for service daemon.StartedService
```

### The call chain, traced

```
Swift  ProxiesView .task  →  AppModel.loadGroups()
  │
  │  IPC  { "method": "get_proxy_groups" }        (or get_proxies)
  ▼
Go     Backend.ProxyGroups()            backend/service/proxies.go
  →    b.transport()                    backend/service/proxies.go:35
  │      daemon mode ⇒ APIService.TransportOverride() != nil
  ▼
       daemonProxyTransport              core/backend_daemon.go
  →    GroupProxies(group)               core/backend_daemon.go
  →    client.GetGroups(ctx, &emptypb.Empty{})        ← the gRPC call
  │
  ▼    the running daemon answers codes.Unimplemented
       wrapped as "daemon GetGroups: rpc error: …"
  ▼
Go     returned as protocol.Error{code: "proxy_list_failed"}   ← HARD ERROR
  ▼
Swift  AppModel set proxyError AND lastError
  ▼
Swift  HomeView rendered lastError as a global red banner
```

### Why the method "does not exist" even though it is declared

`GetGroups` **is** declared in `internal/daemonpb/started_service_grpc.pb.go` —
the generated client has had it since the daemon engine was introduced
(`git log -S` on the generated file). So this is not a missing stub and not a
typo: the launcher's proto and the **running daemon binary** disagree.

The daemon is an external process — `sing-box lxd`, built from the fork — and the
group RPCs arrive with the fork's `lx` command surface. A daemon built without
that surface answers `Unimplemented` for a method the client believes exists.
The launcher only knows the daemon is *reachable*; nothing verified that it
implements the methods the proxy screen needs.

### The actual defect

Not the missing RPC — that is a legitimate state of an older or differently
built daemon. The defect is that the launcher treated **"this engine has no such
capability"** as **"something went wrong"**, in three connected places:

1. `daemonProxyTransport.GroupProxies` wrapped `Unimplemented` in a generic
   `fmt.Errorf`, discarding the distinction the gRPC status code carries.
2. `Backend.Proxies` turned any transport error into
   `proxy_list_failed` — a *recoverable* error, implying a retry could help when
   no retry ever would.
3. `AppModel.loadGroups`/`loadProxies` copied the failure into `lastError`,
   which `HomeView` renders as a **global** banner — so a limitation of one
   screen's engine capability took over the app's main screen and displaced the
   core and config status the user actually needs.

The chain had a fourth problem: `AppModel` fired `loadGroups()` on every
core-start (`coreStateChanged`), so on such a daemon the error reappeared on
every start, looking like an app defect rather than an engine limit.

### The fix

Keep the capability fact, and carry it honestly to the UI. Three layers:

| Layer | Change |
|---|---|
| transport | `services.ErrProxyListUnsupported` sentinel; `GroupProxies` / `SwitchProxy` / `Delay` return it when the daemon answers `Unimplemented`, using the existing `isUnimplemented` helper |
| backend | `ProxyList` gains `supported` + `unsupported_reason`; the unsupported case returns them in a **successful** response instead of an error (the error path survives for genuine failures) |
| frontend | new `ProxyListState.unsupportedByEngine`, ranked above `.failed`; the view explains it with no Retry, and proxy failures no longer set `lastError` |

`isUnimplemented` already existed and is already used this way for
`SetChainPositionEnabled` (`ErrChainToggleUnsupported`), so this follows the
established pattern rather than inventing one. *read, test, IPC*

---

## 2. Capability / runtime support matrix

`supported` answers "can this engine do it at all"; `available` answers "is it up
right now". They are separate fields precisely because they need separate
screens.

| State | `supported` | `available` | Proxies screen | Home |
|---|---|---|---|---|
| classic + core running | true | true | node list, switching, testing | summary shown |
| classic + core stopped | true | false | "Core is not running" + start hint | `—` |
| daemon + service ready | true\* | true | node list | summary shown |
| daemon + installed, not ready | true\* | false | "Core is not running" / backend-down | `—` |
| daemon + no group RPC | **false** | false | **explanation + link to Core Mode, no Retry** | `—`, no banner |
| backend unavailable | — | — | `BackendDownView` + Restart | blocking banner (P0) |
| config with no selector groups | true | true | "No selector groups" + next step | not offered |
| external config | true | true | node list; reload actions withheld | ownership note |

\* a daemon that implements the group RPC. When it does not, the row above
applies — determined by the engine's own answer, not by a version guess.

**One state per condition.** The screen has exactly one `proxyListState`, and the
view switches over it, so two conditions cannot render at once. *read*

**No method is offered that does not exist.** Every proxy entry point goes
through `b.transport()`; when the transport reports the sentinel, the capability
is reported false and the UI stops offering the actions — accepted at the
transport, not at each button. *read, test*

**"Not ready" and "no nodes" stay distinct.** `available=false` with
`supported=true` is "the engine is not up"; `available=true` with an empty list
is "this group has no nodes". They were already separate branches and remain so.
*read, test*

### Known limitation

Whether a daemon implements the group RPC is discovered **on first use**, not
probed up front. A pre-flight capability probe would be a second source of truth
that could disagree with the actual call, and it would cost a round trip on every
panel open. The lazy answer is correct by construction; the cost is that the very
first proxy read on an incompatible daemon returns the capability result rather
than being known in advance. *read*

---

## 3. Proxy / core flow audit

Call flows traced, with the state source and the failure policy for each.

| Flow | Trigger | State source | Failure policy |
|---|---|---|---|
| Home proxy summary | `AppSnapshot.proxy` | snapshot (backend-computed) | none — a missing summary renders `—` |
| Proxies initial load | `ProxiesView .task` → `loadGroups` | `get_proxy_groups` | local (`proxyError`), never global |
| group switch | `selectGroup` → `loadProxies` | `get_proxies` | local |
| node select | `switchProxy` → `withPending` | `switch_proxy` re-reads | global (user-initiated action) |
| single test | `testProxy` | `test_proxy` | global (user-initiated action) |
| test all | `testGroup` | `test_proxy_group` | global (user-initiated action) |
| mode switch | `activateClassicMode`/`DaemonMode` | `set_core_mode` → snapshot | global (blocking, changes the engine) |
| core start | `coreStateChanged` event | event payload | gated on `proxiesSupported` |
| core stop | `coreStateChanged` event | event payload | clears `traffic`; list state recomputes |

The distinction the table encodes: **background/derived** loads report locally,
**user-initiated** commands report globally. A user who clicked "Test All" wants
to know it failed; a screen refreshing itself in the background does not get to
take over Home.

### Bug classes checked

1. **Capability offered but method absent** — the reported case; fixed at the
   transport and reported as a capability. *test*
2. **Request fired without its precondition** — `loadGroups` on core-start is now
   gated on `proxiesSupported`; `loadGroups` already bailed on an empty group
   rather than requesting `""`. *read*
3. **Empty state confused with error state** — `ProxyListState` keeps
   `idle`/`loading`/`coreStopped`/`noGroups`/`configStale`/`empty`/
   `noGroupSelected`/`failed`/`unsupportedByEngine` distinct; each renders its own
   notice. *read*
4. **Summary failure polluting the top-level page** — proxy loads no longer set
   `lastError`; Home shows only blocking conditions. *read*
5. **classic / daemon state sources mixed** — both go through `b.transport()`,
   which returns exactly one transport; there is no per-screen branch on the
   engine. `coreModeLabel` no longer capitalizes a wire value. *read*
6. **Stale error surviving navigation** — `withPending` clears `lastError` on
   entry; `loadGroups` clears `proxyError` when the engine reports supported.
   *read*
7. **One error shown twice** — previously: local notice AND Home banner. Now the
   proxy failure reaches only the Proxies screen. *read*
8. **Stale group / selection** — `loadGroups` re-picks `selectedGroup` whenever
   the remembered name is no longer defined by the config, and
   `proxiesChanged` reloads groups before nodes so a renamed group cannot strand
   the list. *read*

---

## 4. Home error policy

Errors are now split by **what the user can do about them**, which is also what
decides where they appear.

**P0 — blocking, shown on Home:**
core start/stop failure, config unreadable, backend unavailable, core binary
missing or incompatible. These stop the product from working at all, and Home is
where the user is.

**P1 — feature-level, shown in place:** proxy group/node read failure, latency
test failure, subscription refresh failure. These affect one screen and the
screen can explain them alongside the context that makes them actionable.

The mechanism, not a suppression list: `lastError` is the Home banner, and the
proxy load paths no longer write to it. Nothing was hidden — `proxyError` still
carries the backend's message and `ProxiesView` still renders it with a Retry.
Dismissing the banner leaves no gap, because the banner was never the only copy.
*read, test*

---

## 5. Verification

| Property | How |
|---|---|
| unsupported engine ⇒ success response, `supported=false`, reason set | test |
| unsupported engine ⇒ no fabricated nodes, group names still listed | test |
| working transport ⇒ `supported=true`, nodes, selected flag, `delay=-1` for unmeasured | test |
| genuine transport failure ⇒ still `proxy_list_failed` naming the group | test |
| `get_proxy_groups` over IPC, core stopped ⇒ `supported=true, available=false` | IPC |
| the exact reported daemon scenario ⇒ `daemon_no_group_rpc` | test |
| proxy failures cannot reach the Home banner | read + test |
| spacing rhythm monotonic, rows ≥ 30pt | test |
| pages share one section-stack spacing | test |

Reproduce:

```
go test ./backend/service/ -run 'TestProxyList|TestReportedDaemonScenario|TestSwiftSpacing|TestSwiftPages' -count=1
```

### Honest caveats

- **No automated test drives a real daemon.** The daemon is an external binary
  and no service is installed on this machine. The unsupported path is pinned
  with a stub transport that returns exactly the sentinel the real transport
  produces on `Unimplemented`; the mapping from gRPC status to sentinel is
  verified by reading `isUnimplemented`, which is the same helper the
  already-shipped `SetChainPositionEnabled` path uses.
- **The compacted spacing is verified by arithmetic and by guard tests, not by
  eye.** Row height 34pt, group spacing 11pt, header gap 3pt; the guards enforce
  the ordering and the ≥30pt floor. Whether it *looks* right needs a human look
  at the panel.
- **Chinese copy added this round is a proposal**, not verified by a native
  reviewer.
