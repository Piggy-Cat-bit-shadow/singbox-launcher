package subscription

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"singbox-launcher/internal/debuglog"
)

// looksLikeJSON reports whether a body starts like JSON.
//
// Used only to decide whether the structured classification matters; the
// classifier itself performs the real validation.
func looksLikeJSON(trimmed string) bool {
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// tryDecodeBase64 attempts to decode base64 string using multiple encoding variants
// Returns decoded bytes and source description, or error if all attempts fail.
// Thin wrapper over the shared DecodeBase64Multi helper (encoding_utils.go).
func tryDecodeBase64(s string) ([]byte, string, error) {
	return DecodeBase64Multi(s)
}

// DecodeSubscriptionContent декодирует содержимое подписки (base64 или plain text).
// Возвращает декодированные байты или оригинальный контент, если это уже готовые ссылки.
func DecodeSubscriptionContent(content []byte) ([]byte, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("subscription content is empty")
	}

	contentStr := strings.TrimSpace(string(content))
	// If content is only whitespace, return original content (not an error)
	if contentStr == "" {
		return content, nil
	}

	// Try to decode as base64
	decoded, source, err := tryDecodeBase64(contentStr)
	if err == nil {
		// Validate decoded content
		if len(decoded) == 0 {
			return nil, fmt.Errorf("decoded content is empty")
		}

		if !utf8.Valid(decoded) {
			return nil, fmt.Errorf("decoded content contains invalid UTF-8 sequences")
		}

		// Count nodes (lines)
		decodedStr := string(decoded)
		lineCount := strings.Count(decodedStr, "\n")
		if lineCount == 0 || !strings.HasSuffix(decodedStr, "\n") {
			lineCount++ // Count last line if no newline or doesn't end with newline
		}

		debuglog.DebugLog("DecodeSubscriptionContent: %s: successfully decoded: %d node(s)", source, lineCount)
		return decoded, nil
	}

	trimmed := strings.TrimSpace(contentStr)

	// Structured bodies pass through untouched for the parser to handle.
	//
	// The decoder's job is to unwrap base64 and reject the obviously invalid —
	// NOT to decide which formats exist. It previously rejected every body
	// starting with '{' before the parser saw it, so a subscription returning a
	// complete sing-box config was refused even though the importer supports
	// exactly that shape. ClassifySubscriptionBody is the single source of truth
	// for formats (its own doc comment records this divergence), so the decoder
	// asks it rather than second-guessing it.
	//
	// The kinds below are the ones that only the structured importer can read. A
	// URI list keeps falling through to the plain-text branch, and anything the
	// classifier does not recognise also lands there — which is why unknown JSON
	// is rejected explicitly just below rather than passed through.
	// Ask the classifier FIRST, before any local heuristic. It handles the
	// overlapping prefixes that a "starts with '['" test cannot: a wg-quick body
	// begins with "[Interface]", which looks like a JSON array but is an INI
	// file. An earlier version of this code checked the prefix itself and
	// rejected valid WireGuard configs as malformed JSON — the same
	// "decoder is narrower than the parser" mistake, one format over.
	switch kind := ClassifySubscriptionBody(trimmed); kind {
	case BodyKindSingboxOutbound, BodyKindSingboxOutboundArray,
		BodyKindSingboxConfig, BodyKindSingboxConfigArray,
		BodyKindXrayConfig, BodyKindXrayArray:
		debuglog.DebugLog("DecodeSubscriptionContent: structured body (%s) passed through to the parser", kind)
		return []byte(trimmed), nil
	case BodyKindWGConf:
		debuglog.DebugLog("DecodeSubscriptionContent: wg-quick config passed through to the parser")
		return []byte(trimmed), nil
	case BodyKindVPNLink:
		// Not JSON at all, but the classifier checks it first and the parser
		// owns it.
		return []byte(trimmed), nil
	}

	// Only now consider the JSON-looking prefixes, and only to give a BETTER
	// error than the generic one at the end. The classifier already declared
	// these bodies unrecognised, so this branch never overrides a supported
	// format.
	if looksLikeJSON(trimmed) {
		var probe interface{}
		if json.Unmarshal([]byte(trimmed), &probe) == nil {
			debuglog.DebugLog("DecodeSubscriptionContent: unrecognised JSON body")
			return nil, fmt.Errorf("unsupported JSON subscription format")
		}
		debuglog.DebugLog("DecodeSubscriptionContent: malformed JSON body")
		return nil, fmt.Errorf("subscription body looks like JSON but is not valid JSON")
	}

	// Check if it's plain text links
	if strings.Contains(contentStr, "://") {
		debuglog.DebugLog("DecodeSubscriptionContent: Detected plain text subscription (contains '://')")
		return content, nil
	}

	return nil, fmt.Errorf("failed to decode base64 content: %w", err)
}
