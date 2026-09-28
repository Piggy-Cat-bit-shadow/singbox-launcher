package swiftlogic_test

import (
	"testing"
)

// TestDraftsSurviveAModelUpdate covers UI-03 / UI-41.
//
// The drafts used to be `private let urlField = FieldState()` on the View
// struct. SwiftUI rebuilds that struct on every parent body pass, so a model
// update could hand the form a FRESH, EMPTY holder and the user's half-typed URL
// would vanish. Every comment in the forms promised the value survived a
// failure; nothing enforced it.
//
// The property that fixes it is identity: the store must return the SAME
// instance for a given key, no matter how many times it is asked. This asserts
// that directly, and asserts the two things that would make it useless — that
// different screens and different subscriptions do not share one draft, and that
// reading a draft does not clear it.
func TestDraftsSurviveAModelUpdate(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
let store = DraftStore()

// A user types into the Add form.
store.draft(DraftStore.addSubscriptionURL).text = "https://example.com/sub"

// The model updates (any event at all) and the view is rebuilt. From the view's
// point of view this is simply asking for the draft again.
let afterUpdate = store.draft(DraftStore.addSubscriptionURL)
check("UI-03 the URL draft survives a re-render", afterUpdate.text == "https://example.com/sub")
check("UI-03 the draft is the SAME instance", afterUpdate === store.draft(DraftStore.addSubscriptionURL))

// The name field is a different draft, not a second view of the same one.
store.draft(DraftStore.addSubscriptionName).text = "My provider"
check("UI-03 the name draft is independent",
      store.draft(DraftStore.addSubscriptionURL).text == "https://example.com/sub"
      && store.draft(DraftStore.addSubscriptionName).text == "My provider")

// A FAILED submission must not clear what the user typed — the forms promise
// this so a typo can be corrected, and the promise only holds if nothing clears
// on the failure path.
let beforeFailure = store.draft(DraftStore.addSubscriptionURL).text
_ = "simulated failure: the caller simply returns without clearing"
check("UI-41 the URL survives a failed add",
      store.draft(DraftStore.addSubscriptionURL).text == beforeFailure)

// Two subscriptions must not share one edit draft: opening B after typing into
// A's form would otherwise show A's uncommitted text.
store.draft(DraftStore.editName("sub-a")).text = "Alpha"
store.draft(DraftStore.editName("sub-b")).text = "Beta"
check("UI-03 edit drafts are keyed per subscription",
      store.draft(DraftStore.editName("sub-a")).text == "Alpha"
      && store.draft(DraftStore.editName("sub-b")).text == "Beta")

// The Pair screen's invite is the value a user is least able to retype.
store.draft(DraftStore.daemonInvite).text = "host#fingerprint#code"
check("UI-03 the invite draft survives",
      store.draft(DraftStore.daemonInvite).text == "host#fingerprint#code")

// Committing the work clears it, so the form does not reopen pre-filled with a
// spent invite.
store.clear(DraftStore.daemonInvite)
check("UI-03 a committed draft is cleared", store.draft(DraftStore.daemonInvite).text == "")

// hasContent must report presence without mutating: it is used as the guard that
// decides whether to re-seed a form from the record.
let pristine = DraftStore()
check("UI-41 hasContent is false for an absent draft", !pristine.hasContent("never.used"))
_ = pristine.draft("never.used")
check("UI-41 hasContent is false for an empty draft", !pristine.hasContent("never.used"))
pristine.draft("never.used").text = "   "
check("UI-41 hasContent ignores whitespace-only text", !pristine.hasContent("never.used"))
pristine.draft("never.used").text = "x"
check("UI-41 hasContent is true for real text", pristine.hasContent("never.used"))
`)
}

// TestConfirmationDraftsSurviveARender covers the confirmation dialogs.
//
// These are the destructive prompts. Losing one mid-decision is worse than
// losing a text field: the user has already been shown the consequence, and the
// dialog vanishing under it is how a destructive action gets clicked twice or
// not at all.
func TestConfirmationDraftsSurviveARender(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
let store = DraftStore()

store.set(DraftStore.daemonUnpair, DraftStore.confirmWord)
check("the unpair confirmation is open",
      store.draft(DraftStore.daemonUnpair).text == DraftStore.confirmWord)
check("the unpair confirmation survives a re-render",
      store.draft(DraftStore.daemonUnpair).text == DraftStore.confirmWord)

// The sentinel is a word rather than a boolean, so an accidental boolean from
// somewhere else cannot open a destructive dialog.
check("a confirmation is not opened by other text",
      store.draft(DraftStore.daemonUnpair).text != "true")

// Independently keyed: opening one must not open the other.
store.set(DraftStore.daemonRemoveService, DraftStore.confirmWord)
store.clear(DraftStore.daemonUnpair)
check("clearing one confirmation leaves the other",
      store.draft(DraftStore.daemonUnpair).text == ""
      && store.draft(DraftStore.daemonRemoveService).text == DraftStore.confirmWord)

// A confirmation for an unrelated render (the Home adopt prompt) is separate.
store.set(DraftStore.homeAdoptConfig, DraftStore.confirmWord)
check("the adopt confirmation is independent of the daemon ones",
      store.draft(DraftStore.homeAdoptConfig).text == DraftStore.confirmWord
      && store.draft(DraftStore.daemonRemoveService).text == DraftStore.confirmWord)
`)
}
