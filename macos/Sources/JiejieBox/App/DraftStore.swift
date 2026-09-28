// DraftStore — the stable owner of every text a user is part-way through typing.
//
// THE BUG THIS EXISTS TO PREVENT. These fields used to be declared as
//
//	private let urlField = FieldState()
//
// on the View struct itself. A SwiftUI View is a VALUE that the parent rebuilds
// on every body pass, so those references have no guaranteed lifetime across a
// re-render: any model change that redraws the enclosing view can hand the body
// a fresh `FieldState` whose `text` is empty. The user's half-typed subscription
// URL disappears mid-sentence.
//
// The project had already learned this twice — `HoverStore` exists for exactly
// this reason on `HoverState`, and its header says so. The forms were the one
// place the lesson was not applied, and they are the place where losing the
// value also loses the user's WORK rather than a hover tint.
//
// The drafts therefore live here, owned by the model:
//
//   * Ownership is explicit and external to any View, so a re-render cannot
//     replace them. The model outlives every screen.
//   * Identity is a stable KEY, not a struct instance, so two screens that are
//     re-entered keep their own draft and two different subscriptions do not
//     share one.
//   * `@Observable` is used rather than @State for the same toolchain reason
//     recorded in AppModel and MenuRow: this build has the Observation macro
//     plugin but not the SwiftUI one, so @State does not compile.
//
// A draft is deliberately NOT cleared by a failed submission. Every comment in
// the forms promises the typed value survives an error so a typo can be
// corrected, and with the value now owned here that promise is actually
// enforceable — see `RetainedDraft` and its tests.

import Foundation
import Observation

/// One editable text draft.
@Observable
final class TextDraft {
    var text: String

    init(_ text: String = "") { self.text = text }

    var trimmed: String {
        text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    var isEmpty: Bool { trimmed.isEmpty }
}

/// Every draft the frontend keeps, keyed by a stable identity.
///
/// One container rather than a static per-screen singleton: the key carries the
/// identity, so an edit screen opened for subscription A and then for B gets two
/// drafts instead of A's values appearing in B's form.
@MainActor
@Observable
final class DraftStore {
    private var drafts: [String: TextDraft] = [:]

    /// Fetch the draft for a key, creating it empty on first use.
    ///
    /// Returns the SAME instance for the same key on every call, which is what
    /// makes it safe to call from a View body.
    func draft(_ key: String) -> TextDraft {
        if let existing = drafts[key] { return existing }
        let created = TextDraft()
        drafts[key] = created
        return created
    }

    /// Set a draft's text, creating it if needed. Used to seed a form from the
    /// record it edits.
    func set(_ key: String, _ text: String) {
        draft(key).text = text
    }

    /// Forget a draft. Called when the work it belonged to is finished, so a
    /// later visit starts clean rather than restoring a submitted form.
    func clear(_ key: String) {
        drafts[key] = nil
    }

    /// True when the draft exists and holds non-whitespace text.
    func hasContent(_ key: String) -> Bool {
        guard let draft = drafts[key] else { return false }
        return !draft.isEmpty
    }

    // MARK: - Keys
    //
    // Named constants, not inline strings at the call sites: a typo in a string
    // literal produces a silently separate draft, which looks exactly like the
    // bug this file exists to fix.

    /// The Add Subscription form. One draft, because there is only ever one of
    /// these screens.
    static let addSubscriptionURL = "addSubscription.url"
    static let addSubscriptionName = "addSubscription.name"

    /// The Pair screen's invite field.
    static let daemonInvite = "daemonPair.invite"

    /// Confirmation prompts. Held here for the same reason as the text fields:
    /// these were `private let` holders on the View, so a model update while a
    /// dialog was open could drop the flag and dismiss the prompt — or worse,
    /// lose a confirmation the user had already been shown the consequence for.
    ///
    /// The sentinel is a word rather than a Bool so the same holder type serves
    /// both, and so no dialog can be opened by an accidental `true` from
    /// somewhere else.
    static let homeAdoptConfig = "home.adoptConfigConfirm"
    static let daemonUnpair = "daemon.unpairConfirm"
    static let daemonRemoveService = "daemon.removeServiceConfirm"

    /// The word a confirmation draft holds while its dialog is open.
    static let confirmWord = "confirm"

    /// Edit Subscription's fields, keyed BY SUBSCRIPTION so editing a second
    /// source does not inherit the first one's uncommitted edits.
    static func editName(_ id: String) -> String { "editSubscription.\(id).name" }
    static func editURL(_ id: String) -> String { "editSubscription.\(id).url" }
    /// The phrase typed to confirm a deletion.
    static func editConfirmDelete(_ id: String) -> String { "editSubscription.\(id).confirmDelete" }

    static let editAllPrefix = "editSubscription."
}
