// Local subscription import: turn a file the user picked into a canonical
// subscription source.
//
// This is a SNAPSHOT import, not a live link to the file. The nodes become part
// of the state, and the original file may be deleted immediately afterwards —
// keeping a path as the source of truth would break the moment the user moved,
// renamed or unplugged it.
//
// The parser pipeline is deliberately NOT reimplemented here. Remote refresh and
// local import differ in exactly one place — where the raw bytes come from — and
// after that both go through DecodeSubscriptionContent, the classifier and
// MaterializeSubscriptionBody. Two parsers would drift, and the last time this
// project had two answers to "what formats do we support" it produced a
// subscription that could not be imported at all.

package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core/config"
	"singbox-launcher/core/config/configtypes"
	"singbox-launcher/core/config/subscription"
	"singbox-launcher/core/state"
	"singbox-launcher/internal/debuglog"
)

// SubscriptionImportResult is the structured outcome of a local import.
type SubscriptionImportResult struct {
	// Subscription is the created source, in the same shape the list uses, so
	// the frontend can insert it without a second round trip.
	Subscription protocol.SubscriptionDTO `json:"subscription"`
	// NodesImported counts the nodes that will participate in the build.
	NodesImported int `json:"nodes_imported"`
	// UnsupportedCount counts records the parser read but could not turn into
	// nodes. Non-zero is a partial success, reported rather than hidden.
	UnsupportedCount int `json:"unsupported_count"`
	// Warnings is a bounded sample of the parser's reasons.
	Warnings []string `json:"warnings,omitempty"`
	// WarningsCount is the full number, since Warnings is capped.
	WarningsCount int `json:"warnings_count"`
	// RawBytes is the size of the imported file.
	RawBytes int64 `json:"raw_bytes"`
	// ConfigStale reports that the built config no longer matches the state.
	ConfigStale bool `json:"config_stale"`
	// ConfigOwnership is who owns the config on disk: managed / unknown /
	// external. Reported next to ConfigRebuildable so the UI never has to infer
	// ownership from permission.
	ConfigOwnership string `json:"config_ownership"`
	// ConfigRebuildable reports whether JiejieBox may rebuild that config; false
	// for an externally managed one.
	ConfigRebuildable bool `json:"config_rebuildable"`
}

const (
	// importWarningSampleCap bounds how many parser warnings cross the IPC
	// boundary. A file with hundreds of bad records should not produce a
	// hundreds-of-entries response; the count travels separately.
	importWarningSampleCap = 5
)

// ImportSubscriptionFile reads a local file and stores its nodes as a new
// subscription source.
//
// The state is modified only after the content has parsed into something
// trustworthy. A file that is empty, unrecognised, malformed, or that yields no
// supported nodes leaves state.json exactly as it was — a half-created source
// showing "0 nodes, error" would be worse than a refusal, because the user would
// have to find and delete it.
func (b *Backend) ImportSubscriptionFile(path string) (SubscriptionImportResult, error) {
	if b.ac == nil || b.ac.FileService == nil {
		return SubscriptionImportResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	raw, filename, err := readLocalSubscriptionFile(path)
	if err != nil {
		return SubscriptionImportResult{}, err
	}

	// The same two calls a remote refresh makes. Only the byte source differs.
	decoded, derr := subscription.DecodeSubscriptionContent(raw)
	if derr != nil {
		return SubscriptionImportResult{}, &protocol.Error{
			Code:        "unsupported_format",
			Message:     derr.Error(),
			Recoverable: false,
		}
	}

	// Parsing happens before the state lock is taken: a multi-megabyte file can
	// take a moment, and holding the subscription lock for that long would stall
	// a concurrent background refresh for no reason.
	capN := configtypes.MaxNodesPerSubscription
	material, merr := config.MaterializeSubscriptionBody("", decoded, nil, capN)
	if material == nil {
		return SubscriptionImportResult{}, &protocol.Error{
			Code:        "parse_failed",
			Message:     firstNonEmpty(errText(merr), "the file could not be parsed as a subscription"),
			Recoverable: false,
		}
	}
	if merr != nil && material.Supported == 0 {
		return SubscriptionImportResult{}, &protocol.Error{
			Code:        "parse_failed",
			Message:     merr.Error(),
			Recoverable: false,
		}
	}
	// Zero supported nodes is a refusal even without a parser error: a source
	// with nothing in it cannot build anything, and would only be deleted again.
	if material.Supported == 0 {
		return SubscriptionImportResult{}, &protocol.Error{
			Code: "no_nodes",
			Message: "the file was read but contains no usable proxy nodes" +
				warningSuffix(material.Warnings),
			Recoverable: false,
		}
	}

	name := subscriptionNameFromFilename(filename)

	// §78/§79: the same lock remote refreshes use, taken only around the
	// load-mutate-save, and the state is re-read inside it so a concurrent
	// writer's changes are not overwritten by our earlier parse-time snapshot.
	ac := b.ac
	ac.SubscriptionMu.Lock()

	s, statePath, lerr := b.loadState()
	if lerr != nil {
		ac.SubscriptionMu.Unlock()
		return SubscriptionImportResult{}, lerr
	}

	src := state.NewSubscriptionSource(name, "")
	src.InputKind = state.SubscriptionInputLocalSnapshot
	src.LocalFilename = filename
	s.Sources = append(s.Sources, src)

	// Merge through the canonical path so node identity, dedup and detach
	// handling are identical to a remote refresh.
	merged := false
	if ok, _ := state.MergeSubscriptionNodes(&s.Sources[len(s.Sources)-1], &state.SubFetchMaterial{
		Nodes:     material.Nodes,
		Truncated: material.Truncated,
	}, true); ok {
		merged = true
	}

	stored := &s.Sources[len(s.Sources)-1]

	// No HTTP happened, so no HTTP status is invented. The import time and
	// counts are real and worth recording; a fake 200 would be a lie the UI
	// would later render.
	now := time.Now().UTC().Format(time.RFC3339)
	stored.UpdateStatus = &state.SubUpdateStatus{
		LastAttemptAt:     now,
		LastSuccessAt:     now,
		LastStatus:        "ok",
		RawBodyBytes:      int64(len(raw)),
		NodesCountFetched: material.Supported,
	}

	if err := s.Save(statePath); err != nil {
		ac.SubscriptionMu.Unlock()
		return SubscriptionImportResult{}, &protocol.Error{
			Code:        "save_failed",
			Message:     "cannot save the imported subscription: " + err.Error(),
			Recoverable: true,
		}
	}
	ac.SubscriptionMu.Unlock()

	// §71: state changed, so the built config is behind it. The product never
	// rebuilds on its own — this only marks the fact and lets the UI offer it.
	b.markConfigStale()

	// Re-read so the result is the persisted record, not the in-memory one.
	reloaded, _, rerr := b.loadState()
	persisted := stored
	if rerr == nil {
		if found := reloaded.FindSource(stored.ID); found != nil {
			persisted = found
		}
	}

	// log the basename only: a full path can name a user's directory or carry a
	// token-like filename, and the count is what matters diagnostically.
	debuglog.InfoLog("import_subscription_file: imported %d node(s) from %q (%d bytes, merged=%v)",
		material.Supported, filename, len(raw), merged)

	sample := material.Warnings
	if len(sample) > importWarningSampleCap {
		sample = sample[:importWarningSampleCap]
	}

	result := SubscriptionImportResult{
		Subscription:      toSubscriptionDTO(persisted),
		NodesImported:     material.Supported,
		UnsupportedCount:  len(material.Warnings),
		Warnings:          sample,
		WarningsCount:     len(material.Warnings),
		RawBytes:          int64(len(raw)),
		ConfigStale:       b.configStale(),
		ConfigRebuildable: b.configIsRebuildable(),
		ConfigOwnership:   string(b.configOwnership()),
	}
	return result, nil
}

// readLocalSubscriptionFile validates the path and reads the file within the
// canonical size limit.
//
// The limit is the one remote fetches already use: a subscription body is a
// subscription body whatever its transport, and a second limit would be a second
// policy.
func readLocalSubscriptionFile(path string) ([]byte, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, "", &protocol.Error{
			Code: "bad_path", Message: "no file was selected", Recoverable: false,
		}
	}

	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", &protocol.Error{
				Code: "file_not_found", Message: "that file no longer exists", Recoverable: true,
			}
		}
		return nil, "", &protocol.Error{
			Code: "bad_path", Message: "cannot resolve the selected path: " + err.Error(), Recoverable: false,
		}
	}

	st, err := os.Stat(real)
	if err != nil {
		return nil, "", &protocol.Error{
			Code: "file_not_found", Message: "cannot read the selected file: " + err.Error(), Recoverable: true,
		}
	}
	if st.IsDir() {
		return nil, "", &protocol.Error{
			Code: "not_regular_file", Message: "the selected item is a folder", Recoverable: false,
		}
	}
	if !st.Mode().IsRegular() {
		return nil, "", &protocol.Error{
			Code: "not_regular_file", Message: "the selected item is not a regular file", Recoverable: false,
		}
	}
	if st.Size() == 0 {
		return nil, "", &protocol.Error{
			Code: "decode_failed", Message: "the selected file is empty", Recoverable: false,
		}
	}
	if st.Size() > subscription.MaxSubscriptionResponseSize {
		return nil, "", &protocol.Error{
			Code: "file_too_large",
			Message: "the selected file is larger than the " +
				humanBytes(subscription.MaxSubscriptionResponseSize) + " subscription limit",
			Recoverable: false,
		}
	}

	f, err := os.Open(real)
	if err != nil {
		return nil, "", &protocol.Error{
			Code: "bad_path", Message: "cannot open the selected file: " + err.Error(), Recoverable: false,
		}
	}
	defer func() { _ = f.Close() }()

	// Read through a limit reader rather than trusting the earlier stat: the
	// file could grow between the two calls.
	limited := io.LimitReader(f, subscription.MaxSubscriptionResponseSize+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", &protocol.Error{
			Code: "bad_path", Message: "cannot read the selected file: " + err.Error(), Recoverable: true,
		}
	}
	if int64(len(raw)) > subscription.MaxSubscriptionResponseSize {
		return nil, "", &protocol.Error{
			Code: "file_too_large",
			Message: "the selected file is larger than the " +
				humanBytes(subscription.MaxSubscriptionResponseSize) + " subscription limit",
			Recoverable: false,
		}
	}

	return raw, filepath.Base(real), nil
}

// subscriptionNameFromFilename derives a display name from the file name.
//
// The extension is dropped because it carries no meaning here — the parser
// decides the format, not the suffix — and "MyNodes.json" is a worse label than
// "MyNodes".
func subscriptionNameFromFilename(filename string) string {
	base := strings.TrimSpace(filename)
	if base == "" {
		return "Local Subscription"
	}
	if ext := filepath.Ext(base); ext != "" {
		if trimmed := strings.TrimSuffix(base, ext); strings.TrimSpace(trimmed) != "" {
			base = trimmed
		}
	}
	return base
}

// warningSuffix renders the first few parser warnings into an error message, so
// a refusal explains itself instead of saying only "no nodes".
func warningSuffix(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	sample := warnings
	if len(sample) > importWarningSampleCap {
		sample = sample[:importWarningSampleCap]
	}
	return ": " + strings.Join(sample, "; ")
}

// errText renders an error, or "" when there is none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// humanBytes renders a byte count for a message.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.Itoa(n>>20) + " MB"
	case n >= 1<<10:
		return strconv.Itoa(n>>10) + " KB"
	default:
		return strconv.Itoa(n) + " bytes"
	}
}
