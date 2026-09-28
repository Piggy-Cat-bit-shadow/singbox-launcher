// Subscription manager: the minimal CRUD surface the menu bar needs.
//
// This is NOT the old configurator. It reads and writes the canonical v7 state
// source tree (core/state) through the existing state API, so there is exactly
// one subscription truth on disk and the wizard, the backup importer and this
// screen all operate on the same records.
//
// Deliberately absent: identity overrides, tag policy, skip rules, update
// schedules, replacement rules. Those keep their existing state defaults; a
// manager that exposed every field would be the configurator again.

package service

import (
	"errors"
	"strings"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core/state"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

// loadState reads the canonical wizard state.
//
// A missing state.json is NOT an error: core/state documents ErrNotFound as
// "fresh install", and the menu bar must be able to add the user's first
// subscription without sending them through the wizard first. Returning an
// empty state here means Save creates the file in the canonical shape.
// withStateLocked runs one read-modify-write of state.json under the shared lock.
//
// WHY EVERY MUTATION GOES THROUGH HERE. The subscription refresh path takes
// `SubscriptionMu` around its own load-modify-save, but the CRUD paths
// (add/update/remove) loaded state, edited it and saved it back WITHOUT the lock. Two
// writers on one file is not merely a lost update: concurrent whole-file writes were
// observed to interleave and leave state.json unparseable, because each Save is a
// truncate-and-write that the other can walk into.
//
// The lock must span the LOAD as well as the save. Loading outside it and locking only
// for the write still lets this sequence through: refresh loads v10, the user's edit
// loads v10, the edit saves v11, the refresh saves its stale v10 copy — the user's
// change is gone and nothing reports an error.
//
// The callback must not perform network I/O. A caller that needs to fetch does the
// fetch OUTSIDE this function and comes back with only the fields it owns; holding
// this lock across a slow subscription would queue every subscription edit in the UI
// behind a network call.
func (b *Backend) withStateLocked(fn func(s *state.State, path string) error) error {
	if b.ac == nil {
		return &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	b.ac.SubscriptionMu.Lock()
	defer b.ac.SubscriptionMu.Unlock()

	s, path, err := b.loadState()
	if err != nil {
		return err
	}
	if err := fn(s, path); err != nil {
		return err
	}
	if err := s.Save(path); err != nil {
		return &protocol.Error{
			Code:        "save_failed",
			Message:     "cannot save the subscription state: " + err.Error(),
			Recoverable: true,
		}
	}
	return nil
}

// loadState reads state.json WITHOUT the lock.
//
// Read-only callers may use it directly. Any caller that will WRITE must go through
// withStateLocked instead, so that the load it mutates cannot be stale by the time it
// saves.
func (b *Backend) loadState() (*state.State, string, error) {
	dataDir := b.ac.FileService.Layout.Data
	path := platform.GetWizardStatePath(dataDir)
	s, err := state.Load(path)
	if err == nil {
		return s, path, nil
	}
	if errors.Is(err, state.ErrNotFound) {
		debuglog.InfoLog("backend: no state.json yet — starting from an empty state")
		return state.New(), path, nil
	}
	return nil, path, &protocol.Error{
		Code:        "state_unreadable",
		Message:     "cannot read state.json: " + err.Error(),
		Recoverable: true,
	}
}

// Subscriptions lists the configured subscription sources.
//
// Only subscription-kind sources are returned: folders and hand-made nodes are
// not subscriptions and have no URL to manage.
func (b *Backend) Subscriptions() ([]protocol.SubscriptionDTO, error) {
	if b.ac == nil {
		return nil, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	s, _, err := b.loadState()
	if err != nil {
		return nil, err
	}

	sources := s.GetSubscriptionSources()
	out := make([]protocol.SubscriptionDTO, 0, len(sources))
	for i := range sources {
		out = append(out, toSubscriptionDTO(&sources[i]))
	}
	return out, nil
}

// toSubscriptionDTO projects a state.Source onto the compact wire shape.
//
// The frontend never sees the full Source: it carries node bodies, identity
// overrides and update schedules that a menu bar neither shows nor edits.
// Nil-safe because an absent record must yield an empty DTO, not a panic.
func toSubscriptionDTO(src *state.Source) protocol.SubscriptionDTO {
	if src == nil {
		return protocol.SubscriptionDTO{}
	}
	inputKind := state.SubscriptionInputKindOf(src)
	// An absent name means "no custom name", so the display name falls back to
	// the provider's profile title and then to the URL's host. Resolved here, at
	// the one place a DTO is built, so every consumer sees the same label and no
	// screen has to reimplement the fallback — and so clearing a custom name
	// produces a USABLE name rather than a blank row.
	displayName := src.Name
	if displayName == "" && src.Meta != nil && src.Meta.ProfileTitle != "" {
		displayName = src.Meta.ProfileTitle
	}
	if displayName == "" {
		displayName = displayNameFromURL(src.URL)
	}
	dto := protocol.SubscriptionDTO{
		ID:   src.ID,
		Name: displayName,
		// Whether the stored name is a CUSTOM one. The edit form needs this to
		// tell "the user has a custom name" from "this is the derived label", so
		// it can offer a clear only when there is something to clear.
		HasCustomName: src.Name != "",
		URL:           src.URL,
		Enabled:       src.Enabled,
		MaxNodes:      src.MaxNodes,
		InputKind:     string(inputKind),
		CanRefresh:    state.CanRefreshSubscription(src),
		Filename:      src.LocalFilename,
		// Only enabled, non-unsupported nodes count: reporting every record
		// would advertise nodes the build will not emit.
		NodeCount: countUsableNodes(src),
	}
	if src.Meta != nil {
		dto.ProfileTitle = src.Meta.ProfileTitle
		dto.SupportURL = src.Meta.SupportURL
	}
	if src.UpdateStatus != nil {
		dto.LastAttempt = src.UpdateStatus.LastAttemptAt
		dto.LastSuccess = src.UpdateStatus.LastSuccessAt
		dto.LastStatus = src.UpdateStatus.LastStatus
		dto.LastError = src.UpdateStatus.LastErrorMsg
		dto.HTTPStatusCode = src.UpdateStatus.HTTPStatusCode
		dto.NodesFetched = src.UpdateStatus.NodesCountFetched
	}
	return dto
}

// countUsableNodes counts the subscription's materialised nodes.
func countUsableNodes(src *state.Source) int {
	count := 0
	for i := range src.Nodes {
		if src.Nodes[i].Kind == state.SourceKindUnsupported {
			continue
		}
		count++
	}
	return count
}

// AddSubscription appends a subscription source and persists it.
//
// The URL is required; the name is not. When the name is blank the backend
// derives one from the URL host, because forcing a display name on someone who
// just wants to paste a link is friction with no benefit — the provider's own
// title replaces it after the first successful fetch anyway.
func (b *Backend) AddSubscription(name, url string) (protocol.SubscriptionDTO, error) {
	if b.ac == nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	url = strings.TrimSpace(url)
	if url == "" {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code: "bad_request", Message: "a subscription URL is required", Recoverable: false,
		}
	}
	if !looksLikeURL(url) {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code:        "bad_request",
			Message:     "the subscription URL must start with http:// or https://",
			Recoverable: false,
		}
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = displayNameFromURL(url)
	}

	// The whole load-modify-save runs under the shared lock, so a concurrent
	// refresh cannot interleave its own whole-file write.
	var added *state.Source
	err := b.withStateLocked(func(s *state.State, path string) error {
		// A duplicate URL is almost always a double-click or a paste twice, not a
		// deliberate second copy: the same link would fetch the same nodes twice.
		if dup := findSourceByURL(s, url, ""); dup != nil {
			return &protocol.Error{
				Code:        "duplicate",
				Message:     "this subscription URL is already configured",
				Recoverable: false,
			}
		}
		src := state.NewSubscriptionSource(name, url)
		s.Sources = append(s.Sources, src)
		copied := src
		added = &copied
		return nil
	})
	if err != nil {
		return protocol.SubscriptionDTO{}, err
	}

	// UI-30.
	//
	// Adding a source changes the BUILD INPUTS exactly as removing one does: the
	// materialised config no longer matches what the tree would produce, so the
	// cached config is stale and the user must be offered a reload.
	//
	// This call was missing here while RemoveSubscription, UpdateSubscription and
	// SetSubscriptionEnabled all made it, which made adding a subscription the one
	// edit that gave no reload prompt — the user added a provider, saw it listed,
	// and the core kept running the old config with no indication that anything
	// still needed to happen.
	b.noteBuildInputsChanged()

	debuglog.InfoLog("backend: subscription %q added (%s)", name, added.ID)
	return toSubscriptionDTO(added), nil
}

// findSourceByURL returns any subscription already using this URL, excluding
// excludeID (the source being edited).
//
// Case-insensitive and whitespace-trimmed because a URL that differs only in case or
// a trailing space is the same provider: treating them as distinct would fetch the
// same nodes twice and collide on tags.
func findSourceByURL(s *state.State, url, excludeID string) *state.Source {
	if s == nil {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(url))
	if want == "" {
		return nil
	}
	sources := s.GetSubscriptionSources()
	for i := range sources {
		if sources[i].ID == excludeID {
			continue
		}
		if strings.ToLower(strings.TrimSpace(sources[i].URL)) == want {
			return &sources[i]
		}
	}
	return nil
}

// UpdateSubscription edits the name, URL and enabled flag of one source.
//
// Only the fields the caller actually sends are changed, so an edit that does
// not touch the URL cannot silently blank it.
func (b *Backend) UpdateSubscription(id, name, url string, enabled *bool, clearName bool) (protocol.SubscriptionDTO, error) {
	if b.ac == nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	trimmedURL := strings.TrimSpace(url)
	if trimmedURL != "" && !looksLikeURL(trimmedURL) {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code:        "bad_request",
			Message:     "the subscription URL must start with http:// or https://",
			Recoverable: false,
		}
	}
	trimmedName := strings.TrimSpace(name)

	// The whole load-modify-save runs under the shared lock, so a refresh cannot
	// replace the file in the middle of the edit.
	var updated *state.Source
	var enabledChanged, urlChanged, nameChanged bool
	err := b.withStateLocked(func(s *state.State, path string) error {
		src := s.FindSource(id)
		if src == nil || src.Kind != state.SourceKindSubscription {
			return &protocol.Error{
				Code: "not_found", Message: "no such subscription", Recoverable: false,
			}
		}

		if trimmedURL != "" && trimmedURL != src.URL {
			// The invariant that Add enforces must hold for edits too, or it is not
			// an invariant: two sources on one URL fetch the same nodes twice.
			if dup := findSourceByURL(s, trimmedURL, id); dup != nil {
				return &protocol.Error{
					Code:        "duplicate",
					Message:     "another subscription already uses this URL",
					Recoverable: false,
				}
			}
			src.URL = trimmedURL
			// THE URL IS THE SOURCE'S IDENTITY. Everything materialised from it —
			// nodes, provider metadata, fetch status — describes the PREVIOUS
			// provider. Keeping them made the UI show the new URL beside the old
			// provider's node count, and a rebuild could install nodes that the
			// displayed URL does not serve.
			//
			// Cleared rather than kept as "last known good": the user has just told
			// the launcher this source is now a different provider, and silently
			// building the old provider's nodes under the new URL is exactly the
			// confusion this state must not represent. The refresh is required, and
			// the stale flag below says so.
			src.Nodes = nil
			src.Meta = nil
			src.UpdateStatus = nil
			urlChanged = true
		}
		// An explicit clear falls back to the provider/default title rather than
		// storing an empty name, so the source keeps a usable label everywhere it
		// is displayed. `clearName` is a separate input because `name == ""` also
		// means "this edit does not mention the name" — the two cannot share one
		// representation without making the clear impossible to request.
		if clearName {
			src.Name = ""
			nameChanged = true
		} else if trimmedName != "" && trimmedName != src.Name {
			src.Name = trimmedName
			nameChanged = true
		}
		if enabled != nil && *enabled != src.Enabled {
			src.Enabled = *enabled
			enabledChanged = true
		}
		copied := *src
		updated = &copied
		return nil
	})
	if err != nil {
		return protocol.SubscriptionDTO{}, err
	}

	// A change that alters WHAT THE BUILD WOULD PRODUCE must mark the config stale,
	// or the UI keeps presenting the old config.json as current.
	//
	// `enabled` decides whether a source participates in the build at all, and a URL
	// change replaces which nodes it contributes. Both leave config.json describing
	// something other than the current state, and neither previously said so: the
	// Home screen showed no Reload prompt and the core kept running the old config.
	if enabledChanged || urlChanged {
		b.noteBuildInputsChanged()
	} else if nameChanged {
		// A display-name edit changes no build input, so the config is NOT stale
		// and the Reload prompt must not appear for it. The subscriptions list is
		// still announced, because the sidebar shows the name.
		b.emit(protocol.EventSubscriptionsChanged, nil)
	}

	debuglog.InfoLog("backend: subscription %q updated", id)
	return toSubscriptionDTO(updated), nil
}

// noteBuildInputsChanged records that the materialised config no longer matches the
// state it was built from, and tells the UI.
//
// One place rather than at each call site: this is the same fact every time (a build
// input moved), and stating it once is what keeps a future edit path from forgetting
// to.
func (b *Backend) noteBuildInputsChanged() {
	if b.ac == nil {
		return
	}
	if b.ac.StateService != nil {
		b.ac.StateService.MarkConfigStale()
	}
	b.EmitCoreState()
	b.emit(protocol.EventSubscriptionsChanged, nil)
}

// RemoveSubscription deletes a source.
//
// Deleting a source only removes it from the tree; the materialised config is
// left alone until the user reloads, which mirrors how the rest of the product
// treats state changes (see MarkConfigStale callers).
func (b *Backend) RemoveSubscription(id string) error {
	if b.ac == nil {
		return &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	// Removal is a read-modify-write like any other, so it takes the same lock: this
	// path previously saved the whole file without it, and a concurrent refresh
	// could resurrect the source or corrupt the file.
	removed := false
	err := b.withStateLocked(func(s *state.State, path string) error {
		idx := -1
		for i := range s.Sources {
			if s.Sources[i].ID == id && s.Sources[i].Kind == state.SourceKindSubscription {
				idx = i
				break
			}
		}
		if idx < 0 {
			return &protocol.Error{
				Code: "not_found", Message: "no such subscription", Recoverable: false,
			}
		}
		s.Sources = append(s.Sources[:idx], s.Sources[idx+1:]...)
		removed = true
		return nil
	})
	if err != nil {
		return err
	}
	if removed {
		// Removing a source removes the nodes it contributed, so the built config no
		// longer describes the state.
		b.noteBuildInputsChanged()
	}

	debuglog.InfoLog("backend: subscription %q removed", id)
	b.markConfigStale()
	return nil
}

// SetSubscriptionEnabled turns one source on or off.
func (b *Backend) SetSubscriptionEnabled(id string, enabled bool) (protocol.SubscriptionDTO, error) {
	return b.UpdateSubscription(id, "", "", &enabled, false)
}

// RefreshSubscription fetches one source.
//
// It reuses ConfigService.RefreshSingleSubscription, which serialises through
// SubscriptionMu and marks the config stale but never rebuilds on its own —
// rebuilding stays the user's decision, surfaced in the UI as "reload needed".
func (b *Backend) RefreshSubscription(id string) (protocol.SubscriptionDTO, error) {
	if b.ac == nil || b.ac.ConfigService == nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code: "not_ready", Message: "the configuration service is not available", Recoverable: true,
		}
	}

	// Final defence: the UI hides Refresh for a local snapshot, but a stale
	// client must not be able to send it to the network with an empty URL.
	if s, _, lerr := b.loadState(); lerr == nil {
		if existing := s.FindSource(id); existing != nil && !state.CanRefreshSubscription(existing) {
			return protocol.SubscriptionDTO{}, &protocol.Error{
				Code: "not_refreshable",
				Message: "this subscription was imported from a file and has no " +
					"provider to refresh from. Delete it and import the file again.",
				Recoverable: false,
			}
		}
	}

	src, err := b.ac.ConfigService.RefreshSingleSubscription(id)
	if err != nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code:        "refresh_failed",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	// Re-read from disk: RefreshSingleSubscription saved the updated status and
	// node list, and reporting the in-memory copy could disagree with the file
	// the next build will read.
	if s, _, lerr := b.loadState(); lerr == nil {
		if stored := s.FindSource(id); stored != nil {
			src = stored
		}
	}

	b.markConfigStale()
	return toSubscriptionDTO(src), nil
}

// markConfigStale flags that the built config no longer matches the state.
func (b *Backend) markConfigStale() {
	if b.ac != nil && b.ac.StateService != nil {
		b.ac.StateService.MarkConfigStale()
	}
	// Emit the core state too: config_stale lives there, so the Home banner
	// and the Reload affordance update without waiting for the next poll.
	b.EmitCoreState()
	b.emit(protocol.EventSubscriptionsChanged, nil)
}

// looksLikeURL is a deliberately shallow check: the backend must not reject a
// provider URL with an unusual shape, only obvious non-URLs. The fetch itself
// is the real validator, and a failed fetch keeps the source with an error.
func looksLikeURL(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// LooksLikeURLForTest exposes the URL-acceptance rule AS THE API APPLIES IT, to
// the cross-language contract test.
//
// Exported because the frontend must apply the SAME rule, and the only way to
// assert that is to compare the two implementations. A frontend whose rule is
// STRICTER than this one silently disables its own submit button on input the
// backend would accept, and the user is given no error to explain it — the defect
// this pair of functions exists to prevent.
//
// The TRIM is part of the rule, not an accident of one call site: AddSubscription
// trims before validating, so "  https://…  " — which is what a paste from a
// terminal or a chat message often looks like — is accepted. Exposing the bare
// predicate would have encoded a rule the product does not actually apply, and
// the contract test would then have demanded the frontend reject input the
// backend takes.
//
// Named for its purpose rather than as a general API: nothing in the product
// should call it, and the suffix makes that obvious at a call site.
func LooksLikeURLForTest(raw string) bool { return looksLikeURL(strings.TrimSpace(raw)) }

// displayNameFromURL derives a readable name from a URL host.
func displayNameFromURL(raw string) string {
	withoutScheme := raw
	if i := strings.Index(withoutScheme, "://"); i >= 0 {
		withoutScheme = withoutScheme[i+3:]
	}
	if i := strings.IndexAny(withoutScheme, "/?#"); i >= 0 {
		withoutScheme = withoutScheme[:i]
	}
	if withoutScheme == "" {
		return "Subscription"
	}
	return withoutScheme
}
