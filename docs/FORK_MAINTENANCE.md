# Maintaining this fork

This is a private macOS fork of `Leadaxe/singbox-launcher`. It ships as
**JiejieBox** and exists for one user on one Apple Silicon Mac. It is not
intended for upstream contribution.

## Remotes

```text
origin    https://github.com/Piggy-Cat-bit-shadow/singbox-launcher   (your fork)
upstream  https://github.com/Leadaxe/singbox-launcher                (read-only)
```

`upstream` has its **push URL disabled on purpose**:

```bash
git remote -v
# upstream  https://github.com/Leadaxe/singbox-launcher.git (fetch)
# upstream  DISABLED_do_not_push_upstream (push)
```

So a stray `git push upstream` fails instead of publishing to someone else's
repository. Fetching still works and is the only thing `upstream` is for.

No pull requests are opened upstream or in the fork.

## Branches

`main` is the only long-lived branch. Everything else is either already merged
into it or preserved as an `archive/*` tag (see below).

## Syncing with upstream

```bash
git fetch upstream
git checkout main
git merge upstream/main          # merge, not rebase
# resolve fork-specific conflicts (see "Fork-specific changes" below)
go build ./...
bash build/test_darwin.sh nopause
git push origin main
```

The merge is deliberate: `main` here is a *published* branch, so rewriting its
history with rebase/force-push would break clones and the safety tags that point
into it. A merge commit is cheaper than a rewritten history.

## Fork-specific changes to expect conflicts on

These files differ from upstream by design:

| Area | Files | Why |
|---|---|---|
| App identity | `internal/constants/constants.go`, `main.go`, `build/package_macos.sh` | JiejieBox name and `com.piggycat.jiejiebox` so it coexists with the original app |
| Data directory | `internal/constants/constants.go` (`DataDirAppName`) | **Deliberately still `singbox-launcher`**: it already holds the user's config and custom core. Do not rename it without a migration. |
| Privileged start | `internal/platform/privileged_darwin.go` | `-pc` retains effective root; identity-verified kills |
| Process identity | `core/process_detect_darwin.go`, `core/process_service.go` | PID + executable path, never a command-line pattern |
| CI | `.github/workflows/ci.yml`, `golangci-lint.yml` | macOS-only |

## CI

Only macOS is built. On a push to `main` or a pull request, tests run. To make
artifacts:

```bash
gh workflow run ci.yml --ref main -f run_mode=build -f skip_tests=false
```

`run_mode=build` produces artifacts without a tag or release; `prerelease`
additionally creates a pre-release. The build job publishes the **ZIP** created
by `ditto` and runs `build/check_macos_artifact.sh` on it — a bare `.app` does
not survive the Actions artifact round-trip (the bundle seal is dropped), which
is why the archive is the unit of delivery.

## Building locally

```bash
export GITHUB_ACTIONS=true          # skips 'go mod tidy'
./build/package_macos.sh universal  # dist/JiejieBox-*-macos-universal.zip
./build/package_macos.sh arm64      # Apple Silicon only
./build/package_macos.sh catalina   # Intel-only, macOS 10.15+
./build/package_macos.sh universal --install   # also install to /Applications
```

Verify a package before trusting it:

```bash
./build/check_macos_artifact.sh dist/JiejieBox-*-macos-universal.zip universal
```

The sizes to watch (release build, current baseline):

```text
arm64 binary      ~37 MiB      arm64 ZIP      ~16 MiB
x86_64 binary     ~39 MiB      catalina ZIP   ~18 MiB
universal binary  ~76 MiB      universal ZIP  ~33 MiB
```

A jump of more than ~10% means something new was linked in; check with
`go tool nm` and `go list -deps` before accepting it.

## Archive tags instead of dozens of branches

Old work is kept as annotated tags, not branches:

```bash
git tag -l 'archive/*'
git show archive/<name>          # what it contained
```

Nothing unique is lost by deleting a branch this way, and the branch list stays
readable. Create one before deleting anything with unique commits:

```bash
git tag -a archive/<sanitized-branch>-<date> -m "why it is kept" <branch>
git push origin archive/<sanitized-branch>-<date>
```

## Things not to do

- Do not push to `upstream`, and do not open pull requests there.
- Do not force-push `main`.
- Do not delete the Windows/Linux source: this fork only stops *building* them.
- Do not rename `DataDirAppName` without a migration plan.
- Do not commit `config.json`, `state.json`, `cache.db`, subscription files or
  any node list; `.gitignore` covers them, and they contain credentials.

## Known-failing golangci-lint (upstream defect, not this fork)

The fork's `golangci-lint` job is red, and it is red for reasons that come from
upstream, not from anything changed here.

Measured on 2026-09-27 against `main` (`a636bfc`): 13 `errcheck` and 3 `unused`
findings, in these files only —

```text
core/autostart.go               (5 × fmt.Fprint* unchecked)
core/purge.go                   (5 × fmt.Fprint* unchecked)
core/daemon_manager_darwin.go   (1 × f.Close unchecked)
core/daemon_service_state.go    (1 × f.Close unchecked)
internal/paths/copytree.go      (1 × in.Close unchecked)
internal/paths/migrate_test.go  (1 × os.Chmod unchecked)
core/config/subscription/body_read.go          (splitAndTrim unused)
core/config/subscription/xray_protocols.go     (xrayNodeFromOutbound unused)
core/config/subscription/singbox_import_e2e_test.go (entryNodes unused)
```

Every one of those files is byte-identical to upstream — `git diff` against the
upstream baseline is empty for all of them. Upstream's own lint job fails on
its own branches with the same findings (`gh run view 36027458093` shows
`core/purge.go`, `copytree.go`, `migrate_test.go` and `daemon_service_state.go`
failing on Linux, macOS and Windows); the last upstream lint success is on
`develop` at 2026-09-23, before those findings were introduced.

Deliberately **not** fixed here. Rewriting ~16 call sites across eight upstream
files is a large change unrelated to the macOS client work this fork exists
for, and `errcheck` on `fmt.Fprint*` in CLI-style helpers is not a real defect
in a GUI launcher. Touching them would also make every future upstream merge
conflict in files this fork has no reason to own.

If the lint job is wanted green, the honest options are:

1. wait for upstream to fix it, since it is their finding;
2. add a narrow exclusion in `.golangci.yaml` for those specific paths with a
   comment pointing at this section — an explicit, reviewable waiver rather
   than a silent one.

Until then: **`golangci-lint` is NOT a gate for this fork.** `go vet`,
`go test ./...`, `-race`, the l10n and paths guards, and `go mod tidy`
cleanliness all run in CI and all pass.
