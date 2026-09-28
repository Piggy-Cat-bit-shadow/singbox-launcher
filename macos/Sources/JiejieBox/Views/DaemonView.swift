// DaemonView — the daemon engine's own setup and diagnostics screen.
//
// Why this screen exists separately from Core Mode: the daemon is not a mode
// value. It needs a launchd service installed, a paired client identity and a
// reachable control plane. Selecting "Daemon" from a radio list made the app
// try to construct a daemon client against a service that did not exist, which
// is what made the click look like a freeze.
//
// So setup and activation are separate actions. Setup never switches engines;
// the switch is offered only once status says ready. Every step that needs
// administrator rights hands the user a command rather than starting a
// long-running privileged operation behind a spinner.

import SwiftUI

struct DaemonView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    var body: some View {
        PanelScaffold(model: model, title: L.daemon.tr(language),
                      onBack: { model.goBack() }) {
            if model.shouldShowBackendDown {
                BackendDownView(model: model)
            } else if let status = model.daemon {
                content(status)
            } else if model.daemonLoading {
                PendingRow(L.checkingDaemonStatus.tr(language))
            } else {
                // Reachable but no status yet: offer a retry rather than an
                // endless spinner.
                MenuSection(L.statusSection.tr(language)) {
                    Text(L.daemonStatusNotLoaded.tr(language))
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                    MenuRow(L.loadStatus.tr(language), systemImage: "arrow.clockwise") {
                        Task { await model.loadDaemonStatus() }
                    }
                }
            }
        }
        .task { await model.loadDaemonStatus() }
    }

    @ViewBuilder
    private func content(_ status: DaemonStatus) -> some View {
        VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
            if !status.supported {
                unavailable
            } else {
                statusSection(status)
                if status.ready {
                    activationSection(status)
                } else {
                    setupSection(status)
                }
                if status.ready || status.installed {
                    settingsSection(status)
                }
                if status.installed || status.paired {
                    if status.active_mode {
                        Text(L.stopVPNFromHome.tr(language))
                            .font(Typography.rowSubtitle)
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, Metrics.rowPaddingH)
                    }
                    dangerSection(status)
                }
            }

            if let cmd = model.daemonCommand {
                commandSection(cmd)
            }
        }
        .padding(.vertical, 8)
    }

    // MARK: - Status

    private func statusSection(_ status: DaemonStatus) -> some View {
        MenuSection(L.statusSection.tr(language)) {
            DetailLine(label: L.service.tr(language), value: status.serviceLabel(language),
                       tone: status.service == "ok" ? .normal : .error)
            DetailLine(label: L.pairing.tr(language),
                       value: status.paired ? L.paired.tr(language) : L.notPaired.tr(language))
            DetailLine(label: L.connection.tr(language),
                       value: status.reachable
                           ? L.connected.tr(language)
                           : L.daemonUnavailable.tr(language))
            if let address = status.address, !address.isEmpty {
                DetailLine(label: L.endpoint.tr(language), value: address)
            }
            if let fp = status.fingerprint, !fp.isEmpty {
                DetailLine(label: L.fingerprint.tr(language), value: fp)
            }
            if let version = status.running_version ?? status.daemon_version, !version.isEmpty {
                DetailLine(label: "Core", value: version)
            }
            if let core = status.core_status, !core.isEmpty {
                DetailLine(label: "VPN", value: core)
            }
            // A daemon that is installed, paired, reachable and running the
            // expected binary can STILL be unable to serve this launcher: the
            // binary comparison above cannot see a protocol mismatch. Without
            // this row the screen would say "Active" while the Proxies screen
            // was disabled, and the user would have no way to connect the two.
            if status.isProtocolStale {
                DetailLine(label: L.daemonProtocolLabel.tr(language),
                           value: L.daemonProtocolStale.tr(language),
                           tone: .error)
            }
            if let error = status.error, !error.isEmpty {
                DetailLine(label: L.errorLabel.tr(language), value: error, tone: .error)
            }

            // Available in EVERY state, including ready and active. Viewing
            // status is not a privilege that activation should gate: a user
            // whose VPN is running through the daemon may still want to check
            // the service, the endpoint or the version.
            MenuRow(L.refreshStatus.tr(language), systemImage: "arrow.clockwise",
                    hoverID: "daemon.status.refresh") {
                Task { await model.loadDaemonStatus() }
            }
            .disabled(model.pending != nil || model.daemonLoading)
        }
    }

    // MARK: - Setup

    @ViewBuilder
    private func setupSection(_ status: DaemonStatus) -> some View {
        MenuSection(L.setupSection.tr(language)) {
            if !status.core_supports_lxd {
                DetailLine(label: "Core", value: "no daemon support", tone: .error)
                Text(L.noLxdLong.tr(language))
                    .font(Typography.rowSubtitle)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, Metrics.rowPaddingH)
                // A diagnosis with no way forward is a dead end, so offer the
                // one action that can actually change this state.
                MenuRow(L.openCoreFolder.tr(language), systemImage: "folder") {
                    if let dir = model.settings?.data_dir, !dir.isEmpty {
                        NSWorkspace.shared.open(URL(fileURLWithPath: dir + "/bin"))
                    }
                }
            } else {
                stepRow(for: status)
            }

            MenuRow(L.refreshStatus.tr(language), systemImage: "arrow.clockwise",
                    hoverID: "daemon.status.refresh") {
                Task { await model.loadDaemonStatus() }
            }
            .disabled(model.pending != nil || model.daemonLoading)

            if model.pending == .configuringDaemon {
                PendingRow(L.preparing.tr(language))
            }
        }
    }

    /// Exactly one next step, so the screen never presents a checklist of
    /// parallel actions and leaves the user guessing which matters now.
    @ViewBuilder
    private func stepRow(for status: DaemonStatus) -> some View {
        switch status.nextStep {
        case .install:
            MenuRow(L.installService.tr(language),
                    subtitle: L.installServiceSubtitle.tr(language),
                    systemImage: "shippingbox") {
                Task { await model.daemonSetup(.install) }
            }
            .disabled(model.pending != nil)
        case .start:
            MenuRow(L.startService.tr(language),
                    subtitle: L.startServiceSubtitle.tr(language),
                    systemImage: "play.circle") {
                Task { await model.daemonSetup(.start) }
            }
            .disabled(model.pending != nil)
        case .pair:
            // Two phases, so a first-time user is never dropped into an empty
            // form. Phase 1 produces the invite command; phase 2 is offered
            // only once that command exists.
            if model.pairingInviteReady {
                MenuRow(L.continueToPair.tr(language),
                        subtitle: L.continueToPairSubtitle.tr(language),
                        systemImage: "arrow.right",
                        showsChevron: true) {
                    model.path.append(.daemonPair)
                }
                .disabled(model.pending != nil)
            } else {
                MenuRow(L.pairService.tr(language),
                        subtitle: L.pairServiceSubtitle.tr(language),
                        systemImage: "link") {
                    Task { await model.prepareDaemonPairing() }
                }
                .disabled(model.pending != nil)
            }
        case .wait:
            DetailLine(label: L.service.tr(language), value: L.notAnsweringYet.tr(language))
            Text(L.notRespondingLong.tr(language))
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.horizontal, Metrics.rowPaddingH)
            MenuRow(L.refreshStatus.tr(language), systemImage: "arrow.clockwise",
                    hoverID: "daemon.command.refresh") {
                Task { await model.loadDaemonStatus() }
            }
            .disabled(model.pending != nil || model.daemonLoading)
        case nil:
            EmptyView()
        }
    }

    // MARK: - Activation

    @ViewBuilder
    private func activationSection(_ status: DaemonStatus) -> some View {
        MenuSection(L.engineSection.tr(language)) {
            if status.active_mode {
                DetailLine(label: L.mode.tr(language), value: L.daemonActiveLabel.tr(language))
                Text(L.daemonSurvivesQuitLong.tr(language))
                    .font(Typography.rowSubtitle)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, Metrics.rowPaddingH)
                MenuRow(L.switchBackToClassic.tr(language), systemImage: "arrow.uturn.backward") {
                    Task { await model.activateClassicMode() }
                }
                .disabled(!model.canSwitchCoreMode)
            } else {
                Button {
                    Task { await model.activateDaemonMode() }
                } label: {
                    Text(L.useDaemonMode.tr(language))
                        .frame(maxWidth: .infinity)
                }
                .controlSize(.large)
                .buttonStyle(.borderedProminent)
                .disabled(!model.canSwitchCoreMode)
                .padding(.horizontal, Metrics.rowPaddingH)
                .padding(.vertical, 6)

                if let reason = model.coreModeBlockedReason(language) {
                    Text(reason)
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }
            }
        }
    }

    // MARK: - Settings

    @ViewBuilder
    private func settingsSection(_ status: DaemonStatus) -> some View {
        MenuSection(L.behaviourSection.tr(language)) {
            // One action, so it stays a MenuRow — but the state is shown as a
            // value rather than a drawn switch. A switch implies its own hit
            // target, and one that cannot be clicked directly reads as broken
            // even when the row itself works.
            MenuRow(L.keepVPNRunning.tr(language),
                    subtitle: status.persists_after_quit
                        ? L.keepRunningOnSubtitle.tr(language)
                        : L.keepRunningOffSubtitle.tr(language),
                    value: status.persists_after_quit ? "On" : "Off",
                    action: {
                        Task { await model.setDaemonKeepRunning(!status.persists_after_quit) }
                    })
            .disabled(model.pending != nil)
            .help(status.persists_after_quit
                  ? L.keepRunningOnHelp.tr(language)
                  : L.keepRunningOffHelp.tr(language))
        }
    }

    // MARK: - Danger

    @ViewBuilder
    private func dangerSection(_ status: DaemonStatus) -> some View {
        MenuSection(L.advancedSection.tr(language)) {
            // Re-pairing is safe while active: it only produces a NEW invite
            // and does not touch the existing pairing until one is redeemed.
            MenuRow(L.rePair.tr(language),
                    subtitle: L.rePairSubtitle.tr(language),
                    systemImage: "arrow.triangle.2.circlepath") {
                Task { await model.daemonSetup(.repair) }
            }
            .disabled(model.pending != nil)

            // Removing the pairing or the service underneath a live daemon VPN
            // tears down the control channel the running core depends on. This
            // is a real constraint from the daemon's design, not caution, so
            // the rows explain it rather than silently refusing.
            if let blocked = destructiveBlockedReason(status) {
                Text(blocked)
                    .font(Typography.rowSubtitle)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, Metrics.rowPaddingH)
                    .padding(.vertical, 2)
            }

            if status.paired {
                MenuRow(L.forgetPairing.tr(language),
                        subtitle: destructiveSubtitle(status, L.forgetPairingSubtitle.tr(language)),
                        systemImage: "link.badge.plus",
                        role: .destructive) {
                    Task { await model.unpairDaemon() }
                }
                .disabled(model.pending != nil || destructiveBlocked(status))
            }

            MenuRow(L.removeService.tr(language),
                    subtitle: destructiveSubtitle(status, "Removes the system service."),
                    systemImage: "trash",
                    role: .destructive) {
                Task { await model.daemonSetup(.uninstall) }
            }
            .disabled(model.pending != nil || destructiveBlocked(status))
        }
    }

    /// True when a destructive daemon action would break a live connection.
    ///
    /// Only while the daemon is BOTH the active engine and actually carrying
    /// traffic: an installed-but-unused service can be removed freely.
    private func destructiveBlocked(_ status: DaemonStatus) -> Bool {
        status.active_mode && model.core?.state == .running
    }

    private func destructiveBlockedReason(_ status: DaemonStatus) -> String? {
        guard destructiveBlocked(status) else { return nil }
        return "Stop the VPN from Home before removing the pairing or the service."
    }

    private func destructiveSubtitle(_ status: DaemonStatus, _ normal: String) -> String {
        destructiveBlocked(status) ? "Stop the VPN first." : normal
    }

    // MARK: - Command output

    /// The command a setup step produced.
    ///
    /// Shown with Copy and Open-in-Terminal rather than run behind a spinner:
    /// these steps need administrator rights, and a privileged operation that
    /// may prompt for a password must not look like an app hang.
    private func commandSection(_ cmd: DaemonCommandResult) -> some View {
        MenuSection("Command") {
            Text(cmd.message)
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.horizontal, Metrics.rowPaddingH)
                .padding(.vertical, 4)

            if cmd.available {
                Text(cmd.command)
                    .font(.system(.caption, design: .monospaced))
                    .textSelection(.enabled)
                    .lineLimit(6)
                    .padding(8)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(RoundedRectangle(cornerRadius: 6).fill(.quaternary.opacity(0.4)))
                    .padding(.horizontal, Metrics.rowPaddingH)

                MenuRow(L.copyCommand.tr(language), systemImage: "doc.on.doc") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(cmd.command, forType: .string)
                    model.setTransientStatus("Command copied.")
                }
                MenuRow(L.openInTerminal.tr(language), systemImage: "terminal") {
                    openInTerminal(cmd.command, model: model)
                }
                // The step that actually completes pairing. Offered right here
                // rather than only on the Setup row, so the sequence reads as
                // one continuous path: run the command, then paste what it
                // prints.
                if cmd.operation == "fresh_invite" {
                    MenuRow(L.continueToPair.tr(language),
                            subtitle: L.continueToPairSubtitle.tr(language),
                            systemImage: "arrow.right",
                            showsChevron: true) {
                        model.path.append(.daemonPair)
                    }
                }
                MenuRow(L.refreshStatus.tr(language), systemImage: "arrow.clockwise",
                        hoverID: "daemon.command.refresh") {
                    Task {
                        // Refresh FIRST, and let the new state decide.
                        //
                        // This deleted the command before reading the status, so
                        // the user's one action for "has this taken effect yet?"
                        // also destroyed the command they were about to run — and
                        // if the read then failed, the screen was left with
                        // neither. The command outlives a refresh; only a real
                        // state change retires it, which `applyDaemonStatus`
                        // decides from what the refresh actually returns.
                        await model.loadDaemonStatus()
                    }
                }
                .disabled(model.daemonLoading)
            } else {
                Text(L.noCommandAvailable.tr(language))
                    .font(Typography.rowSubtitle)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, Metrics.rowPaddingH)
            }
        }
    }

    private var unavailable: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(L.daemonModeUnavailable.tr(language))
                .font(Typography.rowTitle.weight(.medium))
            Text(L.daemonNoEngine.tr(language))
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }
}

/// Opens a shell command in Terminal.
///
/// Terminal is used rather than running the command in-process because these
/// steps require administrator rights: the user must see the password prompt
/// and the command's own output, and the app must not appear to hang while a
/// privileged process runs.
@MainActor
func openInTerminal(_ command: String, model: AppModel) {
    // AppleScript is used so the command runs in the user's own shell session;
    // a `do script` line is the supported way to hand work to Terminal.
    let escaped = command
        .replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"")
    let script = """
    tell application "Terminal"
        activate
        do script "\(escaped)"
    end tell
    """
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
    process.arguments = ["-e", script]
    do {
        try process.run()
        model.setTransientStatus("Command opened in Terminal.")
    } catch {
        model.setTransientStatus("Could not open Terminal. Copy the command instead.")
    }
}
