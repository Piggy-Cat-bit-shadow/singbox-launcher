// ProxiesView — pick a group, pick a node, see its latency.
//
// The screen is deliberately one list rather than a group list plus a node
// list: a menu bar is a small surface, and the group picker is a menu, not a
// second column. Every row is full-width and clickable (MenuRow), because
// selecting a node is the whole purpose of the screen and a user aiming at a
// 20 pt checkmark is a user the UI has failed.

import SwiftUI

struct ProxiesView: View {
    let model: AppModel

    var body: some View {
        PanelScaffold(model: model, title: "Proxies", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                if !model.proxiesAvailable {
                    unavailable
                } else {
                    header
                    searchField
                    nodeList
                }
            }
            .padding(.vertical, 8)
        }
        .task {
            // Loading in the view's task (not on appear-and-forget) means the
            // spinner reflects the real request lifetime.
            if model.groups.isEmpty {
                await model.loadGroups()
            } else {
                await model.loadProxies()
            }
        }
    }

    // MARK: - Pieces

    private var header: some View {
        HStack(spacing: 6) {
            Menu {
                ForEach(model.groups) { group in
                    Button {
                        Task { await model.selectGroup(group.name) }
                    } label: {
                        // A checkmark marks the group in use; the label shows
                        // the node it currently points at, which is the fact
                        // the user actually wants.
                        if group.name == model.selectedGroup {
                            Label(groupSelectionLabel(group), systemImage: "checkmark")
                        } else {
                            Text(groupSelectionLabel(group))
                        }
                    }
                }
            } label: {
                HStack(spacing: 4) {
                    Text(currentGroupLabel)
                        .font(.subheadline.weight(.medium))
                        .lineLimit(1)
                    Image(systemName: "chevron.up.chevron.down")
                        .font(.caption2)
                }
            }
            .menuStyle(.borderlessButton)
            .fixedSize()

            Spacer()

            // A count makes the search field self-explanatory: with a filter
            // active the number shows how much it hid.
            if !model.proxies.isEmpty {
                Text(filterCountLabel)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .monospacedDigit()
            }

            Button {
                Task { await model.testGroup() }
            } label: {
                if model.pending == .testingGroup {
                    ProgressView().controlSize(.small)
                } else {
                    Text("Test All")
                }
            }
            .buttonStyle(.borderless)
            .disabled(model.pending != nil)
            .help("Measure latency for every node in this group.")
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }

    /// "12 nodes", or "3 of 12" while a search is narrowing the list.
    private var filterCountLabel: String {
        let total = model.proxies.count
        let shown = model.filteredProxies.count
        if shown == total {
            return total == 1 ? "1 node" : "\(total) nodes"
        }
        return "\(shown) of \(total)"
    }

    /// The group label plus the node it currently selects, so the menu answers
    /// "what am I actually using?" without opening it.
    private func groupSelectionLabel(_ group: ProxyGroup) -> String {
        guard let selected = group.selected_display ?? group.selected, !selected.isEmpty else {
            return group.label
        }
        return "\(group.label) — \(selected)"
    }

    private var currentGroupLabel: String {
        let group = model.groups.first { $0.name == model.selectedGroup }
        let base = group?.label ?? (model.selectedGroup.isEmpty ? "Proxies" : model.selectedGroup)
        guard let selected = group?.selected_display ?? group?.selected, !selected.isEmpty else {
            return base
        }
        return "\(base) — \(selected)"
    }

    private var searchField: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .foregroundStyle(.secondary)
            TextField("Search nodes", text: Binding(get: { model.proxySearch },
                                                    set: { model.proxySearch = $0 }))
                .textFieldStyle(.plain)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }

    @ViewBuilder
    private var nodeList: some View {
        if model.proxiesLoading && model.proxies.isEmpty {
            HStack(spacing: 6) {
                ProgressView().controlSize(.small)
                Text("Loading…").font(.caption).foregroundStyle(.secondary)
            }
            .padding(.horizontal, Metrics.rowPaddingH)
        } else if model.proxies.isEmpty {
            emptyNodes
        } else if model.filteredProxies.isEmpty {
            note("No node matches “\(model.proxySearch)”.")
        } else {
            ScrollView {
                LazyVStack(alignment: .leading, spacing: Metrics.sectionSpacing) {
                    ForEach(model.filteredProxies) { node in
                        nodeRow(node)
                    }
                }
                .padding(.bottom, 4)
            }
            // The panel is capped at 640pt overall, so the list scrolls rather
            // than pushing the menu bar window off screen.
            .frame(minHeight: 120, maxHeight: 340)
        }
    }

    private var emptyNodes: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("No nodes in this group.")
                .font(.callout)
            Text("Update Subscriptions in More, then reload the config.")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }

    private func note(_ text: String) -> some View {
        Text(text)
            .font(.caption)
            .foregroundStyle(.secondary)
            .padding(.horizontal, Metrics.rowPaddingH)
    }

    /// One node. The whole row selects it; the latency is a separate control
    /// so a user can re-test without switching.
    private func nodeRow(_ node: ProxyNode) -> some View {
        MenuRow(node.label,
                subtitle: node.type,
                action: { Task { await model.switchProxy(node) } },
                trailing: {
                    HStack(spacing: 8) {
                        if model.pending == .testingProxy(node.name) {
                            ProgressView().controlSize(.small)
                        } else {
                            Button {
                                Task { await model.testProxy(node) }
                            } label: {
                                Text(node.delayLabel)
                                    .font(.caption.monospacedDigit())
                                    .foregroundStyle(delayColor(node))
                            }
                            .buttonStyle(.plain)
                            .disabled(model.pending != nil)
                            .help(node.isMeasured
                                  ? "Measure this node again."
                                  : "Measure this node's latency.")
                        }

                        if node.selected {
                            Image(systemName: "checkmark")
                                .font(.caption.weight(.semibold))
                                .foregroundStyle(.tint)
                        }
                    }
                })
            .opacity(model.pending == .testingProxy(node.name) ? 0.6 : 1)
    }

    private func delayColor(_ node: ProxyNode) -> Color {
        if !node.isMeasured { return .secondary }
        if node.isFast { return .green }
        if node.isSlow { return .orange }
        return .primary
    }

    // MARK: - Unavailable

    private var unavailable: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Proxies are unavailable")
                .font(.callout.weight(.medium))
            Text(model.core?.state == .running
                 ? "The Clash API is not answering yet. Give it a moment after connecting."
                 : "Start the core to list and switch proxies.")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }
}
