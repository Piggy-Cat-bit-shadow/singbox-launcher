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
                // Detached from the panel's lifetime on purpose. The backend
                // serves the whole app, not the panel: closing the menu bar
                // window must not tear down the helper, its event stream, or
                // the traffic sampler. `bootstrap()` is idempotent, so running
                // it on every panel appearance is safe and a re-opened panel
                // simply shows the state the model already holds.
                .task { await model.bootstrap() }
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
///
/// NavigationStack is kept only for the path/push machinery. The back and quit
/// controls are drawn by PanelScaffold on every screen, because the system back
/// affordance is a toolbar item that a borderless menu-bar panel cannot reliably
/// present — which is how a page could previously be entered with no way out.
/// The navigation bar itself is hidden so there is never a second back arrow.
struct RootView: View {
    let model: AppModel

    var body: some View {
        NavigationStack(path: Binding(
            get: { model.path },
            set: { model.path = $0 }
        )) {
            HomeView(model: model)
                .navigationBarBackButtonHidden(true)
                .navigationDestination(for: AppModel.Screen.self) { screen in
                    destination(screen)
                        .navigationBarBackButtonHidden(true)
                }
        }
        // ~420pt: wide enough for a subscription URL or an invite, still a
        // menu-bar utility rather than a main window.
        .frame(minWidth: Metrics.panelWidth, maxWidth: Metrics.panelWidth,
               minHeight: 360, maxHeight: 680)
        .preferredColorScheme(model.appearance.colorScheme)
        // Reading `language.resolved` here is what makes the switch take effect
        // without a relaunch: the store is @Observable, so changing the
        // preference re-evaluates this body and re-injects the new language
        // into every descendant.
        .localized(model.language.resolved)
    }

    @ViewBuilder
    private func destination(_ screen: AppModel.Screen) -> some View {
        switch screen {
        case .coreDetails: CoreDetailsView(model: model)
        case .coreMode: CoreModeView(model: model)
        case .proxies: ProxiesView(model: model)
        case .subscriptions: SubscriptionsView(model: model)
        case .addSubscription: AddSubscriptionView(model: model)
        case .editSubscription(let id): EditSubscriptionView(model: model, id: id)
        case .daemon: DaemonView(model: model)
        case .daemonPair: DaemonPairView(model: model)
        case .more: MoreView(model: model)
        case .about: AboutView(model: model)
        }
    }
}
