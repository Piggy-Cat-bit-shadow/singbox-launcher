// JiejieBoxApp — the native macOS menu-bar entry point.
//
// MenuBarExtra only: there is no WindowGroup, so the app has no main window and
// no Dock icon (LSUIElement is set in Info.plist). The window style is .window
// so clicking the status item opens a compact SwiftUI panel rather than a menu.

import SwiftUI

@main
struct JiejieBoxApp: App {
    private let model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            RootView(model: model)
                .task { await model.start() }
        } label: {
            // A template symbol so macOS tints it for light and dark menu bars.
            Image(systemName: menuBarSymbol)
                .accessibilityLabel("JiejieBox")
        }
        .menuBarExtraStyle(.window)
    }

    /// Status icon reflects core state without being colourful: menu-bar items
    /// should stay monochrome and match the system style.
    private var menuBarSymbol: String {
        switch model.core?.state {
        case .running: return "bolt.horizontal.circle.fill"
        case .starting, .stopping: return "bolt.horizontal.circle"
        case .error: return "exclamationmark.triangle"
        default: return "bolt.horizontal.circle"
        }
    }
}

/// Content of the menu-bar window: a compact navigation stack.
struct RootView: View {
    let model: AppModel

    var body: some View {
        NavigationStack(path: Binding(
            get: { model.path },
            set: { model.path = $0 }
        )) {
            HomeView(model: model)
                .navigationDestination(for: AppModel.Screen.self) { screen in
                    switch screen {
                    case .coreDetails: CoreDetailsView(model: model)
                    case .coreMode: CoreModeView(model: model)
                    case .proxies: ProxiesView(model: model)
                    case .more: MoreView(model: model)
                    case .about: AboutView(model: model)
                    }
                }
        }
        // ~400pt: enough for real information, still a menu-bar utility.
        .frame(minWidth: Metrics.panelWidth, maxWidth: Metrics.panelWidth,
               minHeight: 320, maxHeight: 640)
        .preferredColorScheme(model.appearance.colorScheme)
    }
}
