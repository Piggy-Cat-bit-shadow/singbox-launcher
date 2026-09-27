# GitHub Actions

This fork builds **one product**: `JiejieBox` for Apple Silicon.

```text
source → quick checks → build once → JiejieBox.app (arm64) → acceptance → one ZIP
```

## Workflows

| File | Triggers | Purpose |
|---|---|---|
| `macos.yml` | push to `main`, PRs, `v*` tags, manual | The product workflow: checks, build, artifact, release |
| `claude.yml` | `issues: [labeled]` only | Issue command bot (`if: label == 'claude'`). Never runs on push, PR or tag |

There is deliberately no separate lint, contract, or platform workflow. The
Windows/Linux/macOS matrix, the Universal and Catalina builds, the DMG step and
a dedicated `Meta` runner were all removed — none of them helped build this
client. The source still supports other platforms; only CI scope changed.

## `macos.yml`

```text
Check  (macos-latest)   ─┐
                         ├─ run in parallel
Build  (macos-latest)   ─┘
                         │
                         └─→ Release (ubuntu-latest), tags and prereleases only
```

`Check` and `Build` run in parallel, so wall-clock time is the slower of the
two rather than their sum. `Build` finishes with a downloadable artifact, so a
push to `main` produces a real client instead of only a test result.

### What each job does

**Check** — `build/test_darwin.sh`, `go vet`, the l10n and paths guards, and a
`go mod tidy` cleanliness check. With `deep_checks=true` it additionally runs
`golangci-lint`, contract-doc regeneration, registry tests and the race
detector. Deep checks are off the normal path on purpose: they cost minutes,
and an ordinary commit mostly needs a working ZIP.

**Build** — computes the version, runs `./build/package_macos.sh arm64` once,
runs `build/check_macos_artifact.sh` as a release-blocking acceptance test, and
uploads **one** artifact `jiejiebox-macos-arm64` containing
`JiejieBox-<version>-macos-arm64.zip` and `checksums.txt`.

**Release** — never compiles anything. It downloads the ZIP the build job
produced, verifies it is a valid archive containing the executable, and
attaches it unchanged. It runs on Ubuntu because the macOS-specific acceptance
(codesign, lipo, otool) already happened in `Build`.

### Triggers

| Event | Result |
|---|---|
| push to `main` | Check + Build → artifact |
| pull request | Check + Build → artifact |
| `v*` tag | Check + Build → artifact + GitHub Release |
| manual, `deep_checks=true` | adds lint, contract docs, registry tests, race |
| manual, `prerelease=true` | Check + Build → pre-release |

`paths-ignore` skips documentation-only pushes to the branch, but **never**
applies to tags: a release tag must always build.

## Conventions

- **The version needs no git history.** On a tag it is the tag name, otherwise
  `dev-<short-sha>`. `CFBundleVersion` is the numeric `GITHUB_RUN_NUMBER`,
  which is what macOS requires (digits and dots) and is monotonic. This is why
  every checkout can use `fetch-depth: 1`.
- **Go cache** is restored by `actions/setup-go`. Neither the workflow nor the
  test script overrides `GOCACHE` in CI — doing so would throw away the
  restored cache and rebuild everything from scratch.
- **One `go build`** of the GUI per run.
- `build/test_darwin.sh` skips its "compile test binaries for inspection"
  phase under `GITHUB_ACTIONS=true`. Those `.test` files were rebuilt after a
  full `go test` run (about 60 s per run), were never uploaded, and nobody
  inspected them. Locally the phase still runs.
- **One artifact.** No test binaries, no DMG, no `BUILD_INFO`, no universal or
  Intel output.
- `upload-artifact` uses `compression-level: 0` because the input is already a
  ZIP.
- **Concurrency**: a new push to the same ref cancels the previous run, so a
  burst of commits does not occupy macOS runners in parallel.

## Local equivalents

```bash
export GITHUB_ACTIONS=true
bash build/test_darwin.sh                  # tests (skips the .test compilation)
./build/package_macos.sh arm64             # dist/JiejieBox-*-macos-arm64.zip
./build/package_macos.sh arm64 --install   # also install to /Applications
./build/package_macos.sh --dmg             # opt-in: also build a .dmg (CI never does)
./build/check_macos_artifact.sh dist/JiejieBox-*-macos-arm64.zip arm64
```
