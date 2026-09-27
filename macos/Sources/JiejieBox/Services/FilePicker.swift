// FilePicker — choosing a local file to hand to the backend.
//
// The frontend's entire role here is selection: it returns a path and nothing
// else. It never reads the file, checks its format, counts its contents or
// copies it anywhere. Parsing and installing belong to the backend, which owns
// the shared pipeline and the state; duplicating any of that here would create
// a second opinion about what a file contains.
//
// Anything the user must be told about a rejected file therefore comes back
// from the backend as a real error, phrased for a human.

import AppKit
import UniformTypeIdentifiers

enum FilePicker {
    /// Ask for a sing-box core binary.
    ///
    /// Returns nil when the user cancels, which callers must treat as "no
    /// action taken" rather than as a failure.
    ///
    /// `canChooseFiles` with no type filter on purpose: a sing-box binary has
    /// no filename extension and is not a registered content type, so any
    /// `allowedContentTypes` value either hides the real file or is ignored
    /// anyway. Validation is the backend's job, and it reports precisely why a
    /// candidate was refused.
    static func chooseCoreBinary() -> String? {
        let panel = NSOpenPanel()
        panel.title = "Choose sing-box core"
        panel.message = "Select a sing-box executable to install."
        panel.prompt = "Install"
        // A directory is not selectable; the backend rejects one anyway, but
        // not offering the choice keeps the error paths for real mistakes.
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        // The user may be replacing a working core, so let them see hidden
        // files and follow an alias into a build directory.
        panel.showsHiddenFiles = true
        panel.resolvesAliases = true
        panel.allowsMultipleSelection = false
        guard panel.runModal() == .OK, let url = panel.url else { return nil }
        return url.path
    }

    /// Ask for a subscription file to import.
    ///
    /// JSON, YAML and plain node lists are all supported by the backend, and
    /// providers hand out files with a variety of extensions — so the filter
    /// offers those types but does not enforce them.
    static func chooseSubscriptionFile() -> String? {
        let panel = NSOpenPanel()
        panel.title = "Import subscription from file"
        panel.message = "Select a subscription file (JSON, YAML or a list of node links)."
        panel.prompt = "Import"
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        panel.allowsMultipleSelection = false
        panel.allowedContentTypes = subscriptionContentTypes()
        // Providers rename profiles and ship them with unusual extensions, so
        // the type filter is a convenience, not a gate.
        panel.allowsOtherFileTypes = true
        guard panel.runModal() == .OK, let url = panel.url else { return nil }
        return url.path
    }

    /// Content types offered for a subscription file.
    ///
    /// Built defensively: a type that is unavailable on this OS is skipped
    /// rather than inserted as a fallback, because an empty or bogus entry
    /// would make the open panel filter out the file the user is looking at.
    private static func subscriptionContentTypes() -> [UTType] {
        var types: [UTType] = [.json, .plainText, .text]
        if let yaml = UTType(filenameExtension: "yaml") { types.append(yaml) }
        if let yml = UTType(filenameExtension: "yml") { types.append(yml) }
        if let conf = UTType(filenameExtension: "conf") { types.append(conf) }
        return types
    }
}
