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

    var body: some View {
        PanelScaffold(model: model, title: "Daemon", onBack: { model.goBack() }) {
            if model.shouldShowBackendDown {
                BackendDownView(model: model, subject: "the daemon status")
            } else if let status = model.daemon {
                content(status)
            } else if model.daemonLoading {
                PendingRow("Checking daemon status…")
            } else {
                // Reachable but no status yet: offer a retry rather than an
                // endless spinner.
                MenuSection("Status") {
                    Text("Daemon status has not loaded yet.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                    MenuRow("Load Status", systemImage: "arrow.clockwise") {
                        Task { await model.loadDaemonStatus() }
                    }
                }
            }
        }
        .task { await model.loadDaemonStatus() }
    }

    @ViewBuilder
    private func content(_ status: DaemonStatus) -> some View {
        VStack(alignment: .leading, spacing: 10) {
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
                        Text("Stop the VPN from Home before changing the engine.")
                            .font(.caption)
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
        MenuSection("Status") {
            DetailLine(label: "Service", value: status.serviceLabel,
                       tone: status.service == "ok" ? .normal : .error)
            DetailLine(label: "Pairing", value: status.paired ? "Paired" : "Not paired")
            DetailLine(label: "Connection", value: status.reachable ? "Connected" : "Unavailable")
            if let address = status.address, !address.isEmpty {
                DetailLine(label: "Endpoint", value: address)
            }
            if let fp = status.fingerprint, !fp.isEmpty {
                DetailLine(label: "Fingerprint", value: fp)
            }
            if let version = status.running_version ?? status.daemon_version, !version.isEmpty {
                DetailLine(label: "Core", value: version)
            }
            if let core = status.core_status, !core.isEmpty {
                DetailLine(label: "VPN", value: core)
            }
            if let error = status.error, !error.isEmpty {
                DetailLine(label: "Error", value: error, tone: .error)
            }
        }
    }

    // MARK: - Setup

    @ViewBuilder
    private func setupSection(_ status: DaemonStatus) -> some View {
        MenuSection("Setup") {
            if !status.core_supports_lxd {
                DetailLine(label: "Core", value: "no daemon support", tone: .error)
                Text("The installed core has no `lxd` subcommand, so it cannot run as a service. "
                     + "Install a core that supports it, then refresh.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, Metrics.rowPaddingH)
                // A diagnosis with no way forward is a dead end, so offer the
                // one action that can actually change this state.
                MenuRow("Open Core Folder", systemImage: "folder") {
                    if let dir = model.settings?.data_dir, !dir.isEmpty {
                        NSWorkspace.shared.open(URL(fileURLWithPath: dir + "/bin"))
                    }
                }
            } else {
                stepRow(for: status)
            }

            MenuRow("Refresh Status", systemImage: "arrow.clockwise") {
                Task { await model.loadDaemonStatus() }
            }
            .disabled(model.pending != nil)

            if model.pending == .configuringDaemon {
                PendingRow("Preparing…")
            }
        }
    }

    /// Exactly one next step, so the screen never presents a checklist of
    /// parallel actions and leaves the user guessing which matters now.
    @ViewBuilder
    private func stepRow(for status: DaemonStatus) -> some View {
        switch status.nextStep {
        case .install:
            MenuRow("Install Service",
                    subtitle: "Creates the system service. Needs administrator rights.",
                    systemImage: "shippingbox") {
                Task { await model.daemonSetup(.install) }
            }
            .disabled(model.pending != nil)
        case .start:
            MenuRow("Start Service",
                    subtitle: "The service is installed but not running.",
                    systemImage: "play.circle") {
                Task { await model.daemonSetup(.start) }
            }
            .disabled(model.pending != nil)
        case .pair:
            MenuRow("Pair Service",
                    subtitle: "Pairs this app with the running service.",
                    systemImage: "link") {
                model.path.append(.daemonPair)
            }
            .disabled(model.pending != nil)
        case .wait:
            DetailLine(label: "Service", value: "Not answering yet")
            Text("The service is installed and paired but not responding. "
                 + "If you just installed it, give it a moment and refresh.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.horizontal, Metrics.rowPaddingH)
            MenuRow("Refresh Status", systemImage: "arrow.clockwise") {
                Task { await model.loadDaemonStatus() }
            }
        case nil:
            EmptyView()
        }
    }

    // MARK: - Activation

    @ViewBuilder
    private func activationSection(_ status: DaemonStatus) -> some View {
        MenuSection("Engine") {
            if status.active_mode {
                DetailLine(label: "Mode", value: "Daemon active")
                Text("The VPN runs inside the system service and survives quitting the app.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, Metrics.rowPaddingH)
                MenuRow("Switch Back to Classic", systemImage: "arrow.uturn.backward") {
                    Task { await model.activateClassicMode() }
                }
                .disabled(!model.canSwitchCoreMode)
            } else {
                Button {
                    Task { await model.activateDaemonMode() }
                } label: {
                    Text("Use Daemon Mode")
                        .frame(maxWidth: .infinity)
                }
                .controlSize(.large)
                .buttonStyle(.borderedProminent)
                .disabled(!model.canSwitchCoreMode)
                .padding(.horizontal, Metrics.rowPaddingH)
                .padding(.vertical, 6)

                if let reason = model.coreModeBlockedReason {
                    Text(reason)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }
            }
        }
    }

    // MARK: - Settings

    @ViewBuilder
    private func settingsSection(_ status: DaemonStatus) -> some View {
        MenuSection("Behaviour") {
            // One action, so it stays a MenuRow — but the state is shown as a
            // value rather than a drawn switch. A switch implies its own hit
            // target, and one that cannot be clicked directly reads as broken
            // even when the row itself works.
            MenuRow("Keep VPN Running After Quit",
                    subtitle: status.persists_after_quit
                        ? "The VPN stays connected when JiejieBox quits."
                        : "The VPN stops when JiejieBox quits.",
                    value: status.persists_after_quit ? "On" : "Off",
                    action: {
                        Task { await model.setDaemonKeepRunning(!status.persists_after_quit) }
                    })
            .disabled(model.pending != nil)
            .help(status.persists_after_quit
                  ? "Click to make the VPN stop when JiejieBox quits."
                  : "Click to keep the VPN running after JiejieBox quits.")
        }
    }

    // MARK: - Danger

    @ViewBuilder
    private func dangerSection(_ status: DaemonStatus) -> some View {
        MenuSection("Advanced") {
            MenuRow("Re-pair",
                    subtitle: "Get a fresh invite from the service.",
                    systemImage: "arrow.triangle.2.circlepath") {
                Task { await model.daemonSetup(.repair) }
            }
            .disabled(model.pending != nil)

            if status.paired {
                MenuRow("Forget Pairing", systemImage: "link.badge.plus", role: .destructive) {
                    Task { await model.unpairDaemon() }
                }
                .disabled(model.pending != nil)
            }

            MenuRow("Remove Service", systemImage: "trash", role: .destructive) {
                Task { await model.daemonSetup(.uninstall) }
            }
            .disabled(model.pending != nil)
        }
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
                .font(.caption)
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

                MenuRow("Copy Command", systemImage: "doc.on.doc") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(cmd.command, forType: .string)
                    model.setTransientStatus("Command copied.")
                }
                MenuRow("Open in Terminal", systemImage: "terminal") {
                    openInTerminal(cmd.command, model: model)
                }
                MenuRow("Refresh Status", systemImage: "arrow.clockwise") {
                    Task {
                        model.clearDaemonCommand()
                        await model.loadDaemonStatus()
                    }
                }
            } else {
                Text("No command is available for this step.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, Metrics.rowPaddingH)
            }
        }
    }

    private var unavailable: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Daemon mode is unavailable")
                .font(.callout.weight(.medium))
            Text("This build or platform has no system-service engine. The classic mode works normally.")
                .font(.caption)
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
