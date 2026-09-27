# Legacy Assumption Audit — stale guards vs current capability

**Language**: English

A systematic search for code that still believes in an older version of the
product: guards, fallbacks, error texts and adapters written when the downstream
could do less than it can now, so the old layer blocks, misjudges or silently
degrades current behaviour.

The premise: the question is never "is this code old?" but **"is the world this
code believes in still the real world?"** Legacy compatibility is preserved
unless it demonstrably contradicts the current contract (§8, §33, §47).

---

## 1. Numbers

```
Legacy assumptions reviewed:              14
Confirmed stale assumptions:               2
  False rejections:                        1
  Silent degradations:                     0
  State/provenance conflicts:              0
  Headless lifecycle conflicts:            0 (2 fixed in the preceding round)
Protocol/event drift:                      0
Tests updated because contract changed:    0
Remaining confirmed stale assumptions:     0
```

---

## 2. The capability matrix (built before searching)

Pipeline: `HTTP fetch → DecodeSubscriptionContent → ClassifySubscriptionBody →
ParseSubscriptionBody → format parser → materialize → state.Source → build`.

The invariant checked: **no upstream stage may accept less than the parser
downstream**, unless it has an explicit reason.

| Stage | Accepted input | Source of truth |
|---|---|---|
| `fetcher.go` | any HTTP body (size-capped) | — |
| `DecodeSubscriptionContent` | base64-wrapped, or a recognised body, or a URI list | asks the classifier |
| `ClassifySubscriptionBody` | **canonical**: all `BodyKind`s | `body_classify.go` |
| `ParseSubscriptionBody` | every `BodyKind` | `parse_body.go` |

---

## 3. Confirmed stale assumptions

### LA-001 — "A JSON body is a configuration, not a subscription" (P0, fixed previously)

| | |
|---|---|
| **Legacy assumption** | Any body starting with `{` or `[` is a config, not a subscription list → reject |
| **Current contract** | The importer supports sing-box outbound / outbound-array / config / config-array and both Xray forms |
| **Conflict** | Yes — the decoder was strictly narrower than the parser |
| **Affected paths** | `decoder.go` → `parse_body.go` |
| **Fix** | Ask `ClassifySubscriptionBody` instead of using a local prefix heuristic |
| **Regression** | `TestDecodeSubscriptionContentPassesStructuredBodies`, `TestSingboxConfigSubscriptionEndToEnd` |

### LA-002 — "`[` starts a JSON array" (P0, found by this audit)

| | |
|---|---|
| **Legacy assumption** | A body beginning with `[` is JSON — so a wg-quick body starting `[Interface]` is malformed JSON |
| **Current contract** | `ClassifySubscriptionBody` returns `BodyKindWGConf` for such a body, and the parser produces a node from it |
| **Conflict** | Yes. The classifier's own comment records why: *"'[' начинает и JSON-массив, и INI-секцию wg-quick"* |
| **Affected paths** | `decoder.go` |
| **Root cause** | The LA-001 fix consulted the classifier, but *after* its own `looksLikeJSON` prefix test had already decided the body was JSON |
| **Severity** | P0 — a valid WireGuard subscription returned "subscription body looks like JSON but is not valid JSON" instead of nodes |
| **Fix** | Classify **first**; the prefix test now only chooses a better error message for bodies the classifier already rejected, so it can never override a supported format |
| **Regression** | `TestPipelineCapabilityMatrix` (WireGuard `.conf` case) |

This is the same mistake one format over, which is why the permanent test now
covers **every** supported format rather than the one that broke.

**Verified end to end** through the packaged backend: a WireGuard subscription
now refreshes with `nodes=1, status=ok`. While confirming this I first saw
`nodes=0`, which turned out to be my fixture using `aGVsbG8=` ("hello") as a
private key — the pipeline rejected it with
*"The WireGuard key at private_key is not a 32-byte key… no handshake is possible
without a correct key"*. That is the correct outcome, not a further stale
assumption, and it is worth recording because a less precise guard would have
produced a silent zero instead.

---

## 4. Cross-layer format matrix (§43)

Every format driven through decode → classify → parse:

| Format | Classify | Decode | Parse | Entries |
|---|---|---|---|---|
| Base64 URI list | uri-list | ok | ok | ✓ |
| Plain URI list | uri-list | ok | ok | 2 |
| sing-box outbound | singbox-outbound | ok | ok | 1 |
| sing-box config | singbox-config | ok | ok | 1 |
| sing-box outbound array | singbox-outbound-array | ok | ok | 1 |
| sing-box config array | singbox-config-array | ok | ok | ✓ |
| Xray config | xray-config | ok | ok | 1 |
| Xray array | xray-array | ok | ok | 1 |
| WireGuard `.conf` | wgconf | ok | ok | 1 |
| Amnezia `vpn://` | vpn-link | ok | ok | ✓ |
| Unknown JSON | uri-list | **reject** | — | correct |
| Malformed JSON | uri-list | **reject** | — | correct |

The last two rows are the important negative half: widening acceptance did not
make genuinely invalid bodies look supported.

---

## 5. Reviewed and found still correct

Recorded because "checked and clean" is the useful part.

### LA-003 — `ParserConfig` is a Load-time projection (reviewed)

`state.go` documents `ParserConfig` as read-only, built by
`syncLegacyFromCanonical` at Load, and warns that canonical mutation can leave it
stale. `buildSnapshotFromState` reads it, and the core-reject loop mutates
canonical `Sources` before calling that function again — structurally the exact
hazard §10 describes.

**Verified not stale**: `SetCoreRejected` mutates only a node's `Enabled` flag and
reason; `parserCfg.Outbounds` projects `s.Directions`, which no node-level
mutation touches. Confirmed by probe: projection length unchanged (1 → 1) while
the node was disabled.

### LA-004 — `core/backup` mutates canonical state without re-syncing (reviewed)

`backup/import.go` writes `s.Directions` and `s.Rules` and never calls
`syncLegacyFromCanonical`, so its in-memory projection would be stale.

**Not a live bug for this product**: the macOS backend never starts the Debug API
(no `StartDebugAPI` call) and the IPC protocol has no backup or import method, so
that code is linked but unreachable. Recorded rather than "fixed", since changing
it would be an unrequested refactor of code the menu bar cannot reach.

### LA-005 — `IsXrayJSONArrayBody` is dead (reviewed)

Exported, documented as "used for subscription branch", with **no production
callers** — only its own test and two comments explaining why it is not the right
test. Left in place: removing an exported symbol is an API change, and the
classifier comments already document that it answers a different question.

### LA-006 — Manual node import is narrower than the parser (reviewed, correct)

`NodeFromManualConfigJSON` accepts only a single sing-box outbound. That is
intentional: it serves a "paste one node" field, not a subscription body. It is
reached only from state migration hooks, not from user input in this product.

### LA-007 — Binary `running` checks in the UI (reviewed, correct)

Several `state == .running` checks remain, but all are **display** gates (button
label, tint, traffic visibility, whether to reload groups). Every **mutation**
gate uses the shared `canSwitchCoreMode` / `coreOperationBusy` policy. The
"`!= running` is too permissive" defect was fixed in an earlier round and has not
regressed.

### LA-008 — `isProviderBannerLine` uses `://` (reviewed, correct)

Deliberately narrow and documented: a line without a scheme separator never
pretended to be a node. Not a format claim.

### Remaining assumptions reviewed and unchanged

| ID | Assumption | Verdict |
|---|---|---|
| LA-009 | state v2–v7 migrations | Real backward compatibility — preserved |
| LA-010 | legacy settings / config import | Real backward compatibility — preserved |
| LA-011 | `Capabilities` values | Accurate: `Remote`/`Configurator` false, real ones true |
| LA-012 | IPC method set | 31 declared, all dispatched, all called (one heuristic false positive) |
| LA-013 | Event declare/emit/handle | 8 events, all three-way consistent |
| LA-014 | Content-Type is not used to choose a parser | Correct — the body decides |

---

## 6. Tests preserved rather than rewritten

No test was changed to match new behaviour. The historical expectation that
"JSON configuration is not a subscription" was **never** encoded as a test
assertion — it lived only in the decoder, which is why the bug survived a green
suite. The new coverage is additive:

- `decoder_test.go` — six structured pass-through cases, unknown/malformed
  rejection, URI-list regression
- `decoder_singbox_e2e_test.go` — fetch → decode → parse over a full sing-box
  config, asserting nodes materialise while `direct`/`block`/`dns` never become
  nodes and selector/urltest keep group semantics
- `pipeline_matrix_test.go` — the cross-layer invariant for **every** supported
  format, plus the negative cases

---

## 7. Integration coverage (§44)

Unit parser tests were already green while the bug was live — that is precisely
how it survived. The end-to-end tests therefore drive the real chain:

```
httptest server → FetchSubscriptionWithMeta → DecodeSubscriptionContent
  → ClassifySubscriptionBody → ParseSubscriptionBody → entries
```

Verified against a real HTTP server through the packaged backend:
`refresh_subscription` → `node_count=4, last_status=ok`, and
`update_subscriptions` → `ok=true, sources=1/1, nodes=4` — the parity §28
requires, guaranteed because both call `FetchSubscriptionWithMetaFor`, which is
where the decoder sits.

---

## 8. Remaining

**No confirmed stale assumptions remain.** Two reviewed items are recorded as
dormant rather than fixed (LA-004, LA-005), both in code the menu bar cannot
reach; fixing either would be an unrequested refactor.
