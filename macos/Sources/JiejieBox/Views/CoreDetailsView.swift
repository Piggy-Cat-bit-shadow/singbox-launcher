// CoreDetailsView — runtime diagnostics.
//
// This replaces the old full Diagnostics page. It shows the facts that matter
// when something is wrong, without a second dashboard: status, versions, the
// resolved paths, and the backend identity.
//
// Missing values are shown as "Unknown"/"Missing" rather than omitted, so a
// blank row is never ambiguous between "no data" and "not implemented".

import SwiftUI

struct CoreDetailsView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    var body: some View {
        PanelScaffold(model: model, title: L.coreDetails.tr(language),
                      onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                MenuSection(L.core.tr(language)) {
                    detail(L.status.tr(language), model.core?.state.label(language))
                    detail(L.version.tr(language), model.core?.core_version,
                           fallback: L.unknown.tr(language))
                    detail(L.mode.tr(language), model.coreModeLabel(language))
                    detail(L.binary.tr(language),
                           model.coreMissing ? L.missing.tr(language) : L.found.tr(language))
                    detail(L.configFile.tr(language),
                           model.configMissing ? L.missing.tr(language) : L.ready.tr(language))
                }

                MenuSection(L.backendVersion.tr(language)) {
                    detail(L.version.tr(language), model.handshake?.backend_version,
                           fallback: L.unknown.tr(language))
                    detail(L.protocolVersion.tr(language), "\(model.handshake?.protocol_version ?? 0)")
                    detail(L.processID.tr(language), model.handshake.map { String($0.pid) },
                           fallback: "—")
                }

                MenuSection(L.paths.tr(language)) {
                    pathRow(L.configFile.tr(language), model.settings?.config_path, kind: .file)
                    pathRow(L.dataFolder.tr(language), model.settings?.data_dir, kind: .directory)
                    pathRow(L.logsFolder.tr(language), model.settings?.logs_dir, kind: .directory)
                }

                MenuSection {
                    // Restart is offered only when it MEANS something.
                    //
                    // This row had no condition at all, so it was clickable with
                    // the core stopped, starting, stopping, in error, or while
                    // another operation held the model — and every one of those
                    // is a case where "restart" is not the action the user
                    // wants. The label, the disabled state and the tooltip all
                    // come from the shared core policy, so they cannot disagree.
                    MenuRow(L.restartCore.tr(language),
                            subtitle: restartSubtitle,
                            systemImage: "arrow.clockwise") {
                        Task { await model.restartCore() }
                    }
                    .disabled(!corePolicy.canRestart)
                    .help(corePolicy.canRestart
                          ? L.restartCoreHelp.tr(language)
                          : (corePolicy.reason ?? L.restartNeedsRunningCore.tr(language)))
                    if model.pending == .restarting {
                        pendingRow(L.restarting.tr(language))
                    }
                }
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
        }
    }

    /// A read-only detail row.
    private func detail(_ title: String, _ value: String? = nil, fallback: String = "—") -> some View {
        HStack(spacing: 8) {
            Text(title)
            Spacer(minLength: 8)
            Text(value?.isEmpty == false ? value! : fallback)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
                .help(value ?? fallback)
        }
        .frame(minHeight: Metrics.rowHeight)
        .padding(.horizontal, Metrics.rowPaddingH)
    }

    /// What a path row points at, because a file and a directory are opened
    /// differently. Treating all three paths as files meant "Data" and "Logs"
    /// revealed a folder's parent with the folder selected, rather than opening
    /// the folder — the same click doing two different things depending on the
    /// row, with nothing to tell the user which was which.
    enum PathKind {
        case file
        case directory
    }

    /// A path row that reveals the location on click.
    ///
    /// The row is disabled when the path is unknown, and the help text says
    /// which action it performs, so what a click will do is never a surprise.
    private func pathRow(_ title: String, _ path: String?, kind: PathKind) -> some View {
        let usable = !(path ?? "").isEmpty
        let action = kind == .file
            ? L.revealInFinder.tr(language)
            : L.openFolder.tr(language)
        return MenuRow(title,
                       subtitle: usable ? action : L.pathNotReported.tr(language),
                       systemImage: kind == .file ? "doc" : "folder",
                       value: usable ? abbreviate(path!) : L.unknown.tr(language),
                       action: {
                           guard let path, !path.isEmpty else { return }
                           switch kind {
                           case .file:
                               NSWorkspace.shared.selectFile(path, inFileViewerRootedAtPath: "")
                           case .directory:
                               NSWorkspace.shared.open(URL(fileURLWithPath: path))
                           }
                       })
            .disabled(!usable)
            .help(usable ? "\(action): \(path!)" : "The backend did not report this path.")
    }

    private func pendingRow(_ text: String) -> some View {
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text(text).font(Typography.rowSubtitle).foregroundStyle(.secondary)
            Spacer()
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .frame(minHeight: 24)
    }

    /// Shorten a long path for display; the full value is in the tooltip.
    private func abbreviate(_ path: String) -> String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        if path.hasPrefix(home) {
            return "~" + path.dropFirst(home.count)
        }
        return path
    }
    /// The shared core policy, so this screen and Home cannot disagree.
    private var corePolicy: AppModel.CoreActionPolicy {
        model.coreActionPolicy(language: language)
    }

    /// The restart row's subtitle: the normal help, or the reason it is blocked.
    private var restartSubtitle: String {
        corePolicy.canRestart
            ? L.restartCoreHelp.tr(language)
            : (model.core?.state == .running
                ? L.waitForOperation.tr(language)
                : L.restartNeedsRunningCore.tr(language))
    }

}
