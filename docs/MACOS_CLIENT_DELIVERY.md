# macOS client — delivery, install and rollback

Fork-only delivery. Everything below is produced and kept in
`Piggy-Cat-bit-shadow/singbox-launcher`; nothing is pushed or proposed upstream.

- Repository: `https://github.com/Piggy-Cat-bit-shadow/singbox-launcher`
- Branch: `fix/macos-custom-core-reliability`
- Commit: `cd7000bf261ff105d91ee2905522f8d005c36fe5`
- Baseline the branch is built on: `58f3eb47ed015c984b7d8fff3fd97f5494a793c8` (`v2.3.2`)

The `upstream` git remote was removed, so `git remote -v` shows `origin` only.

## 1. Build artifact

| | |
|---|---|
| Path | `singbox-launcher.app` (repo root of the working copy) |
| Executable | `singbox-launcher.app/Contents/MacOS/singbox-launcher` |
| Size | `39 275 474` bytes |
| Architecture | Mach-O 64-bit executable **arm64** |
| SHA256 | `ea516cf13a852b1b0d631d05b86141df7fc6ab88b98e3024e41f99d83d35c865` |
| Signature | ad-hoc (`Signature=adhoc`, `TeamIdentifier=not set`) — **not notarized** |
| Embedded commit | `cd7000bf…` (the build stamps `RequiredTemplateRef` from `HEAD`) |

Rebuild with:

```bash
cd "<working copy>"
export APP_VERSION="2.3.2-jiejie-macos-reliability"
export GITHUB_ACTIONS=true          # skips 'go mod tidy'
./build/build_darwin.sh arm64
```

The SHA256 **tracks `HEAD`**, because the commit is embedded in the binary
(`RequiredTemplateRef`, verifiable with `strings … | grep "$(git rev-parse HEAD)"`).
Rebuilding after any new commit yields a different hash; that is expected, not a
mismatch. Always record the hash together with the commit it came from.

The `.app` is git-ignored (`*.app/`) and is therefore a build output, not repository
content.

## 2. What the installed launcher reports today

`/Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher -paths` prints:

```text
Mode: system
Program: /Applications/singbox-launcher.app/Contents/MacOS
Data: /Users/jie/Library/Application Support/singbox-launcher
Logs: /Users/jie/Library/Logs/singbox-launcher
Core: /Users/jie/Library/Application Support/singbox-launcher/bin/sing-box (version: (none), source: data)
Shadowed core: /Applications/singbox-launcher.app/Contents/MacOS/bin/sing-box
Template: /Users/jie/Library/Application Support/singbox-launcher/bin/wizard_template.json (source: (none))
```

This is the shadowing described in the audit, confirmed on the live installation:
the running core comes from **Data**, and the copy inside the app bundle is
**shadowed**. Replacing a core only inside the bundle therefore cannot change what
runs — which is exactly why "I updated the core but the version did not change" was
observed. The new UI states this instead of leaving it to be discovered.

## 3. Install steps

Do this only when you choose to. It replaces the launcher executable and re-seals the
bundle; it does **not** touch your data, your core, or your config.

> **The launcher is running right now** (verified: PID 56051 for
> `/Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher`). Quit it before
> replacing its executable — overwriting a running binary is not safe. Your proxy keeps
> working while the launcher is closed only if the core was started independently; if
> the launcher owns the core, expect the tunnel to go down with it. Do this at a time
> you choose.

**Before installing — capture the rollback point.**

```bash
# 3.1 Back up the currently installed launcher executable
cp /Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher \
   ~/singbox-launcher.backup.$(date +%Y%m%d-%H%M%S)
shasum -a 256 ~/singbox-launcher.backup.*    # record this

# 3.2 Record the current core / root copy / config hashes (no secret is printed)
shasum -a 256 \
  "$HOME/Library/Application Support/singbox-launcher/bin/sing-box" \
  "/Applications/singbox-launcher.app/Contents/MacOS/bin/sing-box"
shasum -a 256 "/Library/PrivilegedHelperTools/sing-box-lxd"   # root-readable
shasum -a 256 "$HOME/Library/Application Support/singbox-launcher/config.json"
```

**Quit the launcher first.** Do not overwrite a running executable:

```bash
osascript -e 'tell application "/Applications/singbox-launcher.app" to quit'
# verify nothing is left before continuing
pgrep -fl 'singbox-launcher.app/Contents/MacOS/singbox-launcher' || echo "not running"
```

**Install.** The project's own script replaces only the executable when the app
already exists (it never drags a whole bundle over your data), then ad-hoc re-signs
and re-seals it:

```bash
cd "<working copy>"
export APP_VERSION="2.3.2-jiejie-macos-reliability"
export GITHUB_ACTIONS=true
SB_NO_RESTART=1 ./build/build_darwin.sh -i arm64
```

`SB_NO_RESTART=1` keeps the script from starting the launcher for you; drop it if you
want it started automatically.

**Verify after installing.**

```bash
cd "<working copy>"
shasum -a 256 /Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher
# must equal: ea516cf13a852b1b0d631d05b86141df7fc6ab88b98e3024e41f99d83d35c865
file /Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher   # must say arm64
codesign --verify /Applications/singbox-launcher.app && echo "seal OK"
/Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher -paths
```

## 4. Rollback

Restore only the executable that was replaced. Your data, core, config and the root
copy are untouched by the install, so nothing else needs restoring.

```bash
# 4.1 Quit the launcher
osascript -e 'tell application "/Applications/singbox-launcher.app" to quit'
pgrep -fl 'singbox-launcher.app/Contents/MacOS/singbox-launcher' || echo "not running"

# 4.2 Put the backed-up executable back
cp ~/singbox-launcher.backup.<TIMESTAMP> \
   /Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher
chmod +x /Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher

# 4.3 The bundle seal must match the bundle again (SIGKILL otherwise on modern macOS)
codesign --force --sign - --identifier com.singbox.launcher /Applications/singbox-launcher.app
codesign --verify /Applications/singbox-launcher.app && echo "seal OK"

# 4.4 Confirm the rollback target hash
shasum -a 256 /Applications/singbox-launcher.app/Contents/MacOS/singbox-launcher
# must equal the value recorded in 3.1
```

`./build/build_darwin.sh -i arm64` re-seals the bundle itself, so if you reinstall the
previous build from a checkout instead of restoring the file, step 4.3 is already done
by that script.

**If the privileged copy needs attention.** The launcher never writes it; it prints one
command for you to run. After a core change the Audit/Storage panel says the copy is
"not the current core". For a core that cannot copy itself (yours — `sing-box lxd` is
an unknown command), the command is:

```bash
sudo /bin/mkdir -p '/Library/PrivilegedHelperTools' \
 && sudo /bin/cp -f "$HOME/Library/Application Support/singbox-launcher/bin/sing-box" \
      '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
 && sudo /usr/sbin/chown root:wheel '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
 && sudo /bin/chmod 0755 '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
 && sudo /bin/mv -f '/Library/PrivilegedHelperTools/sing-box-lxd.new' \
      '/Library/PrivilegedHelperTools/sing-box-lxd'
```

Verify afterwards (ownership chain and hash equality are what the gate checks):

```bash
ls -l /Library/PrivilegedHelperTools/sing-box-lxd      # root:wheel, -rwxr-xr-x
shasum -a 256 /Library/PrivilegedHelperTools/sing-box-lxd \
              "$HOME/Library/Application Support/singbox-launcher/bin/sing-box"   # equal
```

The launcher prints this command itself in the core-copy dialog; copy it from there
rather than retyping. Never `chmod 777` the path and never run the GUI as root.

## 5. Uninstall / return to upstream build

Install the upstream `v2.3.2` build over it exactly as in step 3 (the script replaces
the executable only). Nothing in this work writes outside
`~/Library/Application Support/singbox-launcher`, `~/Library/Logs/singbox-launcher`,
`/Library/Logs/sing-box-lxd` and `/Library/PrivilegedHelperTools/sing-box-lxd`, so
there is no separate uninstall step beyond the app itself.

## 6. Verification status

Run in this environment:

- `go build ./...` — passes.
- Named tests — pass:
  - `go test ./api/ -run 'TestLoadClashAPIConfig|TestSetAuthHeader|TestTestAPIConnection|TestGetProxiesInGroup_EmptySecret' -count=1`
  - `go test ./core/ -run 'TestClassifyCoreVersion|TestCoreSupportsServiceCopy|TestPrivilegedCopyCommandFor|TestRootCopyInstallCommand|TestClassifyExitText|TestExitReason|TestDecideCrashAction|TestLastLines|TestDeterministicExitText' -count=1`
  - `go test ./internal/platform/ -run 'TestPrivilegedStartCommand' -count=1`
- The project's own CI runner `bash build/test_darwin.sh nopause` fails **identically
  before and after** the changes, with the same two tests and the same exit code:
  - `TestListProcesses_Smoke` — the sandbox blocks `/bin/ps`
    (`fork/exec /bin/ps: operation not permitted`);
  - `TestCorpusBodiesPassSingboxCheck` — the corpus is checked against the installed
    core, which is built without `with_wireguard`
    (`endpoints[0]: unknown endpoint type: wireguard`).
  Neither is caused by this work; no new failure is introduced.
- Verified against the real custom core: an unrecognized custom version now receives an
  atomic root-copy command instead of "reinstall the official core", and the root-copy
  check reports `ok` with copy and launcher hashes both `501c9b5e…`.

**Not verified, and not claimed:**

- Root TUN creation and live network behaviour. The authorization prompt cannot be
  raised from the build environment (`AuthorizationCreate` returns `-60008`,
  `errAuthorizationInteractionNotAllowed`; `SECURITYSESSIONID` is unset and
  `SecurityAgent` is unreachable in `gui/501`). This requires a run from the GUI and is
  the one remaining item.
- The Clash API end-to-end path against the running core with a secret (the unit tests
  cover the parser and the no-auth request path, not a live authenticated session).
- Upstream CI never ran: the pull request sat in `action_required`, which needs a
  maintainer of the target repository to approve fork workflow runs.
- The Naive IPv6 UDP and TUN-vs-mixed divergence: untouched, still undiagnosed.

## 7. Working agreement

- Development happens only in `Piggy-Cat-bit-shadow/singbox-launcher`.
- No pull requests are created upstream or in the fork unless explicitly requested.
- The fork, branch and all commits are kept.
- Your config, secrets, node addresses and tokens are never committed, printed, or
  placed in a URL, process argument, log, or the repository.
