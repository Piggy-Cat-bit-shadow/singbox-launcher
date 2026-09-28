# Config reference integrity — root cause analysis

**Language**: English
**Incident**: `FATAL: default outbound not found: proxy-out` on real hardware.
**Baseline**: `0afd096c1cda04b5689a993e9d449846dd605c4e`
**Failing Action**: [run 36453455061](https://github.com/Piggy-Cat-bit-shadow/singbox-launcher/actions/runs/36453455061)

This file records what was actually measured, including the parts that **contradict
the initial hypothesis**. Every claim below was produced by running the real
pipeline, not by reading it.

---

## 1. Summary

The reported hypothesis was that outbound generation silently deletes the required
`proxy-out` selector, and that reference repair fixes a temporary object which a
later rebuild overwrites.

**Neither is what happens.** Measured:

| Hypothesis | Measured result |
|---|---|
| Generation deletes the required selector | **False.** `proxy-out` is generated whenever at least one node exists, and it always contains `direct-out`. |
| Repair mutates a temporary object | **False.** `RepairRouteFinal` mutates the decoded config object, and `finalizeReferences` **re-emits the bytes from that repaired object**, so the emitted file matches what was validated. |
| A second rebuild restores the old value | **False on the build path.** `rebuildRound` re-runs the *whole* `BuildConfig`, repair included. |
| Validation is missing | **False.** `ValidateConfigReferences` is comprehensive and runs on every activation path. |

The real defect is narrower and worse than "a missing check":

> **The builder repairs the config to a *valid* one that silently sends all traffic
> direct, and `route.final` is the only place it may do so.** With an empty parser
> cache, `proxy-out` is absent (correctly — there are no nodes), so
> `RepairRouteFinal` rewrites `route.final` from `proxy-out` to `direct-out` and
> reports only a warning. The config starts. The tunnel does not carry traffic. The
> `required: true` declaration in `parser_config` is **never consulted**.

And separately, the delivery path has no gate at all:

> **`config.json` is read from disk and handed to the daemon with no reference
> validation whatsoever.**

## 2. What was measured

### 2.1 The required selector is generated correctly

Real `bin/wizard_template.json`, one enabled source with one node:

```
OUTBOUND tag="node-a"          type="shadowsocks"
OUTBOUND tag="proxy-out-auto"  type="urltest"   members=[node-a]
OUTBOUND tag="proxy-out"       type="selector"  members=[proxy-out-auto direct-out node-a]
OUTBOUND tag="vpn ①"           type="selector"  members=[direct-out proxy-out node-a]
OUTBOUND tag="vpn ②"           type="selector"  members=[direct-out proxy-out node-a]
```

`proxy-out` survives, and `direct-out` is in it — exactly as the `addOutbounds`
declaration promises. **The generator is not the bug.**

### 2.2 Zero nodes is a hard error, not a dangling config

With every source empty or every node disabled:

```
GenerateOutboundsFromParserConfig: no nodes parsed from any source
```

Generation **fails**, so no config is produced. Also not the bug.

### 2.3 The actual mechanism: repair redirects traffic to `direct-out`

Real template, **empty** parser cache (the state after a fresh install, or after the
last node was filtered away):

```
[WARN] build: reference repair: route.final "proxy-out" → "direct-out"
       (the original outbound no longer exists)
route.final="direct-out"
outbounds: map[block-out:true direct-out:true]
```

The same happens with a non-empty cache that lacks `proxy-out`:

```
route.final="direct-out"  tags=map[block-out:true direct-out:true lonely-node:true]
```

So the emitted config is reference-**clean** — the invariant holds — while being
semantically wrong in the one way that is hardest to notice: **the VPN silently
becomes a no-op.** A warning in a log is the only trace.

### 2.4 The delivery path has no gate

`DaemonBackend.applyOnce` reads the config and delivers it:

```
os.ReadFile(ac.FileService.ConfigPath)
    → prepareDaemonConfig(...)      // cache_file path + clash_api host ONLY
    → b.admin.ApplyCtx(ctx, config) // starts a core
```

`ValidateConfigReferences` / `ValidateConfigBytes` appear **only** in
`core/build`. Nothing on this path validates anything. Whatever is on disk is
delivered, whatever it says.

### 2.5 The pre-start rebuild can decline to run

`rebuildConfigBeforeStart` returns `nil` — silently — in two cases:

- `!mayRebuildConfig()`: "config.json is not managed by JieJieBox";
- `state.ErrNotFound`: "no state.json".

Both then use **the file already on disk, unvalidated**. This is the route by which
a config the builder would have accepted-or-repaired reaches the core without ever
passing through the builder.

## 3. Why `proxy-out` can be gone while `route.final` still names it

The sequence that produces the reported FATAL:

1. A config is written while nodes existed, so `proxy-out` is present and
   `route.final = "proxy-out"`.
2. The node set later becomes empty (subscription removed, all nodes disabled, a
   filter change, an import, a preset switch).
3. On the next build the cache yields no `proxy-out`, so `RepairRouteFinal`
   rewrites `route.final` → `direct-out`. **This is the silent-traffic-redirect
   case**, and it is what the code does today.
4. **But if the rebuild is skipped** (§2.5) or the file is otherwise not rebuilt,
   the old bytes stay on disk: `route.final = "proxy-out"` with no `proxy-out`
   outbound.
5. The daemon delivers those bytes (§2.4). The core refuses to start:
   `default outbound not found: proxy-out`.

Steps 3 and 5 are the two independent defects: **the wrong-but-valid repair**, and
**the absence of any gate**.

## 4. Source of truth

There is exactly **one** canonical input — persisted wizard state + `state.json` —
and one generated artefact — `config.json`. That part of the design is sound.

The problem is not multiple sources of truth. It is that:

- `config.json` **on disk is treated as authoritative at launch**, and it can
  predate the state it was built from;
- `parser_config.outbounds[].required` is **declared and validated by nobody**.

So the missing piece is a **gate**, not a reconciliation of two models.

## 5. Why `required` does nothing today

`configtypes.ParserConfig` parses `required` correctly (`Required bool`), and
`NormalizeParserConfig` preserves it — but **no code reads it during generation or
validation**. Search for `.Required` finds only the registry and linkmap, which are
about *node field* requirements, not outbound declarations.

A `required: true` selector is therefore exactly as disposable as a decorative one.

## 6. What the fix must do

1. A required selector must survive an empty node set (keep `proxy-out` with
   `direct-out`), or the build must fail with a named error. It must never be
   repaired away to `direct-out`.
2. Repair must never choose a target that silently changes traffic semantics
   (`direct-out`) when a `required` group is declared.
3. A single reusable validator must be callable at **every** point a config could
   reach the core — including the delivery path and the skipped-rebuild path.
4. The delivery path must run that validator, and must fail with the offending path,
   the missing tag, and the list of available tags.
5. A deterministic configuration failure must not enter the restart loop.
6. CI must prove a generated config is accepted by a real `sing-box check`.
