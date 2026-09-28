# DAEMON PROXY FALLBACK AUDIT

When a local daemon's gRPC surface is missing a proxy-management RPC, the
launcher may serve that action from the **same daemon's own loopback Clash API**.
Nothing else changes: the UI still talks to one `ProxyTransport`, and the
transport decides the wire.

---

## 1. The problem, measured

The daemon installed on the reporting machine
(`1.15.0-jiejie-masquerade.6`, `/Library/PrivilegedHelperTools/sing-box-lxd`)
answers `Unimplemented` to:

```
GetGroups  GetOutbounds  GetChains  GetPool  GetRules
URLTestOutbound  SetChainPositionEnabled  SetEndpointEnabled
```

and implements `GetVersion`, `GetStartedAt`, `SelectOutbound`,
`CloseAllConnections`, the `Subscribe*` streams.

Consequence on that machine: **listing and latency were impossible**, while
switching worked. The proxy screen showed "this engine cannot list proxies" for
an engine that was running the user's core perfectly well.

`SubscribeGroups` is not a workaround: the live daemon answers
`Unknown: invalid argument` to `SubscribeGroups(&emptypb.Empty{})`, so the
request shape differs from the vendored type. Reconstructing a private contract
from a binary would be guessing.

### 1.1 Why the obvious fix would have failed

The daemon-bound config had `experimental.clash_api` **deleted**:

```go
// (2) clash_api — удаляем целиком (daemon работает по gRPC).
if _, ok := exp["clash_api"]; ok {
    delete(exp, "clash_api")
}
```

So `NewClashTransport(APIService.BaseURL)` would have pointed at **nothing**.
The runtime endpoint had to exist before a fallback could use it. That ordering
is the whole reason this change touches config preparation at all.

Verified after the fix (bundled core, real config, inbounds stripped so no TUN
was needed):

```
INFO clash-api: restful api listening at 127.0.0.1:9099
GET /proxies (no auth)          → 401
GET /proxies (Bearer <secret>)  → 🌍 国外流量 type=Selector now=🇺🇸 美国｜NaiveProxy nodes=5
                                  🤖 AI        type=Selector now=🇺🇸 美国｜住宅 AnyTLS nodes=3
GET /proxies/<node>/delay       → {"delay":402}
```

---

## 2. Design

```
                    proxy operation
                          │
                          ▼
                daemonProxyTransport
                          │
            ┌─────────────┴─────────────┐
     RPC implemented            RPC Unimplemented
            │                           │
            ▼                           ▼
       gRPC RPC            local daemon Clash API
                          (verified loopback endpoint)
                                    │
                          unavailable → Unsupported
```

**RPC first, always.** The RPC is the daemon's native control plane: already
paired, already mTLS, needs no extra listener, and does not disturb the
`SelectOutbound` path that already works. The fallback is consulted only when the
probe reports the RPC **absent**.

**Fallback on capability absence only.** A timeout, permission error, bad
request or missing node is an operational failure and is returned as an error.
Rerouting those would hide real problems behind a second code path whose
behaviour the user cannot predict.

**Per action, never "the daemon switches to HTTP".** On the measured machine the
correct answer is a hybrid, and a whole-transport switch would have broken the
working switch path.

### 2.1 Expected transports on the measured machine

| Operation | RPC available | HTTP fallback | Effective transport |
|---|---|---|---|
| List | no (`GetGroups`) | yes | **clash_http** |
| Switch | **yes** (`SelectOutbound`) | yes | **rpc** |
| Latency | no (`URLTestOutbound`) | yes | **clash_http** |

Confirmed by the end-to-end test, which prints exactly
`list=clash_http switch=rpc test=clash_http`.

---

## 3. Config transformation

`prepareConfigForDaemonWith` now keeps `clash_api` and rewrites its host to
loopback, preserving the port and secret. It returns a structured result:

```go
type PreparedDaemonConfig struct {
    Bytes          []byte
    ProxyServer    string
    ClashFallback  DaemonClashFallbackConfig  // Enabled, BaseURL, Token
    SelectorGroups []string
}
```

| Disk config | Daemon runtime copy | Fallback |
|---|---|---|
| `0.0.0.0:9090` | `127.0.0.1:9090` | enabled |
| `[::]:9090` / `:::9090` | `127.0.0.1:9090` | enabled |
| `192.168.1.5:9090` | `127.0.0.1:9090` | enabled |
| `127.0.0.1:9090` | unchanged | enabled |
| no `clash_api` | nothing invented | **disabled** |
| `""` / `127.0.0.1` / `notaport` | section dropped | **disabled** |

Constraints that hold throughout:

* **The disk config is never written.** Only the runtime copy changes; the test
  asserts the on-disk SHA-256 is unchanged.
* **Classic mode is untouched.** The rewrite happens only in the daemon
  preparation path, so Classic keeps whatever `clash_api` the user configured.
* **No new port is invented.** The configured port is reused.
* **`SelectorGroups` comes from the same transform**, so verification evidence
  cannot drift from the bytes actually sent.

---

## 4. Safety rules

The fallback endpoint is adopted only when **all** of these hold:

1. **Same machine.** `isLocalDaemonAddress` accepts `127.0.0.0/8`, `localhost`,
   `::1` (bracketed or bare) via the existing `api.IsLoopbackHost` — one
   canonical loopback rule, exported rather than duplicated.
2. **Loopback listener.** The endpoint is always `127.0.0.1:<port>`.
3. **Derived from the same transformation** that produced the daemon's config.
4. **A real Clash API answers.** A `GET /proxies` that returns a Clash-shaped
   body. A plain HTTP server on the port yields HTML and fails to decode; a
   `200` without a `proxies` object is rejected.
5. **Auth succeeds.** A `401` means something is listening that does not share
   our secret, i.e. probably not our daemon.
6. **Configured groups are present.** The running API must know the selector
   groups from the config we sent.

**A TCP connect is never proof.** Any unrelated process can hold the port, and
adopting it would mean driving a stranger's API.

### 4.1 Remote daemons are refused

A remote daemon (`10.0.0.5:19091`) gets **no** local fallback: `127.0.0.1` on
this machine is the launcher's own host, not the daemon's. Its absent RPCs stay
unsupported. This is enforced by `block()`, not by convention.

### 4.2 What cannot be proven

`GetRunningConfig` is `Unimplemented` on this daemon, so there is **no
fingerprint** linking the HTTP endpoint to the gRPC channel. This audit does not
claim cryptographic proof of a shared process. What is claimed is the
conjunction in §4, which is the strongest statement available — and the reason
every term is mandatory rather than best-effort.

The hybrid's real risk is **two wires observing different state**. That is a
testable invariant, and it is tested (§7).

### 4.3 Secrets

The token is read from the config, kept in Go, sent as a header, and never
crosses IPC. No `clash_token` field was added to any DTO. `redactURL` strips
userinfo and query from anything logged.

---

## 5. Lifecycle

**Commit only after a successful apply.** The fallback configuration is recorded
after `admin.Apply` returns success — never before. A failed apply leaves the
daemon on its previous config (or on last-good after a rollback), so registering
the new endpoint early would aim the fallback at a port nothing is listening on.
There is no optimistic update.

**Readiness is a state, not a bool:**

| State | Meaning | Behaviour |
|---|---|---|
| `not_configured` | no usable `clash_api` | stable; nothing to retry |
| `unverified` | configured, not yet proven | retried on next use |
| `ready` | proven against the running daemon | used |
| `blocked` | remote daemon | stable; never used |

This distinction is why a core that is still starting is not permanently written
off. A connection refused is a **runtime** state, not a capability fact, so it
yields `unverified` and the next attempt retries — while capability absence stays
stable.

**Invalidation** (verification dropped, configuration kept) happens on: core
stop, backend close/reconnect, address change, re-pairing, config apply, engine
mode switch.

### 5.1 Restart recovery

The daemon outlives the GUI, so on relaunch the in-memory state is gone while the
core keeps running. Recovery does **not** require restarting the VPN:

1. transform the current disk config with the same pure function;
2. derive the expected endpoint, token and groups;
3. verify them against the live API;
4. adopt only if §4 holds.

If the disk config has changed since the core started, the running API will not
serve the newly declared groups, verification fails, and the fallback stays
unavailable — a named `config`/`runtime` mismatch rather than a raw `401`. The
launcher never restarts the VPN on its own; the user does that explicitly.

---

## 6. Effective capability

`ProxyActionCapabilities` reports **effective** ability: `RPC OR verified-configured
fallback`. On the measured machine the UI now sees list/switch/test all `true`,
which is the truth — the user can do all three.

RPC absence is still reported (it is a fact about the build), but it is kept
separate from product capability:

```
RPC missing:        GetGroups, URLTestOutbound     ← audit fact
Proxy list:         available via HTTP             ← product state
Latency:            available via HTTP             ← product state
```

Consequences:

* The Daemon screen no longer says "update required" when the fallback covers the
  gap: `coveredByFallback` suppresses the staleness warning, because the user has
  lost nothing and — since the installed daemon is already the newest build — an
  update would fix nothing.
* When the fallback is genuinely unavailable (remote daemon, or no `clash_api`),
  the UI says the service does not offer that ability. It still does **not** say
  "update the daemon", because the launcher has no evidence that a newer build
  exists or would help.

Transport used per action is recorded (`list_transport`, `switch_transport`,
`test_transport` = `rpc` / `clash_http` / `none`) for diagnosis. The UI does not
display it.

---

## 7. Verification

| Property | Test |
|---|---|
| wildcard → loopback, secret+port kept, disk byte-identical | `TestDaemonConfigWildcardBecomesLoopback` |
| loopback input passes through | `TestDaemonConfigLoopbackUnchanged` |
| IPv6 wildcard forms narrowed | `TestDaemonConfigIPv6WildcardBecomesLoopback` |
| LAN address narrowed | `TestDaemonConfigLanAddressBecomesLoopback` |
| missing `clash_api` → no fallback, none invented | `TestDaemonConfigNoClashAPIMeansNoFallback` |
| unusable controller dropped | `TestDaemonConfigUnusableControllerIsDropped` |
| groups derive from the same transform | `TestDaemonConfigReportsSelectorGroups` |
| locality gate | `TestIsLocalDaemonAddress` |
| **remote daemon refused** | `TestRemoteDaemonHasNoLocalFallback`, `TestRemoteDaemonCapabilitiesStayUnsupported` |
| RPC-first (fallback not consulted) | `TestRPCFirstWinsOverFallback` |
| operational failure does not reroute | `TestOperationalFailureDoesNotTriggerFallback` |
| non-Clash endpoint rejected | `TestFallbackVerificationRejectsNonClash` |
| wrong secret rejected, right secret accepted | `TestFallbackVerificationRejectsWrongSecret` |
| missing configured group rejected | `TestFallbackVerificationRejectsMissingGroup` |
| 200-without-`proxies` rejected | `TestFallbackVerificationRequiresRealAPI` |
| transient failure is not permanent | `TestTemporaryUnreachableIsNotPermanent` |
| invalidation keeps configuration | `TestInvalidateKeepsConfiguration` |
| unconfigured never used | `TestUnconfiguredFallbackIsNeverUsed` |
| token not logged | `TestRedactURLHidesCredentials` |
| **hybrid consistency** (gRPC switch observed by HTTP list) | `TestHybridConsistencyThroughOneRuntime` |
| restart recovery | `TestRestartRecoveryRestoresFallback` |
| config/runtime mismatch | `TestConfigChangedWhileDaemonRunsIsMismatch`, `TestMismatchHasItsOwnState` |
| RPC absence ≠ product breakage | `TestRPCAbsenceIsNotProductBreakage` |
| same shape, local vs remote | `TestLocalDaemonSameShapeGetsFallback` |
| no capability claimed before probe | `TestCapabilitiesUnknownBeforeProbe` |

Race detector clean on all of the above.

### 7.1 Manual verification on a real machine

`go run ./tools/daemonfallback` performs the read-only check: it reads the disk
config, derives the runtime endpoint, confirms it is loopback, and probes the
live API for shape, auth and the configured groups. Exit codes distinguish
`not ready (retryable)` from `credentials rejected` from `config mismatch`. It
applies no config and switches no node, so it is safe with a live VPN.

To make the fallback real, Start/Restart the core in JiejieBox so the daemon
receives a config containing `clash_api`; the change is not retroactive, because
a running daemon keeps the config it was last given.

---

## 8. Result on the measured machine

| Check | Result |
|---|---|
| `GetGroups` HTTP fallback | **PASS** (verified against the real config on loopback: groups, `now`, node counts, `delay:402`) |
| `URLTestOutbound` HTTP fallback | **PASS** (delay endpoint returns a real measurement) |
| `SelectOutbound` gRPC | **PASS** (unchanged; still the effective transport for switch) |
| HTTP observes a gRPC switch | **PASS** (`TestHybridConsistencyThroughOneRuntime`) |
| Remote daemon rejection | **PASS** |
| Disk config modified | **NO** (SHA-256 unchanged) |
| Secrets in logs / IPC | **none** |

The live end-to-end run requires the daemon to be given the new config; until the
core is restarted, port 9090 has no listener and the fallback correctly reports
`unverified` (retryable), not `unsupported`.

---

## 9. Honest caveats

* **The daemon-installed path was not exercised end to end.** Restarting the live
  VPN daemon was out of scope for this work, so the final "daemon actually serves
  the new config" step is verified with the same core binary and the same
  transformed config, not through the installed root daemon. The tool in §7.1 is
  what closes that gap on a machine where a restart is acceptable.
* **No fingerprint ties HTTP to gRPC.** §4.2. The consistency test proves the two
  wires agree about the selected node in a simulated runtime; it cannot prove
  process identity on a real one.
* **`SelectOutbound` keeps a fallback branch that the measured daemon never
  takes.** It exists for future builds and is covered only by unit tests.
* **Fallback readiness is not proactively re-probed.** It is verified lazily on
  first use after invalidation, so the very first listing after a restart pays one
  extra HTTP round trip.
* **`urltest` outbounds are treated as groups** in `SelectorGroups` for
  verification purposes; the Clash API exposes them with a different `type`, and
  only presence is checked, so this is harmless but slightly loose.
* **A daemon whose `clash_api` is bound to a non-loopback address on the daemon
  host cannot use the fallback** if that address is genuinely remote. This is
  intentional: the launcher will not control an endpoint it cannot attribute to
  the paired daemon.
* **The unrelated leftover process** on this machine
  (`./dist/sing-box-darwin-arm64 lxd --state-dir /tmp/lxdstate`, holding port
  9091 from an earlier experiment) was deliberately left running and untouched;
  verification used a different port.
