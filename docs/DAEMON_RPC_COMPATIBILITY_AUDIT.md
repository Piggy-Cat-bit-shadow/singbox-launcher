# DAEMON RPC COMPATIBILITY AUDIT

Client/server capability drift in daemon mode: the launcher's generated gRPC
client and the daemon that actually answers it do not agree on which
`daemon.StartedService` methods exist.

This audit was triggered by one visible failure and found the drift to be eight
methods wide.

---

## 1. The reported failure

```
cannot read the proxies of group "🌍 国外流量": daemon GetGroups:
rpc error: code = Unimplemented desc = unknown method GetGroups for service daemon.StartedService
```

Shown both on the Proxies screen and, through the global error banner, on Home.

## 2. Root cause: measured, not assumed

`GetGroups` **is** declared in `internal/daemonpb`. The generated client has had
it since the daemon engine landed, and `internal/daemonpb/SYNC_REV` pins the fork
commit the stubs were synced from. So the method's presence in the client proves
nothing about the server — which is the entire bug.

The reachable daemon was probed directly, with the launcher's own paired mTLS
identity, on port 19091:

| Fact | Value |
|---|---|
| Service definition | `/Library/LaunchDaemons/com.leadaxe.sing-box-lxd.plist` |
| Binary | `/Library/PrivilegedHelperTools/sing-box-lxd` |
| Version | `1.15.0-jiejie-masquerade.6` |
| Build tags | `…,with_lxd,jiejie_client_macos,…` |
| sha256 | `ee875290758ac126b58f80e50ddedaca232c7c3c30c2d8b5221cda8ca31b1c13` |
| Listener | `127.0.0.1:19091` (open, mTLS) |

### 2.1 The drift is eight methods wide

Every result below is a live gRPC status code from that daemon, not an inference:

| RPC | launcher calls it | live daemon |
|---|---|---|
| `GetGroups` | 3 sites | **`Unimplemented`** |
| `GetOutbounds` | 5 sites | **`Unimplemented`** |
| `GetChains` | 10 sites | **`Unimplemented`** |
| `GetPool` | 2 sites | **`Unimplemented`** |
| `GetRules` | 3 sites | **`Unimplemented`** |
| `URLTestOutbound` | 5 sites | **`Unimplemented`** |
| `SetChainPositionEnabled` | 2 sites | **`Unimplemented`** |
| `SetEndpointEnabled` | 1 site | **`Unimplemented`** |
| `SelectOutbound` | 2 sites | supported |
| `GetVersion` | — | supported |
| `GetStartedAt` | 2 sites | supported |
| `CloseAllConnections` | 2 sites | supported |
| `SubscribeStatus` | 1 site | supported |
| `SubscribeGroups` / `SubscribeOutbounds` / `SubscribeConnections` | 1 site | supported |

The same conclusion is visible in the binary itself: `GetGroups`, `GetPool`,
`GetChains`, `SetChainPositionEnabled`, `SetEndpointEnabled` and
`SubscribeDNSQueries` appear **zero** times in its string table, while
`SubscribeGroups` appears 18 times.

**The daemon does not have the unary lx command surface at all.** It has the
subscription plane and outbound selection; every unary read is absent.

### 2.2 Why this is not a stale daemon

The instinct is "the user has an old daemon". The evidence says otherwise:

- The installed binary is the newest one on the machine — nothing older exists
  to compare against.
- Its version is a **custom fork build** (`-jiejie-masquerade.6`), so its series
  is not comparable with the `lx.N` series the vendored proto was synced from.
- Its build tags **include `with_lxd`**, so it is not the
  "core built without the lx command surface" case that `isUnimplemented`'s
  comment describes.

The honest description is not "old daemon" but **client ahead of server**: the
launcher was generated from a proto the reachable daemon was not built from. A
version comparison could not have detected this, which is why none was added.

### 2.3 Why stale detection never fired

`core/daemon_service_state.go` classifies the service as `not_installed`,
`unsafe`, `stale`, `not_running`, `process_stale` or `ok`. Every one of those
verdicts comes from **binaries, hashes, ownership and paths** — "is the installed
service the copy we expect?". None of them can see "the copy is right but its
protocol is wrong", and `grep` confirms the classifier contains no reference to
proto, RPC, capability or `Unimplemented`.

So the machine reported a healthy, active, correctly-installed daemon while the
Proxies screen could not load a single node. Both statements were true; the
status simply had no vocabulary for the second.

### 2.4 The four defects that turned drift into a user-visible failure

The missing RPC is a legitimate state. What made it a bug:

1. **`daemonProxyTransport.GroupProxies`** wrapped `Unimplemented` in a generic
   `fmt.Errorf`, discarding the distinction the gRPC status carries.
2. **`Backend.Proxies`** turned any transport error into `proxy_list_failed`, a
   *recoverable* error, implying a retry could help when none ever would.
3. **`AppModel`** copied it into `lastError`, which `HomeView` renders as a
   global banner — so one screen's engine limit displaced the core and config
   status the user actually needed.
4. **`loadGroups`** fired on every core start, so the failure reappeared each
   time and read as an app defect.

Fixed in the previous change (`e067770f`). This audit addresses what that change
did **not**: the other seven drifting methods, the absence of any capability
mechanism, and the missing protocol vocabulary in the status.

---

## 3. What was built

### 3.1 Capability truth comes from the server

`core/daemon_rpc_compat.go` probes the live daemon once per connection by
**calling** each method and reading the status code:

```
codes.Unimplemented  → the server does not have the method
anything else        → the method exists (the call reached real argument
                       validation, which is proof of implementation)
```

The result is cached on `DaemonBackend.caps`. Before a probe runs, every
capability reads **false** — an unknown capability must never be assumed present,
because that assumption is exactly what produced this bug.

**Version strings are deliberately not parsed.** A custom build carries no
comparable series, and the fork's own version had already moved past the proto it
was generated from — a version→capability table would have been wrong about the
one machine this audit exists for. The probe asks the server directly, which is
also why `GetVersion` is recorded for display and the audit trail but never used
to infer a capability.

### 3.2 Capability gates on every user-reachable call

Each method on the reachable daemon surface is now gated in the compatibility
layer, so the upper layers never see a raw gRPC string:

| Method | Gate |
|---|---|
| `GroupProxies` | `GetGroups` |
| `SwitchProxy` | `SelectOutbound` |
| `Delay` | `URLTestOutbound` |
| `EndpointStatuses` | `GetOutbounds` |
| `SetEndpointEnabled` | `SetEndpointEnabled` |
| `DaemonBackend.PoolSlots` | `GetPool` |
| `DaemonBackend.Chains` | `GetChains` |
| `DaemonBackend.ProbeLayer` | `URLTestOutbound` |

All of them return `services.ErrProxyListUnsupported` — a capability, not an
error. The awareness stays in the daemon layer: `backend/service/proxies.go` and
the Swift UI still see only `GroupProxies()`, and neither knows `GetGroups` from
`SubscribeGroups`.

### 3.3 Fallback: evaluated, measured, and rejected

The audit asked whether `GetGroups` can fall back to `SubscribeGroups`. It was
tested against the live daemon rather than reasoned about, and **it cannot**:

```
SubscribeGroups(&emptypb.Empty{}) → rpc error: code = Unknown desc = invalid argument
```

The two do not take the same request, so the vendored subscription request type
is not what this server expects. Even where a subscription existed, its first
frame is a stream's opening state while `ProxyTransport` is a request/response
interface; silently reinterpreting one as the other would invent a contract
neither side agreed to. Reconstructing the server's expected request from a
binary would be guessing at a private contract.

**So no fallback was added, and the reason is recorded in the code** next to the
call. An engine without `GetGroups` is reported as unable to list proxies, and
the UI explains that. This is the honest outcome, and it is better than a
fallback that would have looked plausible and failed on contact with the real
server.

### 3.4 Protocol staleness is a named state

`DaemonStatusDTO` gained `protocol_stale` and `missing_rpcs`, kept **separate**
from `service`. Collapsing them would lose which repair applies: reinstalling the
service fixes a binary mismatch, while a protocol mismatch needs a daemon built
from a newer proto.

The Daemon screen shows a `Compatibility: Needs a newer daemon` row, so the
screen no longer says "Active" while Proxies is disabled with no visible
connection between the two. Only methods the shipped product actually calls over
IPC are reported — chains and the pool have no IPC method, so naming them would
ask for an update that changes nothing the user can see.

**No automatic update.** The launcher detects and reports; the user repairs from
the Daemon screen. Nothing replaces the root-owned service on its own.

### 3.5 No raw capability text reaches the UI

`core/daemon_rpc_leak_guard_test.go` scans every Go call site of the eight
drifting methods and fails unless the enclosing function consults the capability
set or recognises `Unimplemented`. It found **16 unguarded call sites** when
first written.

Exemptions are explicit and each states its reason. `daemon_rpc_compat.go` must
call the methods — that is the mechanism. The remote-machine transport and the
debug API are exempt because they are **not reachable from the shipped product**
(`capabilities.Remote` is `false`, the menu-bar backend never starts the debug
server); they are left alone deliberately rather than fixed here, so this audit
does not grow into a remote-machine refactor.

---

## 4. RPC compatibility matrix

| RPC | Purpose | Live server | Introduced | Fallback | Required? |
|---|---|---|---|---|---|
| `GetVersion` | handshake / display | yes | base | — | required |
| `GetStartedAt` | uptime | yes | base | — | optional |
| `SelectOutbound` | switch node | yes | base | — | required for switching |
| `CloseAllConnections` | connections UI | yes | base | — | optional |
| `SubscribeStatus` | status stream | yes | base | — | required |
| `SubscribeGroups` | groups stream | yes | base | — | unused by IPC |
| `SubscribeOutbounds` | outbounds stream | yes | base | — | unused by IPC |
| `SubscribeConnections` | connections stream | yes | base | — | optional |
| `GetGroups` | list proxies | **no** | not in this server | **none — measured impossible** | required for listing |
| `GetOutbounds` | endpoint state | **no** | not in this server | none | optional |
| `GetChains` | chain diagnostics | **no** | not in this server | none | optional (no IPC) |
| `GetPool` | pool slots | **no** | not in this server | none | optional (no IPC) |
| `GetRules` | rules view | **no** | not in this server | none | optional (no IPC) |
| `URLTestOutbound` | latency | **no** | not in this server | none | optional |
| `SetChainPositionEnabled` | chain toggle | **no** | not in this server | none | optional |
| `SetEndpointEnabled` | endpoint toggle | **no** | not in this server | none | optional |

"Not in this server" is the measured fact for the daemon in §2.1. It is a
property of that binary, not a claim about every daemon.

---

## 5. Verification

### 5.1 Against the real daemon

The capability probe was run against the live daemon with the launcher's paired
mTLS identity, and it reported exactly the matrix in §2.1:

```
daemon RPC compatibility: the reachable daemon "1.15.0-jiejie-masquerade.6"
does not implement: [GetGroups GetOutbounds GetChains GetPool GetRules
URLTestOutbound SetChainPositionEnabled SetEndpointEnabled]
```

and `GroupProxies` returned the capability sentinel
(`the active engine does not support listing proxies`) rather than the raw
`unknown method GetGroups` text.

### 5.2 Fake daemon servers, three generations

`core/daemon_rpc_fake_server_test.go` runs the **real** probe against three gRPC
servers over `bufconn`, each defined by what it implements:

| Generation | Server | Expected |
|---|---|---|
| A | current — all methods | every capability true |
| B | legacy — `SelectOutbound` + subscriptions, no unary reads | `GetGroups` false, `SelectOutbound` true |
| C | bare — handshake only | every proxy capability false |

`bufconn` keeps these off the network, which matters because the reporting
machine has a real daemon listening.

Also covered: absent-vs-rejected discrimination (a method that exists but rejects
the probe reads as supported; a method that does not exist reads as unsupported),
probe caching, and that a capable daemon is **not** reported stale.

### 5.3 These tests catch the original defect

The tests were mutation-checked. Reverting the probe to the pre-fix assumption
("the client interface says it exists, so it exists") makes them fail with:

```
GetGroups probed as supported against a server answering Unimplemented
    — this is the original defect
```

A regression test that cannot fail when the fix is removed proves nothing; this
one fails on the precise defect.

### 5.4 Guards

`go build ./...`, `gofmt`, the leak guard, and `l10n_check --strict`
(keys used 88, catalog 89, missing+orphan warns 0, hard fails 0) all pass. The
Swift app builds against the new `protocol_stale` field.

---

## 6. Summary

```
RPCs audited:                    16
Version-sensitive RPCs:           0  (0 parsed; capability is probed, not inferred)
Fallbacks added:                  0  (1 evaluated and measured impossible)
Capability gates added:           8
Raw Unimplemented leaks remaining: 0
```

## 7. Honest caveats

- **The eight-method finding is about one binary.** It is the daemon on the
  reporting machine, measured directly. Other installations may drift
  differently; the probe adapts per connection, which is why capability is
  discovered at runtime rather than from a table.
- **`SubscribeGroups` was not reverse-engineered.** Its request shape is a
  private contract; the fallback is absent because it was measured not to work
  with the vendored request, not because a fallback is impossible in principle.
- **No automatic end-to-end test drives a real daemon in CI.** The fake servers
  speak the vendored protobuf and return the real `Unimplemented` message, and
  the probe was verified against the live daemon by hand; but CI has no daemon,
  so the probe's behaviour against a third-party server rests on that manual run.
- **The remote-machine and debug-API call sites are knowingly unguarded.** They
  are unreachable from the shipped product; leaving them is a scoping decision,
  recorded in the guard's exemption list.
- **The protocol verdict is discovered on first use**, not pre-flighted at
  connect time — a probe on every connect would cost a round trip for users who
  never open Proxies. The Daemon screen therefore reports the condition once
  something has asked, which is why it can lag the first paint.
- **The new Chinese copy was not reviewed by a native speaker.**
- **No manual GUI walkthrough was performed**; the spacing and layout claims rest
  on the build plus guard tests, not on visual inspection.
