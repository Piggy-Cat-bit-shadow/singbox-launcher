// SubscriptionURLInput — the frontend's half of the subscription-URL contract.
//
// The backend is the authority on whether a URL is acceptable; this exists so the
// UI does not DISABLE a button for a URL the backend would happily accept. That
// mismatch is the worst kind of frontend validation, because the user is given no
// error to read — the control simply never becomes available.
//
// It is stated once, here, rather than as a prefix test inside each form: Add and
// Edit had their own copies and they could drift from each other as easily as
// from the backend.

import Foundation

enum SubscriptionURLInput {
    /// True when the text is plausibly a subscription URL.
    ///
    /// Mirrors `looksLikeURL` in backend/service/subscriptions.go, which lowercases
    /// before testing the scheme. Case-insensitivity is the whole point: `HTTPS://`
    /// is a valid URL, and a case-sensitive test here left the Add button disabled
    /// forever on a URL the backend accepts.
    ///
    /// Deliberately NOT a full URL parser. The backend only checks the scheme and
    /// the host, and this must stay no STRICTER than that — a frontend that rejects
    /// what the backend allows is the bug being fixed, and a frontend that accepts
    /// more merely produces the backend's own error message, which the screen now
    /// shows.
    static func looksValid(_ text: String) -> Bool {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        let lowered = trimmed.lowercased()
        guard lowered.hasPrefix("http://") || lowered.hasPrefix("https://") else {
            return false
        }
        // A scheme with no host is not a URL the fetcher can use.
        let afterScheme = lowered.hasPrefix("https://")
            ? trimmed.dropFirst("https://".count)
            : trimmed.dropFirst("http://".count)
        return !afterScheme.trimmingCharacters(in: .whitespaces).isEmpty
    }
}
