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
	dto := protocol.SubscriptionDTO{
		ID:       src.ID,
		Name:     src.Name,
		URL:      src.URL,
		Enabled:  src.Enabled,
		MaxNodes: src.MaxNodes,
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

	s, path, err := b.loadState()
	if err != nil {
		return protocol.SubscriptionDTO{}, err
	}

	// A duplicate URL is almost always a double-click or a paste twice, not a
	// deliberate second copy: the same link would fetch the same nodes twice.
	for _, existing := range s.GetSubscriptionSources() {
		if strings.EqualFold(strings.TrimSpace(existing.URL), url) {
			return protocol.SubscriptionDTO{}, &protocol.Error{
				Code:        "duplicate",
				Message:     "this subscription URL is already configured",
				Recoverable: false,
			}
		}
	}

	src := state.NewSubscriptionSource(name, url)
	s.Sources = append(s.Sources, src)

	if err := s.Save(path); err != nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code:        "save_failed",
			Message:     "cannot save the subscription: " + err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: subscription %q added (%s)", name, src.ID)
	stored := s.FindSource(src.ID)
	if stored == nil {
		stored = &src
	}
	return toSubscriptionDTO(stored), nil
}

// UpdateSubscription edits the name, URL and enabled flag of one source.
//
// Only the fields the caller actually sends are changed, so an edit that does
// not touch the URL cannot silently blank it.
func (b *Backend) UpdateSubscription(id, name, url string, enabled *bool) (protocol.SubscriptionDTO, error) {
	if b.ac == nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	s, path, err := b.loadState()
	if err != nil {
		return protocol.SubscriptionDTO{}, err
	}
	src := s.FindSource(id)
	if src == nil || src.Kind != state.SourceKindSubscription {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code: "not_found", Message: "no such subscription", Recoverable: false,
		}
	}

	if trimmed := strings.TrimSpace(url); trimmed != "" {
		if !looksLikeURL(trimmed) {
			return protocol.SubscriptionDTO{}, &protocol.Error{
				Code:        "bad_request",
				Message:     "the subscription URL must start with http:// or https://",
				Recoverable: false,
			}
		}
		src.URL = trimmed
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		src.Name = trimmed
	}
	if enabled != nil {
		src.Enabled = *enabled
	}

	if err := s.Save(path); err != nil {
		return protocol.SubscriptionDTO{}, &protocol.Error{
			Code:        "save_failed",
			Message:     "cannot save the subscription: " + err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: subscription %q updated", id)
	return toSubscriptionDTO(src), nil
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

	s, path, err := b.loadState()
	if err != nil {
		return err
	}

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
	if err := s.Save(path); err != nil {
		return &protocol.Error{
			Code:        "save_failed",
			Message:     "cannot remove the subscription: " + err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: subscription %q removed", id)
	b.markConfigStale()
	return nil
}

// SetSubscriptionEnabled turns one source on or off.
func (b *Backend) SetSubscriptionEnabled(id string, enabled bool) (protocol.SubscriptionDTO, error) {
	return b.UpdateSubscription(id, "", "", &enabled)
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
