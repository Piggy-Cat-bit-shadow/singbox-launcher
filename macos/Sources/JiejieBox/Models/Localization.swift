// Localization — the single source of user-facing text for the macOS frontend.
//
// One table, one lookup, no second mechanism. The Go side already has its own
// locale package for BACKEND-authored strings (report text, daemon messages),
// which arrive over IPC as finished sentences; this file covers only what the
// frontend itself draws. Keeping them separate is deliberate — a frontend that
// tried to re-translate backend prose would need the backend's context and would
// drift from it immediately.
//
// Why a Swift table rather than .lproj/Strings files:
//
//   * The toolchain here is Command Line Tools only; SwiftPM resource bundles
//     are not assembled into the hand-built .app (build/build_macos_app.sh),
//     so a .strings file would compile and then silently resolve to its key at
//     runtime — the worst possible failure for localization.
//   * A compile-time table means a missing translation is a BUILD ERROR, not a
//     blank label discovered by a user. `Localization.tr` is exhaustive over the
//     enum, so `switch` completeness is enforced by the compiler.
//
// The tradeoff is that translations live in Swift rather than in translator-
// friendly files. For two languages maintained in-repo that is a good trade;
// if a third language arrives with an outside translator, that is the moment to
// revisit it.
//
// Language choice is NEVER inferred by inspecting rendered text or comparing
// against English strings. It comes from the stored preference (or the system
// locale when following it) and flows through `AppLanguage` only.

import Foundation
import Observation
import SwiftUI

// MARK: - Language

/// The language the interface is drawn in.
///
/// `system` is a distinct choice, not an alias for one of the languages: it
/// means "keep following macOS", so a user who later changes their system
/// language sees the app follow. Resolving it is a separate step (`resolved`)
/// precisely so the stored preference and the effective language cannot be
/// confused — a UI that stored the resolved value would silently convert
/// "follow system" into a hard choice on first launch.
enum AppLanguage: String, CaseIterable, Identifiable {
    case system
    case simplifiedChinese = "zh-Hans"
    case english = "en"

    var id: String { rawValue }

    /// Name of this choice for the Language row.
    ///
    /// "Follow System" is translated, because it describes a behaviour. The two
    /// concrete languages are NOT: they are shown in their own script
    /// (简体中文 / English), which is the only way a picker stays usable for
    /// someone who cannot read the language currently selected.
    func label(_ language: Localization) -> String {
        switch self {
        case .system: return L.followSystem.tr(language)
        case .simplifiedChinese: return Localization.zhHans.endonym
        case .english: return Localization.en.endonym
        }
    }

    /// The stored preference, resolved against the current system locale.
    var resolved: Localization {
        switch self {
        case .simplifiedChinese: return .zhHans
        case .english: return .en
        case .system:
            // Chinese if the user's preferred language is any Chinese variant.
            // Matching on the language code rather than the full identifier
            // keeps zh-Hant, zh-HK and zh-TW on Chinese: they are different
            // scripts, but English would be a stranger default for all of them.
            let preferred = Locale.preferredLanguages.first ?? "en"
            return preferred.hasPrefix("zh") ? .zhHans : .en
        }
    }
}

/// The concrete language a string is drawn in.
///
/// Only the real languages: `AppLanguage.system` resolves to one of these, so
/// every lookup is total and no call site has to handle "system".
enum Localization: String, CaseIterable, Identifiable {
    case zhHans
    case en

    var id: String { rawValue }

    /// Name of this language, written IN this language.
    ///
    /// Endonyms on purpose: a language picker that renders "Chinese" to someone
    /// who cannot read English is unusable precisely when it is needed most.
    var endonym: String {
        switch self {
        case .zhHans: return "简体中文"
        case .en: return "English"
        }
    }
}

/// Observable holder for the language preference.
///
/// An `@Observable` class rather than `@Published`: this toolchain ships the
/// Observation macro plugin but NOT the SwiftUI one, so `@State`/`@Published`
/// cannot compile here (verified — "external macro implementation type
/// 'SwiftUIMacros.StateMacro' could not be found"). `AppModel` already carries
/// its observable state the same way.
///
/// Persistence is UserDefaults, matching `AppearancePreference`. A language is a
/// UI-only preference: it never reaches the backend, so it must not invent an
/// IPC method or a settings.json field.
@Observable
final class LanguageStore {
    /// UserDefaults key. Deliberately NOT "AppleLanguages": writing that key
    /// changes the process-wide locale at next launch and would also move
    /// system-formatted dates and numbers, which is a bigger change than
    /// "translate the interface".
    static let defaultsKey = "appLanguage"

    /// The stored preference. Setting it persists immediately and updates every
    /// observing view, which is what makes the switch take effect without a
    /// relaunch.
    var preference: AppLanguage {
        didSet {
            guard preference != oldValue else { return }
            UserDefaults.standard.set(preference.rawValue, forKey: Self.defaultsKey)
        }
    }

    /// The language strings are actually drawn in.
    var resolved: Localization { preference.resolved }

    init(defaults: UserDefaults = .standard) {
        let stored = defaults.string(forKey: Self.defaultsKey)
        preference = AppLanguage(rawValue: stored ?? "") ?? .system
    }
}

// MARK: - Environment plumbing

/// Carries the RESOLVED language down the view tree.
///
/// The resolved value rather than the store: a view only ever needs to know
/// which language to draw, and passing the store would invite a view to change
/// the preference from somewhere other than the one screen that owns that
/// decision.
private struct LocalizationKey: EnvironmentKey {
    static let defaultValue: Localization = .en
}

extension EnvironmentValues {
    var localization: Localization {
        get { self[LocalizationKey.self] }
        set { self[LocalizationKey.self] = newValue }
    }
}

extension View {
    /// The language this view tree draws in.
    ///
    /// Views use this to pick a translated string, and — where a string is
    /// chosen by the view rather than passed in — to decide which one.
    func localized(_ language: Localization) -> some View {
        environment(\.localization, language)
    }
}

/// A `Text` from a localization key, resolved against the environment.
///
/// A dedicated view rather than an extension on `String`: keeping the key type
/// distinct means an untranslated literal cannot masquerade as a translated
/// one, and the compiler catches a key that does not exist.
struct LText: View {
    @Environment(\.localization) private var language
    let key: L

    init(_ key: L) { self.key = key }

    var body: some View {
        Text(key.tr(language))
    }
}

/// Resolve a key for use in non-`Text` positions, such as `Button` titles that
/// need a plain `String` or `.help()` tooltips.
struct Localized {
    @Environment(\.localization) private var language

    func callAsFunction(_ key: L) -> String { key.tr(language) }
}

// MARK: - Keys

/// Every user-facing string the frontend draws.
///
/// Flat and grouped by comment rather than nested by screen: a screen rename
/// should not move keys, and a flat namespace keeps the table greppable. The
/// enum name is intentionally terse at call sites via `.tr(...)`.
///
/// `CaseIterable` so the table can be enumerated as a whole: the Go-side guards
/// check a few properties textually, and a future in-app "translation coverage"
/// check (or a test run on a toolchain that has XCTest) needs to walk every key
/// without maintaining a second list that would drift.
enum L: CaseIterable {
    // Chrome
    case appName
    case back
    case backHelp
    case quit
    case quitHelp
    case quitting
    case cancel
    case save
    case delete
    case retry
    case close
    case refresh
    case copy
    case copied

    // Home
    case home
    case connected
    case disconnected
    case starting
    case stopping
    case coreError
    case start
    case stop
    case loadCore
    case loadCoreHelp
    case replaceCoreHelp
    case installingCore
    case coreNotFound
    case revealFolder
    case noConfigYet
    case configChanged
    case configChangedExternal
    case configChangedUnknown
    case configOwnershipUnknownHelp
    case adoptConfig
    case adoptConfigConfirm
    case startFailed
    case startFailedConfigRebuild
    case startFailedSpawn
    case startFailedDaemonUnreachable
    case startFailedDaemonApply
    case startFailedConfigCheck
    case startFailedPortInUse
    case startFailedCancelled
    case startFailedPrivilegedCopy
    case startFailedPermission
    case startFailedAuthTimeout
    case startFailedFastExit
    case startFailedRestartExhausted
    case stopFailed
    case configWasNotReplaced
    case reload
    case openConfig
    case speed

    // Navigation rows
    case proxies
    case subscriptions
    case coreDetails
    case coreMode
    case more
    case about
    case aboutJiejieBox

    // Proxies
    case noProxyGroups
    case noNodes
    case testAll
    case testing
    /// "Testing 12/36" — live progress inside the Test All button.
    case testingProgress
    case latencyTimedOut
    case latencyFailed
    case latencyNotMeasured
    /// Shown when the engine cannot measure latency at all.
    case latencyUnsupported
    case allTestsFailed
    case notMeasured
    case searchProxies
    case selectGroup

    // Subscriptions
    case addSubscription
    case addFromURL
    case importFromFile
    case importingFromFile
    case noSubscriptions
    case noSubscriptionsHint
    case noSubscriptionsHintLocal
    case importFromFileButton
    case sources
    case updateAll
    case updatingSubscriptions
    case subscriptionAdded
    case subscriptionSaved
    case subscriptionRemoved
    case enable
    case disable
    case enabled
    case disabled
    case saving
    case refreshNow
    case fetching
    case deleteSubscription
    case deleteSubscriptionConfirm
    case deleteSubscriptionMessage
    case providerSupport
    case nodesCount
    case importedFrom
    case importedFromAFile
    case imported
    case importedNodes
    case entriesNotRecognised
    case neverUpdated
    case lastUpdateFailed
    case updatedAgo
    case refreshNotAvailable
    case refreshNotAvailableWhy
    case source
    case status
    case nodes
    case fetched
    case lastSuccess
    case noLongerConfigured
    case nothingToSave
    case saveChanges

    // Add / edit subscription
    case addSubscriptionTitle
    case name
    case url
    case namePlaceholder
    case urlPlaceholder
    case add
    case adding
    case urlRequired

    // Configuration
    case configuration
    case reloadConfig
    case rebuildingConfig
    case configSource
    case external
    case externalExplanation
    case reloadPromptTitle
    case externalPromptTitle
    case reloadPromptBody
    case externalPromptBody
    case reloadConfigAction

    // Core
    case core
    case restartCore
    case restarting
    case restart
    case version
    case configFile
    case dataFolder
    case logsFolder
    case openConfigFolder
    case openLogs

    // Automation / application / files
    case automation
    case autoPingAfterConnect
    case autoPingHelp
    case autoUpdateSubscriptions
    case autoUpdateHelp
    case launchAtLogin
    case launchAtLoginHelp
    case application
    case appearance
    case appearanceHelp
    case language
    case languageHelp
    case followSystem
    case files
    case appearanceTitle
    case languageTitle
    case appearanceSystem
    case appearanceLight
    case appearanceDark
    case launchAtLoginFailed

    // Core mode
    case classic
    case classicHelp
    case daemon
    case daemonHelp
    case active
    case switchingEngine
    case stopBeforeSwitching

    // Daemon
    case daemonSetup
    case installService
    case startService
    case pairService
    case rePair
    case forgetPairing
    case removeService
    case refreshStatus
    case useDaemonMode
    case switchBackToClassic
    case keepVPNRunning
    case keepVPNRunningHelp
    case openCoreFolder
    case preparing
    case pairing
    case pair
    case pasteFromClipboard
    case copyCommand
    case openInTerminal
    case daemonReady
    case daemonSetupRequired
    case daemonServiceStopped
    case daemonPairingRequired
    case daemonUnavailable
    case daemonNotInstalled
    case daemonUnsafe
    case daemonUpdateRequired
    case daemonRestartRequired
    case daemonRunning
    case daemonActive
    case daemonStale
    case daemonProtocolLabel
    case daemonProtocolStale
    case service
    case address
    case fingerprint
    case daemonVersion
    case coreStatus

    // Backend down / connection
    case backendUnavailable
    case backendUnavailableHint
    case reconnecting
    case tryAgain
    case connecting

    // About
    case versionLabel
    case protocolLabel
    case github
    case telegram
    case aboutBlurb

    // Status and errors
    case anotherOperationRunning
    case operationFailed
    case notAvailable
    case unknown
    case none

    // Daemon (additional)
    case checkingDaemonStatus
    case daemonStatusNotLoaded
    case daemonModeUnavailable
    case generateNewInvite
    case continueToPair
    case inviteFormat
    case inviteOneTime
    case pairInstructions
    case noCommandAvailable
    case daemonNoLxd
    case daemonNotResponding
    case daemonSurvivesQuit
    case daemonNoEngine
    case stopVPNFromHome

    // Proxies / subscriptions (additional)
    case loadingNodes
    case loading
    case restartBackend
    case loadStatus
    case subscriptionSavedEvenIfFetchFails
    case protocolVersion
    case backendVersion
    case appVersion
    case totalLabel

    // Core details
    case mode
    case binary
    case missing
    case found
    case ready
    case processID
    case paths
    case restartCoreHelp
    case revealInFinder
    case openFolder
    case pathNotReported

    // Home additions
    case choose
    case chooseProxyGroup
    case startCoreToChoose
    case manage
    case unmeasured
    case waitForOperation
    case stopTheCore
    case startTheCore
    case downloaded
    case uploaded
    case runtime

    // Core mode
    case engine
    case classicSubtitle
    case daemonSubtitleDefault
    case daemonNotInBuild
    case daemonActiveClick
    case daemonActiveAttention
    case daemonNoCoreSupport
    case daemonServiceReady
    case daemonNotInstalledClick
    case daemonInstalledStopped
    case daemonNotPaired
    case daemonUnreachable
    case runningWithSaved
    case savedPreference
    case coreStartingWait
    case coreStoppingWait
    case coreErrorRestart

    // Daemon screen
    case paired
    case notPaired
    case connection
    case endpoint
    case errorLabel
    case statusSection
    case setupSection
    case engineSection
    case behaviourSection
    case advancedSection
    case installServiceSubtitle
    case startServiceSubtitle
    case continueToPairSubtitle
    case pairServiceSubtitle
    case noLxdLong
    case notAnsweringYet
    case notRespondingLong
    case daemonActiveLabel
    case daemonSurvivesQuitLong
    case keepRunningOnSubtitle
    case keepRunningOffSubtitle
    case keepRunningOnHelp
    case keepRunningOffHelp
    case rePairSubtitle
    case forgetPairingSubtitle
    case removeServiceSubtitle
    case removeServiceHelp
    case installCmdHint
    case daemonModeBlurb

    // Proxies screen
    case switchGroupHelp
    case activeSelectorGroup
    case coreStoppedTest
    case backendUnavailableShort
    case noNodesToTest
    case measureAllHelp
    case measureAgainHelp
    case measureNodeHelp
    case anotherOpRunning
    case nothingToTest
    case noGroup
    case searchNodes
    case clearSearch
    case coreNotRunning
    case coreNotRunningDetail
    case couldNotLoadProxies
    case backendDidNotAnswer
    case subscriptionsChangedReload
    case noSelectorGroups
    case noProxiesYet
    case noSelectorGroupsDetail
    case openSubscriptions
    case groupCountHelp
    case enabledHelp
    case disabledHelp

    // Proxy capability (engine cannot list proxies)
    case proxiesUnsupportedTitle
    case proxiesUnsupportedDaemon
    case proxiesUnsupportedGeneric
    case noGroupSelected
    case chooseGroupAbove
    case openConfigPlain

    // Frontend status messages (AppModel)
    case launchAtLoginEnabled
    case launchAtLoginDisabled
    case daemonPaired
    case pairingRemoved
    case finishDaemonSetup
    case nodeDidNotRespond
    case waitForCoreOperation
    case backendShuttingDown
}

// The table. One function, exhaustive over L, so adding a key without a
// translation is a compile error.
extension L {
    func tr(_ lang: Localization) -> String {
        switch lang {
        case .en: return Self.en(self)
        case .zhHans: return Self.zhHans(self)
        }
    }

    /// Translate and substitute positional arguments.
    ///
    /// Separate from `tr` so the common case stays a plain lookup. The format
    /// string lives in the translation table, which is what lets a language
    /// reorder the numbers — "测速 12/36" and "Testing 12/36" do not
    /// necessarily want them in the same order.
    func tr(_ lang: Localization, _ args: CVarArg...) -> String {
        String(format: tr(lang), arguments: args)
    }

    private static func en(_ key: L) -> String {
        switch key {
        case .appName: return "JiejieBox"
        case .back: return "Back"
        case .backHelp: return "Back"
        case .quit: return "Quit"
        case .quitHelp: return "Quit JiejieBox"
        case .quitting: return "Quitting…"
        case .cancel: return "Cancel"
        case .save: return "Save"
        case .delete: return "Delete"
        case .retry: return "Retry"
        case .close: return "Close"
        case .refresh: return "Refresh"
        case .copy: return "Copy"
        case .copied: return "Copied."

        case .home: return "Home"
        case .connected: return "Connected"
        case .disconnected: return "Disconnected"
        case .starting: return "Starting…"
        case .stopping: return "Stopping…"
        case .coreError: return "Error"
        case .start: return "Start"
        case .stop: return "Stop"
        case .loadCore: return "Load Core…"
        case .loadCoreHelp: return "Choose a sing-box binary to install"
        case .replaceCoreHelp: return "Replace the sing-box core"
        case .installingCore: return "Installing core…"
        case .coreNotFound: return "The sing-box core binary was not found."
        case .revealFolder: return "Reveal Folder"
        case .noConfigYet: return "No config.json yet. Add a subscription to build one."
        case .configChanged: return "The configuration has changed since it was built."
        case .configChangedExternal: return "The configuration changed, but it is managed outside JiejieBox."
        case .configChangedUnknown: return "The configuration has changed, but JiejieBox cannot confirm that it produced this configuration."
        case .configOwnershipUnknownHelp: return "JiejieBox did not build this config.json, or it was built before JiejieBox started recording that. Nothing is overwritten unless you allow it."
        case .adoptConfig: return "Let JiejieBox Manage It"
        case .adoptConfigConfirm: return "JiejieBox will be able to rebuild and overwrite config.json from now on. Continue?"
        case .startFailed: return "Failed to start"
        case .startFailedConfigRebuild: return "The configuration could not be rebuilt, so the core was not started. Open the log for the build error."
        case .startFailedSpawn: return "The core process could not be started."
        case .startFailedDaemonUnreachable: return "The VPN service did not respond. Check that it is installed and running."
        case .startFailedDaemonApply: return "The VPN service rejected or could not apply the configuration."
        case .startFailedConfigCheck: return "The core rejected the configuration. Open the config, fix the reported field, then start again."
        case .startFailedPortInUse: return "Clash API port 9090 is already in use. Free the port or change it in the config, then start again."
        case .startFailedCancelled: return "The start was cancelled."
        case .startFailedPrivilegedCopy: return "The protected core copy needs to be installed or updated"
        case .startFailedPermission: return "Permission denied"
        case .startFailedAuthTimeout: return "Authorization did not complete in time. If you finished it, the core may still start."
        case .startFailedFastExit: return "The core exited immediately after starting"
        case .startFailedRestartExhausted: return "The core kept failing and automatic restart gave up"
        case .stopFailed: return "The core could not be stopped and may still be running"
        case .configWasNotReplaced: return "Your previous working config is still in use"
        case .reload: return "Reload"
        case .openConfig: return "Open Config"
        case .speed: return "Speed"

        case .proxies: return "Proxies"
        case .subscriptions: return "Subscriptions"
        case .coreDetails: return "Core Details"
        case .coreMode: return "Core Mode"
        case .more: return "More"
        case .about: return "About"
        case .aboutJiejieBox: return "About JiejieBox"

        case .noProxyGroups: return "No proxy groups"
        case .noNodes: return "This group has no nodes."
        case .testAll: return "Test All"
        case .testing: return "Testing…"
        case .testingProgress: return "Testing %1$d/%2$d"
        case .latencyTimedOut: return "Timed out"
        case .latencyFailed: return "Failed"
        case .latencyNotMeasured: return "—"
        case .latencyUnsupported: return "Latency testing is not supported"
        case .allTestsFailed: return "All nodes failed the latency test"
        case .notMeasured: return "—"
        case .searchProxies: return "Search"
        case .selectGroup: return "Group"

        case .addSubscription: return "Add Subscription"
        case .addFromURL: return "Add from URL…"
        case .importFromFile: return "Import from File…"
        case .importingFromFile: return "Importing from file…"
        case .noSubscriptions: return "No Subscriptions"
        case .noSubscriptionsHint: return "Add a subscription URL to import proxy nodes."
        case .noSubscriptionsHintLocal: return "Add a subscription URL, or import a file from disk."
        case .importFromFileButton: return "Import from File"
        case .sources: return "Sources"
        case .updateAll: return "Update All"
        case .updatingSubscriptions: return "Updating subscriptions…"
        case .subscriptionAdded: return "Subscription added."
        case .subscriptionSaved: return "Subscription saved."
        case .subscriptionRemoved: return "Subscription removed."
        case .enable: return "Enable"
        case .disable: return "Disable"
        case .enabled: return "On"
        case .disabled: return "Off"
        case .saving: return "Saving…"
        case .refreshNow: return "Refresh Now"
        case .fetching: return "Fetching…"
        case .deleteSubscription: return "Delete Subscription"
        case .deleteSubscriptionConfirm: return "Delete this subscription?"
        case .deleteSubscriptionMessage: return "Its nodes will be removed from the configuration on the next reload."
        case .providerSupport: return "Provider Support"
        case .nodesCount: return "Nodes"
        case .importedFrom: return "Imported from"
        case .importedFromAFile: return "Imported from a file"
        case .imported: return "Imported"
        case .importedNodes: return "Imported"
        case .entriesNotRecognised: return "not recognised"
        case .neverUpdated: return "Never updated"
        case .lastUpdateFailed: return "Last update failed"
        case .updatedAgo: return "Updated"
        case .refreshNotAvailable: return "Refresh"
        case .refreshNotAvailableWhy: return "Not available for an imported file"
        case .source: return "Source"
        case .status: return "Status"
        case .nodes: return "Nodes"
        case .fetched: return "Fetched"
        case .lastSuccess: return "Last success"
        case .noLongerConfigured: return "This subscription is no longer configured."
        case .nothingToSave: return "Nothing to save yet."
        case .saveChanges: return "Save Changes"

        case .addSubscriptionTitle: return "Add Subscription"
        case .name: return "Name"
        case .url: return "URL"
        case .namePlaceholder: return "Optional"
        case .urlPlaceholder: return "https://…"
        case .add: return "Add"
        case .adding: return "Adding…"
        case .urlRequired: return "A subscription URL is required."

        case .configuration: return "Configuration"
        case .reloadConfig: return "Reload Config"
        case .rebuildingConfig: return "Rebuilding config.json…"
        case .configSource: return "Config Source"
        case .external: return "External"
        case .externalExplanation: return "This configuration is managed outside JiejieBox and cannot be rebuilt here. Edit the file directly."
        case .reloadPromptTitle: return "Configuration needs reload"
        case .externalPromptTitle: return "Configuration is managed externally"
        case .reloadPromptBody: return "Reload the config to apply the new node list."
        case .externalPromptBody: return "These changes will not take effect until the external configuration is updated."
        case .reloadConfigAction: return "Reload Config"

        case .core: return "Core"
        case .restartCore: return "Restart Core"
        case .restarting: return "Restarting…"
        case .restart: return "Restart"
        case .version: return "Version"
        case .configFile: return "Config file"
        case .dataFolder: return "Data"
        case .logsFolder: return "Logs"
        case .openConfigFolder: return "Open Config Folder"
        case .openLogs: return "Open Logs"

        case .automation: return "Automation"
        case .autoPingAfterConnect: return "Auto Ping After Connect"
        case .autoPingHelp: return "Test proxies shortly after the core connects."
        case .autoUpdateSubscriptions: return "Auto Update Subscriptions"
        case .autoUpdateHelp: return "Refresh subscription data on a schedule."
        case .launchAtLogin: return "Launch at Login"
        case .launchAtLoginHelp: return "Start JiejieBox when you sign in."
        case .application: return "Application"
        case .appearance: return "Appearance"
        case .appearanceHelp: return "How JiejieBox looks."
        case .language: return "Language"
        case .languageHelp: return "The language of the interface."
        case .followSystem: return "Follow System"
        case .files: return "Files"
        case .appearanceTitle: return "Appearance"
        case .languageTitle: return "Language"
        case .appearanceSystem: return "System"
        case .appearanceLight: return "Light"
        case .appearanceDark: return "Dark"
        case .launchAtLoginFailed: return "Could not change Launch at Login. Check Login Items in System Settings."

        case .classic: return "Classic"
        case .classicHelp: return "JiejieBox runs and supervises the core itself."
        case .daemon: return "Daemon"
        case .daemonHelp: return "A system service keeps the core running."
        case .active: return "Active"
        case .switchingEngine: return "Switching engine…"
        case .stopBeforeSwitching: return "Stop the core before switching engines."

        case .daemonSetup: return "Setup"
        case .installService: return "Install Service"
        case .startService: return "Start Service"
        case .pairService: return "Pair Service"
        case .rePair: return "Re-pair"
        case .forgetPairing: return "Forget Pairing"
        case .removeService: return "Remove Service"
        case .refreshStatus: return "Refresh Status"
        case .useDaemonMode: return "Use Daemon Mode"
        case .switchBackToClassic: return "Switch Back to Classic"
        case .keepVPNRunning: return "Keep VPN Running After Quit"
        case .keepVPNRunningHelp: return "Leave the core running when JiejieBox quits."
        case .openCoreFolder: return "Open Core Folder"
        case .preparing: return "Preparing…"
        case .pairing: return "Pairing…"
        case .pair: return "Pair"
        case .pasteFromClipboard: return "Paste from Clipboard"
        case .copyCommand: return "Copy Command"
        case .openInTerminal: return "Open in Terminal"
        case .daemonReady: return "Ready"
        case .daemonSetupRequired: return "Setup required"
        case .daemonServiceStopped: return "Service stopped"
        case .daemonPairingRequired: return "Pairing required"
        case .daemonUnavailable: return "Unavailable"
        case .daemonNotInstalled: return "Not installed"
        case .daemonUnsafe: return "Unsafe"
        case .daemonUpdateRequired: return "Update required"
        case .daemonRestartRequired: return "Restart required"
        case .daemonRunning: return "Running"
        case .daemonActive: return "Active"
        case .daemonStale: return "Update required"
        case .daemonProtocolLabel: return "Compatibility"
        case .daemonProtocolStale: return "Needs a newer daemon"
        case .service: return "Service"
        case .address: return "Address"
        case .fingerprint: return "Fingerprint"
        case .daemonVersion: return "Daemon"
        case .coreStatus: return "Core"

        case .backendUnavailable: return "The backend is not responding"
        case .backendUnavailableHint: return "JiejieBox could not reach its helper process."
        case .reconnecting: return "Reconnecting…"
        case .tryAgain: return "Try Again"
        case .connecting: return "Connecting…"

        case .versionLabel: return "Version"
        case .protocolLabel: return "Protocol"
        case .github: return "GitHub"
        case .telegram: return "Telegram"
        case .aboutBlurb: return "A menu bar client for sing-box."

        case .anotherOperationRunning: return "Another operation is still running. Wait for it to finish."
        case .operationFailed: return "The operation failed."
        case .notAvailable: return "Not available"
        case .unknown: return "Unknown"
        case .none: return "None"

        case .checkingDaemonStatus: return "Checking daemon status…"
        case .daemonStatusNotLoaded: return "Daemon status has not loaded yet."
        case .daemonModeUnavailable: return "Daemon mode is unavailable"
        case .generateNewInvite: return "Generate New Invite"
        case .continueToPair: return "Continue to Pair"
        case .inviteFormat: return "Expected format: address#fingerprint#code"
        case .inviteOneTime: return "An invite can only be used once. If this one was already used, generate a new one."
        case .pairInstructions: return "Run the pairing command in Terminal, then paste the one-time invite here."
        case .noCommandAvailable: return "No command is available for this step."
        case .daemonNoLxd: return "The installed core has no `lxd` subcommand, so it cannot run as a service."
        case .daemonNotResponding: return "The service is installed and paired but not responding."
        case .daemonSurvivesQuit: return "The VPN runs inside the system service and survives quitting the app."
        case .daemonNoEngine: return "This build or platform has no system-service engine. The classic mode works normally."
        case .stopVPNFromHome: return "Stop the VPN from Home before changing the engine."

        case .loadingNodes: return "Loading nodes…"
        case .loading: return "Loading…"
        case .restartBackend: return "Restart Backend"
        case .loadStatus: return "Load Status"
        case .subscriptionSavedEvenIfFetchFails: return "The subscription is saved even if the first fetch fails; you can update it later."
        case .protocolVersion: return "Protocol"
        case .backendVersion: return "Backend"
        case .appVersion: return "App"
        case .totalLabel: return "total"

        case .mode: return "Mode"
        case .binary: return "Binary"
        case .missing: return "Missing"
        case .found: return "Found"
        case .ready: return "Ready"
        case .processID: return "Process ID"
        case .paths: return "Paths"
        case .restartCoreHelp: return "Stop and start the core again"
        case .revealInFinder: return "Reveal in Finder"
        case .openFolder: return "Open Folder"
        case .pathNotReported: return "Path not reported"

        case .choose: return "Choose…"
        case .chooseProxyGroup: return "Choose a proxy group and node."
        case .startCoreToChoose: return "Start the core to choose a proxy."
        case .manage: return "Manage"
        case .unmeasured: return "—"
        case .waitForOperation: return "Please wait for the current operation to finish."
        case .stopTheCore: return "Stop the core"
        case .startTheCore: return "Start the core"
        case .downloaded: return "Downloaded"
        case .uploaded: return "Uploaded"
        case .runtime: return "Runtime"

        case .engine: return "Engine"
        case .classicSubtitle: return "sing-box runs as a child of JiejieBox."
        case .daemonSubtitleDefault: return "sing-box runs as a persistent system service."
        case .daemonNotInBuild: return "Not available in this build."
        case .daemonActiveClick: return "Active. Click for service details."
        case .daemonActiveAttention: return "Active, but the service needs attention. Click for details."
        case .daemonNoCoreSupport: return "The installed core has no daemon support."
        case .daemonServiceReady: return "Service ready. Click to switch."
        case .daemonNotInstalledClick: return "Not installed. Click to set up."
        case .daemonInstalledStopped: return "Installed but stopped. Click to set up."
        case .daemonNotPaired: return "Not paired. Click to set up."
        case .daemonUnreachable: return "Service unreachable. Click for details."
        case .runningWithSaved: return "Running"
        case .savedPreference: return "saved preference"
        case .coreStartingWait: return "The core is starting. Wait for it to settle."
        case .coreStoppingWait: return "The core is stopping. Wait for it to settle."
        case .coreErrorRestart: return "The core is in an error state. Restart it before switching."

        case .paired: return "Paired"
        case .notPaired: return "Not paired"
        case .connection: return "Connection"
        case .endpoint: return "Endpoint"
        case .errorLabel: return "Error"
        case .statusSection: return "Status"
        case .setupSection: return "Setup"
        case .engineSection: return "Engine"
        case .behaviourSection: return "Behaviour"
        case .advancedSection: return "Advanced"
        case .installServiceSubtitle: return "Creates the system service. Needs administrator rights."
        case .startServiceSubtitle: return "The service is installed but not running."
        case .continueToPairSubtitle: return "Paste the invite printed by Terminal."
        case .pairServiceSubtitle: return "Create a one-time invite, then pair this app."
        case .noLxdLong: return "The installed core has no `lxd` subcommand, so it cannot run as a service. Install a core that supports it, then refresh."
        case .notAnsweringYet: return "Not answering yet"
        case .notRespondingLong: return "The service is installed and paired but not responding. If you just installed it, give it a moment and refresh."
        case .daemonActiveLabel: return "Daemon active"
        case .daemonSurvivesQuitLong: return "The VPN runs inside the system service and survives quitting the app."
        case .keepRunningOnSubtitle: return "The VPN stays connected when JiejieBox quits."
        case .keepRunningOffSubtitle: return "The VPN stops when JiejieBox quits."
        case .keepRunningOnHelp: return "Click to make the VPN stop when JiejieBox quits."
        case .keepRunningOffHelp: return "Click to keep the VPN running after JiejieBox quits."
        case .rePairSubtitle: return "Create a new one-time invite for this app."
        case .forgetPairingSubtitle: return "Removes this app's pairing."
        case .removeServiceSubtitle: return "Uninstalls the system service."
        case .removeServiceHelp: return "Stops and removes the service. The core keeps working in classic mode."
        case .installCmdHint: return "Run this command in Terminal."
        case .daemonModeBlurb: return "Daemon mode keeps the core running as a system service."

        case .switchGroupHelp: return "Switch group."
        case .activeSelectorGroup: return "The active selector group."
        case .coreStoppedTest: return "Start the core to test latency."
        case .backendUnavailableShort: return "The backend is unavailable."
        case .noNodesToTest: return "This group has no nodes to test."
        case .measureAllHelp: return "Measure latency for every node in this group."
        case .measureAgainHelp: return "Measure this node again."
        case .measureNodeHelp: return "Measure this node's latency."
        case .anotherOpRunning: return "Another operation is running."
        case .nothingToTest: return "Nothing to test yet."
        case .noGroup: return "No group"
        case .searchNodes: return "Search nodes"
        case .clearSearch: return "Clear the search."
        case .coreNotRunning: return "Core is not running"
        case .coreNotRunningDetail: return "Start the core to load, test and switch nodes."
        case .couldNotLoadProxies: return "Could not load proxies"
        case .backendDidNotAnswer: return "The backend did not answer."
        case .subscriptionsChangedReload: return "Subscriptions changed, so the node list is out of date."
        case .noSelectorGroups: return "No selector groups"
        case .noProxiesYet: return "No proxies yet. Add a subscription first."
        case .noSelectorGroupsDetail: return "The current configuration defines no selector groups."
        case .openSubscriptions: return "Open Subscriptions"
        case .groupCountHelp: return "groups available."
        case .enabledHelp: return "Enabled. Click to exclude it from the built config."
        case .disabledHelp: return "Disabled. Click to include it again."

        case .proxiesUnsupportedTitle: return "Proxy list unavailable"
        case .proxiesUnsupportedDaemon: return "The daemon engine that is running does not provide the proxy group list, so nodes cannot be listed or switched here. JiejieBox is not reporting an error — this engine simply has no such capability. Switch to Classic mode on the Core Mode screen to manage proxies."
        case .proxiesUnsupportedGeneric: return "The active engine does not provide a proxy group list, so nodes cannot be listed or switched here. This is an engine capability, not a failure."
        case .noGroupSelected: return "No group selected"
        case .chooseGroupAbove: return "Choose a selector group above."
        case .openConfigPlain: return "Open Config"

        case .launchAtLoginEnabled: return "Launch at Login enabled."
        case .launchAtLoginDisabled: return "Launch at Login disabled."
        case .daemonPaired: return "Service paired."
        case .pairingRemoved: return "Pairing removed."
        case .finishDaemonSetup: return "Finish the daemon setup before switching to it."
        case .nodeDidNotRespond: return "did not respond."
        case .waitForCoreOperation: return "Wait for the current core operation to finish."
        case .backendShuttingDown: return "The backend is shutting down."
        }
    }

    private static func zhHans(_ key: L) -> String {
        switch key {
        case .appName: return "JiejieBox"
        case .back: return "返回"
        case .backHelp: return "返回"
        case .quit: return "退出"
        case .quitHelp: return "退出 JiejieBox"
        case .quitting: return "正在退出…"
        case .cancel: return "取消"
        case .save: return "保存"
        case .delete: return "删除"
        case .retry: return "重试"
        case .close: return "关闭"
        case .refresh: return "刷新"
        case .copy: return "复制"
        case .copied: return "已复制。"

        case .home: return "主页"
        case .connected: return "已连接"
        case .disconnected: return "未连接"
        case .starting: return "正在启动…"
        case .stopping: return "正在停止…"
        case .coreError: return "出错"
        case .start: return "启动"
        case .stop: return "停止"
        case .loadCore: return "载入内核…"
        case .loadCoreHelp: return "选择要安装的 sing-box 可执行文件"
        case .replaceCoreHelp: return "替换 sing-box 内核"
        case .installingCore: return "正在安装内核…"
        case .coreNotFound: return "未找到 sing-box 内核文件。"
        case .revealFolder: return "打开所在文件夹"
        case .noConfigYet: return "还没有 config.json。添加订阅后即可生成。"
        case .configChanged: return "配置在生成之后已被修改。"
        case .configChangedExternal: return "配置已变化，但它由 JiejieBox 之外的工具管理。"
        case .configChangedUnknown: return "检测到配置变化，但无法确认这个配置是否由 JiejieBox 生成。"
        case .configOwnershipUnknownHelp: return "JiejieBox 无法确认这份 config.json 是否由自己生成，或者它是在 JiejieBox 开始记录之前生成的。除非你允许，否则不会覆盖它。"
        case .adoptConfig: return "设为 JiejieBox 管理"
        case .adoptConfigConfirm: return "以后 JiejieBox 可以重建并覆盖 config.json。是否继续？"
        case .startFailed: return "启动失败"
        case .startFailedConfigRebuild: return "无法重新生成配置，因此内核未启动。请打开日志查看构建错误。"
        case .startFailedSpawn: return "无法启动内核进程。"
        case .startFailedDaemonUnreachable: return "VPN 服务没有响应。请检查它是否已安装并正在运行。"
        case .startFailedDaemonApply: return "VPN 服务拒绝或无法应用该配置。"
        case .startFailedConfigCheck: return "内核拒绝了这份配置。请打开配置，修正报错的字段后重试。"
        case .startFailedPortInUse: return "Clash API 端口 9090 已被占用。请释放该端口或在配置中更改后重试。"
        case .startFailedCancelled: return "启动已取消。"
        case .startFailedPrivilegedCopy: return "需要安装或更新受保护的内核副本"
        case .startFailedPermission: return "权限不足"
        case .startFailedAuthTimeout: return "授权未在时间内完成。如果你已完成授权，内核可能仍会启动。"
        case .startFailedFastExit: return "内核启动后立即退出"
        case .startFailedRestartExhausted: return "内核反复失败，已停止自动重启"
        case .stopFailed: return "无法停止内核，它可能仍在运行"
        case .configWasNotReplaced: return "仍在使用的上一份可用配置"
        case .reload: return "重新加载"
        case .openConfig: return "打开配置"
        case .speed: return "速度"

        case .proxies: return "代理"
        case .subscriptions: return "订阅"
        case .coreDetails: return "内核详情"
        case .coreMode: return "内核模式"
        case .more: return "更多"
        case .about: return "关于"
        case .aboutJiejieBox: return "关于 JiejieBox"

        case .noProxyGroups: return "没有代理分组"
        case .noNodes: return "该分组没有节点。"
        case .testAll: return "全部测速"
        case .testingProgress: return "测速 %1$d/%2$d"
        case .latencyTimedOut: return "超时"
        case .latencyFailed: return "失败"
        case .latencyNotMeasured: return "未测速"
        case .latencyUnsupported: return "暂不支持测速"
        case .allTestsFailed: return "所有节点测速失败"
        case .testing: return "测速中…"
        case .notMeasured: return "—"
        case .searchProxies: return "搜索"
        case .selectGroup: return "分组"

        case .addSubscription: return "添加订阅"
        case .addFromURL: return "从链接添加…"
        case .importFromFile: return "从文件导入…"
        case .importingFromFile: return "正在从文件导入…"
        case .noSubscriptions: return "暂无订阅"
        case .noSubscriptionsHint: return "添加订阅链接即可导入节点。"
        case .noSubscriptionsHintLocal: return "添加订阅链接，或从磁盘导入文件。"
        case .importFromFileButton: return "从文件导入"
        case .sources: return "来源"
        case .updateAll: return "全部更新"
        case .updatingSubscriptions: return "正在更新订阅…"
        case .subscriptionAdded: return "订阅已添加。"
        case .subscriptionSaved: return "订阅已保存。"
        case .subscriptionRemoved: return "订阅已删除。"
        case .enable: return "启用"
        case .disable: return "停用"
        case .enabled: return "已启用"
        case .disabled: return "已停用"
        case .saving: return "正在保存…"
        case .refreshNow: return "立即更新"
        case .fetching: return "正在获取…"
        case .deleteSubscription: return "删除订阅"
        case .deleteSubscriptionConfirm: return "要删除这个订阅吗？"
        case .deleteSubscriptionMessage: return "下次重新加载配置时，它的节点会被移除。"
        case .providerSupport: return "服务商支持"
        case .nodesCount: return "节点"
        case .importedFrom: return "导入自"
        case .importedFromAFile: return "导入自文件"
        case .imported: return "已导入"
        case .importedNodes: return "已导入"
        case .entriesNotRecognised: return "条无法识别"
        case .neverUpdated: return "尚未更新"
        case .lastUpdateFailed: return "上次更新失败"
        case .updatedAgo: return "更新于"
        case .refreshNotAvailable: return "更新"
        case .refreshNotAvailableWhy: return "导入的文件无法更新"
        case .source: return "来源"
        case .status: return "状态"
        case .nodes: return "节点"
        case .fetched: return "获取"
        case .lastSuccess: return "上次成功"
        case .noLongerConfigured: return "该订阅已不存在。"
        case .nothingToSave: return "暂无可保存的修改。"
        case .saveChanges: return "保存修改"

        case .addSubscriptionTitle: return "添加订阅"
        case .name: return "名称"
        case .url: return "链接"
        case .namePlaceholder: return "可选"
        case .urlPlaceholder: return "https://…"
        case .add: return "添加"
        case .adding: return "正在添加…"
        case .urlRequired: return "请填写订阅链接。"

        case .configuration: return "配置"
        case .reloadConfig: return "重新加载配置"
        case .rebuildingConfig: return "正在重新生成 config.json…"
        case .configSource: return "配置来源"
        case .external: return "外部"
        case .externalExplanation: return "该配置由 JiejieBox 之外的工具管理，无法在这里重新生成。请直接编辑该文件。"
        case .reloadPromptTitle: return "配置需要重新加载"
        case .externalPromptTitle: return "配置由外部管理"
        case .reloadPromptBody: return "重新加载配置以应用新的节点列表。"
        case .externalPromptBody: return "在外部配置更新之前，这些改动不会生效。"
        case .reloadConfigAction: return "重新加载配置"

        case .core: return "内核"
        case .restartCore: return "重启内核"
        case .restarting: return "正在重启…"
        case .restart: return "重启"
        case .version: return "版本"
        case .configFile: return "配置文件"
        case .dataFolder: return "数据目录"
        case .logsFolder: return "日志"
        case .openConfigFolder: return "打开配置文件夹"
        case .openLogs: return "打开日志"

        case .automation: return "自动化"
        case .autoPingAfterConnect: return "连接后自动测速"
        case .autoPingHelp: return "内核连接后自动测试节点延迟。"
        case .autoUpdateSubscriptions: return "自动更新订阅"
        case .autoUpdateHelp: return "按计划刷新订阅数据。"
        case .launchAtLogin: return "开机启动"
        case .launchAtLoginHelp: return "登录时自动启动 JiejieBox。"
        case .application: return "应用"
        case .appearance: return "外观"
        case .appearanceHelp: return "JiejieBox 的外观样式。"
        case .language: return "语言"
        case .languageHelp: return "界面显示语言。"
        case .followSystem: return "跟随系统"
        case .files: return "文件"
        case .appearanceTitle: return "外观"
        case .languageTitle: return "语言"
        case .appearanceSystem: return "跟随系统"
        case .appearanceLight: return "浅色"
        case .appearanceDark: return "深色"
        case .launchAtLoginFailed: return "无法修改开机启动。请在「系统设置」的「登录项」中检查。"

        case .classic: return "经典"
        case .classicHelp: return "由 JiejieBox 自己运行并守护内核。"
        case .daemon: return "守护进程"
        case .daemonHelp: return "由系统服务保持内核运行。"
        case .active: return "使用中"
        case .switchingEngine: return "正在切换引擎…"
        case .stopBeforeSwitching: return "请先停止内核，再切换引擎。"

        case .daemonSetup: return "安装配置"
        case .installService: return "安装服务"
        case .startService: return "启动服务"
        case .pairService: return "配对服务"
        case .rePair: return "重新配对"
        case .forgetPairing: return "取消配对"
        case .removeService: return "移除服务"
        case .refreshStatus: return "刷新状态"
        case .useDaemonMode: return "切换到守护进程模式"
        case .switchBackToClassic: return "切回经典模式"
        case .keepVPNRunning: return "退出后保持连接"
        case .keepVPNRunningHelp: return "退出 JiejieBox 后让内核继续运行。"
        case .openCoreFolder: return "打开内核目录"
        case .preparing: return "正在准备…"
        case .pair: return "配对"
        case .pasteFromClipboard: return "从剪贴板粘贴"
        case .copyCommand: return "复制命令"
        case .openInTerminal: return "在终端中打开"
        case .daemonReady: return "已就绪"
        case .daemonSetupRequired: return "需要配置"
        case .daemonServiceStopped: return "服务已停止"
        case .daemonPairingRequired: return "需要配对"
        case .daemonUnavailable: return "不可用"
        case .daemonNotInstalled: return "未安装"
        case .daemonUnsafe: return "不安全"
        case .daemonUpdateRequired: return "需要更新"
        case .daemonRestartRequired: return "需要重启"
        case .daemonRunning: return "运行中"
        case .daemonActive: return "使用中"
        case .daemonStale: return "需要更新"
        case .daemonProtocolLabel: return "兼容性"
        case .daemonProtocolStale: return "需要更新守护服务"
        case .service: return "服务"
        case .address: return "地址"
        case .fingerprint: return "指纹"
        case .daemonVersion: return "守护进程"
        case .coreStatus: return "内核"

        case .backendUnavailable: return "后端无响应"
        case .backendUnavailableHint: return "JiejieBox 无法连接到它的后台进程。"
        case .reconnecting: return "正在重新连接…"
        case .tryAgain: return "重试"
        case .connecting: return "正在连接…"

        case .versionLabel: return "版本"
        case .protocolLabel: return "协议"
        case .github: return "GitHub"
        case .telegram: return "Telegram"
        case .aboutBlurb: return "一个 sing-box 菜单栏客户端。"

        case .anotherOperationRunning: return "还有操作正在进行，请等待完成。"
        case .operationFailed: return "操作失败。"
        case .notAvailable: return "不可用"
        case .unknown: return "未知"
        case .none: return "无"

        case .checkingDaemonStatus: return "正在检查服务状态…"
        case .daemonStatusNotLoaded: return "尚未获取服务状态。"
        case .daemonModeUnavailable: return "守护进程模式不可用"
        case .generateNewInvite: return "生成新的邀请码"
        case .continueToPair: return "继续配对"
        case .inviteFormat: return "格式应为：地址#指纹#验证码"
        case .inviteOneTime: return "邀请码只能使用一次。如果这个已经用过，请重新生成。"
        case .pairInstructions: return "在终端中运行配对命令，然后在此粘贴一次性邀请码。"
        case .noCommandAvailable: return "该步骤暂无可用命令。"
        case .daemonNoLxd: return "当前内核没有 `lxd` 子命令，无法作为系统服务运行。"
        case .daemonNotResponding: return "服务已安装并完成配对，但没有响应。"
        case .daemonSurvivesQuit: return "连接由系统服务维持，退出应用后仍然保持。"
        case .daemonNoEngine: return "当前版本或平台不提供系统服务引擎。经典模式可正常使用。"
        case .stopVPNFromHome: return "请先在主页停止连接，再切换引擎。"

        case .loadingNodes: return "正在载入节点…"
        case .loading: return "正在载入…"
        case .restartBackend: return "重启后端"
        case .loadStatus: return "载入状态"
        case .subscriptionSavedEvenIfFetchFails: return "即使首次获取失败，订阅也会被保存，之后可以再更新。"
        case .protocolVersion: return "协议"
        case .backendVersion: return "后端"
        case .appVersion: return "应用"
        case .totalLabel: return "合计"

        case .mode: return "模式"
        case .binary: return "内核文件"
        case .missing: return "缺失"
        case .found: return "已找到"
        case .ready: return "就绪"
        case .processID: return "进程号"
        case .paths: return "路径"
        case .restartCoreHelp: return "停止并重新启动内核"
        case .revealInFinder: return "在访达中显示"
        case .openFolder: return "打开文件夹"
        case .pathNotReported: return "未获取到路径"

        case .choose: return "请选择…"
        case .chooseProxyGroup: return "选择代理分组和节点。"
        case .startCoreToChoose: return "启动内核后即可选择代理。"
        case .manage: return "管理"
        case .unmeasured: return "—"
        case .waitForOperation: return "请等待当前操作完成。"
        case .stopTheCore: return "停止内核"
        case .startTheCore: return "启动内核"
        case .downloaded: return "已下载"
        case .uploaded: return "已上传"
        case .runtime: return "运行状态"

        case .engine: return "引擎"
        case .classicSubtitle: return "sing-box 作为 JiejieBox 的子进程运行。"
        case .daemonSubtitleDefault: return "sing-box 作为常驻系统服务运行。"
        case .daemonNotInBuild: return "当前版本不支持。"
        case .daemonActiveClick: return "使用中。点击查看服务详情。"
        case .daemonActiveAttention: return "使用中，但服务需要处理。点击查看详情。"
        case .daemonNoCoreSupport: return "当前内核不支持守护进程。"
        case .daemonServiceReady: return "服务已就绪。点击切换。"
        case .daemonNotInstalledClick: return "未安装。点击开始配置。"
        case .daemonInstalledStopped: return "已安装但未启动。点击开始配置。"
        case .daemonNotPaired: return "未配对。点击开始配置。"
        case .daemonUnreachable: return "服务无响应。点击查看详情。"
        case .runningWithSaved: return "正在运行"
        case .savedPreference: return "已保存的选择"
        case .coreStartingWait: return "内核正在启动，请等待完成。"
        case .coreStoppingWait: return "内核正在停止，请等待完成。"
        case .coreErrorRestart: return "内核处于错误状态，请先重启再切换。"

        case .pairing: return "配对"
        case .paired: return "已配对"
        case .notPaired: return "未配对"
        case .connection: return "连接"
        case .endpoint: return "地址"
        case .errorLabel: return "错误"
        case .statusSection: return "状态"
        case .setupSection: return "安装配置"
        case .engineSection: return "引擎"
        case .behaviourSection: return "行为"
        case .advancedSection: return "高级"
        case .installServiceSubtitle: return "创建系统服务，需要管理员权限。"
        case .startServiceSubtitle: return "服务已安装，但没有运行。"
        case .continueToPairSubtitle: return "粘贴终端中输出的邀请码。"
        case .pairServiceSubtitle: return "生成一次性邀请码，然后配对。"
        case .noLxdLong: return "当前内核没有 `lxd` 子命令，无法作为系统服务运行。请更换支持该功能的内核，然后刷新。"
        case .notAnsweringYet: return "尚无响应"
        case .notRespondingLong: return "服务已安装并完成配对，但没有响应。如果是刚安装，请稍等片刻再刷新。"
        case .daemonActiveLabel: return "守护进程使用中"
        case .daemonSurvivesQuitLong: return "连接由系统服务维持，退出应用后仍然保持。"
        case .keepRunningOnSubtitle: return "退出 JiejieBox 后保持连接。"
        case .keepRunningOffSubtitle: return "退出 JiejieBox 时断开连接。"
        case .keepRunningOnHelp: return "点击可改为退出时断开连接。"
        case .keepRunningOffHelp: return "点击可在退出后保持连接。"
        case .rePairSubtitle: return "为本应用生成新的一次性邀请码。"
        case .forgetPairingSubtitle: return "移除此应用的配对信息。"
        case .removeServiceSubtitle: return "卸载系统服务。"
        case .removeServiceHelp: return "停止并移除服务。内核仍可在经典模式下工作。"
        case .installCmdHint: return "请在终端中运行此命令。"
        case .daemonModeBlurb: return "守护进程模式让内核作为系统服务持续运行。"

        case .switchGroupHelp: return "切换分组。"
        case .activeSelectorGroup: return "当前使用的分组。"
        case .coreStoppedTest: return "启动内核后即可测试延迟。"
        case .backendUnavailableShort: return "后端不可用。"
        case .noNodesToTest: return "该分组没有可测试的节点。"
        case .measureAllHelp: return "测试该分组中所有节点的延迟。"
        case .measureAgainHelp: return "重新测量该节点的延迟。"
        case .measureNodeHelp: return "测量该节点的延迟。"
        case .anotherOpRunning: return "还有其他操作正在进行。"
        case .nothingToTest: return "暂无可测试内容。"
        case .noGroup: return "无分组"
        case .searchNodes: return "搜索节点"
        case .clearSearch: return "清除搜索。"
        case .coreNotRunning: return "内核未运行"
        case .coreNotRunningDetail: return "启动内核后即可载入、测试和切换节点。"
        case .backendDidNotAnswer: return "后端没有响应。"
        case .subscriptionsChangedReload: return "订阅已变化，节点列表已过期。"
        case .noSelectorGroups: return "没有代理分组"
        case .noProxiesYet: return "暂无代理，请先添加订阅。"
        case .noSelectorGroupsDetail: return "当前配置中没有定义选择器分组。"
        case .openSubscriptions: return "打开订阅"
        case .groupCountHelp: return "个分组可用。"
        case .enabledHelp: return "已启用。点击可将其排除在生成的配置之外。"
        case .disabledHelp: return "已停用。点击可重新启用。"

        case .proxiesUnsupportedTitle: return "无法读取代理列表"
        case .proxiesUnsupportedDaemon: return "正在运行的守护进程内核不提供代理分组列表，因此这里无法列出或切换节点。这不是出错，而是该内核本身没有这项能力。如需管理代理，请在「内核模式」中切换到经典模式。"
        case .proxiesUnsupportedGeneric: return "当前使用的内核不提供代理分组列表，因此这里无法列出或切换节点。这是内核能力所限，并非故障。"
        case .couldNotLoadProxies: return "无法载入代理"
        case .noGroupSelected: return "未选择分组"
        case .chooseGroupAbove: return "请在上方选择一个分组。"
        case .openConfigPlain: return "打开配置"

        case .launchAtLoginEnabled: return "已开启开机启动。"
        case .launchAtLoginDisabled: return "已关闭开机启动。"
        case .daemonPaired: return "服务已配对。"
        case .pairingRemoved: return "已取消配对。"
        case .finishDaemonSetup: return "请先完成守护进程的配置，再切换到该模式。"
        case .nodeDidNotRespond: return "没有响应。"
        case .waitForCoreOperation: return "请等待当前内核操作完成。"
        case .backendShuttingDown: return "后端正在关闭。"
        }
    }
}
