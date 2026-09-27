// CoreModeView — choose the engine.
//
// Classic and Daemon are deliberately NOT presented as two equal radio options.
// They have different prerequisites: classic works immediately as a child
// process, while the daemon needs an installed, paired and reachable system
// service. Rendering them as symmetric choices is what made picking Daemon
// appear to hang — it tried to activate an engine that was never set up.
//
// So the Daemon row is a doorway: when the engine is ready it activates, and
// when it is not it opens the Daemon screen, which owns the setup sequence.

import SwiftUI

struct CoreModeView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    var body: some View {
        PanelScaffold(model: model, title: L.coreMode.tr(language),
                      onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                MenuSection(L.engine.tr(language)) {
                    classicRow
                    daemonRow
                }

                if let reason = model.coreModeBlockedReason(language) {
                    Text(reason)
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }

                if model.coreModePreferenceDiverged {
                    // The running engine and the saved preference disagree:
                    // the switch worked but the choice did not persist. Both
                    // names come from the protocol identifier, never from a
                    // rendered label.
                    Text("\(L.runningWithSaved.tr(language)) \(model.coreModeLabel(language))"
                         + " (\(L.savedPreference.tr(language)): \(model.savedCoreModeLabel(language)))")
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }

                if model.pending == .switchingMode("classic")
                    || model.pending == .switchingMode("daemon") {
                    PendingRow(L.switchingEngine.tr(language))
                }
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
        }
        .task {
            // The daemon row's subtitle is a status claim, so it is loaded
            // rather than guessed.
            if model.daemon == nil { await model.loadDaemonStatus() }
        }
    }

    // MARK: - Rows

    /// Classic: a plain engine choice, activated directly.
    ///
    /// Disabled only when it cannot be acted on — already active, or the core
    /// is not in a state where switching is safe — and the footer explains why.
    private var classicRow: some View {
        MenuRow(L.classic.tr(language),
                subtitle: L.classicSubtitle.tr(language),
                action: {
                    guard !classicActive else { return }
                    Task { await model.activateClassicMode() }
                },
                trailing: { activeBadge(classicActive) })
            .disabled(!model.canSwitchCoreMode || classicActive)
    }

    /// Daemon: ALWAYS a doorway to the Daemon screen.
    ///
    /// It deliberately does not activate on click. Selecting an engine and
    /// managing a system service are different jobs, and one row that sometimes
    /// switches mode and sometimes opens a page is confusing — worse, gating
    /// the row on "may I switch engines" made it unreachable in exactly the
    /// state where the user most needs it: daemon active with the VPN running.
    ///
    /// Viewing status and changing mode are separate permissions. This row only
    /// ever navigates, so it is never disabled, and it always shows a chevron
    /// so it reads as a destination rather than as inert text.
    private var daemonRow: some View {
        MenuRow(L.daemon.tr(language),
                subtitle: daemonSubtitle,
                showsChevron: true,
                action: { model.path.append(.daemon) },
                trailing: { activeBadge(daemonActive) })
    }

    @ViewBuilder
    private func activeBadge(_ active: Bool) -> some View {
        if active {
            Text(L.active.tr(language))
                .font(Typography.rowValue.weight(.medium))
                .foregroundStyle(.tint)
        }
    }

    // MARK: - Daemon state

    /// Active engine, from RUNTIME state (`core.backend`), not the saved
    /// preference. The two can diverge when a switch succeeded but persisting
    /// it did not, and calling the saved value "Active" would misdescribe what
    /// is actually running.
    /// Compared against the protocol identifier, never a rendered label: the
    /// label is language-dependent, so comparing it would silently pick the
    /// wrong row in any interface that is not English.
    private var daemonActive: Bool { model.activeEngine == "daemon" }
    private var classicActive: Bool { model.activeEngine == "classic" }

    private var daemonReady: Bool { model.daemon?.ready == true }

    /// Says what the engine's state actually is, so the row answers "can I use
    /// this?" before it is clicked.
    private var daemonSubtitle: String {
        guard let status = model.daemon else {
            return L.daemonSubtitleDefault.tr(language)
        }
        if !status.supported {
            return L.daemonNotInBuild.tr(language)
        }
        if daemonActive {
            return status.ready
                ? L.daemonActiveClick.tr(language)
                : L.daemonActiveAttention.tr(language)
        }
        // A core without `lxd` cannot host the service at all, and saying
        // "setup required" would send the user to a screen that cannot help.
        if !status.core_supports_lxd {
            return L.daemonNoCoreSupport.tr(language)
        }
        // Driven by the structured next-step field rather than by the rendered
        // English `summary` string, which is for display only.
        switch status.nextStep {
        case .install: return L.daemonNotInstalledClick.tr(language)
        case .start: return L.daemonInstalledStopped.tr(language)
        case .pair: return L.daemonNotPaired.tr(language)
        case .wait: return L.daemonServiceReady.tr(language)
        case nil: return L.daemonUnreachable.tr(language)
        }
    }
}
