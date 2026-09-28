package swiftlogic_test

import (
	"strconv"
	"strings"
	"testing"

	"singbox-launcher/backend/service"
)

// TestSubscriptionURLValidationNeverRejectsWhatTheBackendAccepts covers UI-28.
//
// The Add button's rule was a case-SENSITIVE prefix test while the backend
// lowercases before checking. `HTTPS://example.com/sub` was therefore accepted
// by the backend and left the button permanently disabled — the worst kind of
// validation mismatch, because the user gets no error to read; the control
// simply never becomes available.
//
// BOTH SIDES are exercised here, and the Swift result is compared against the
// Go one. Testing the Swift helper alone would only pin today's behaviour; what
// must hold is that they AGREE, which is the property that was violated.
func TestSubscriptionURLValidationNeverRejectsWhatTheBackendAccepts(t *testing.T) {
	requireHarness(t)

	cases := []string{
		"https://example.com/sub",
		"HTTPS://example.com/sub",
		"Http://example.com/sub",
		"  https://example.com/sub  ",
		"https://example.com/sub\n",
		"http://EXAMPLE.COM/SUB",
		"https://example.com",
		"http://127.0.0.1:8080/x",
	}

	// Build one Swift expression per case and require agreement with Go.
	var b strings.Builder
	for i, c := range cases {
		swiftLiteral := strings.ReplaceAll(c, `\`, `\\`)
		swiftLiteral = strings.ReplaceAll(swiftLiteral, `"`, `\"`)
		swiftLiteral = strings.ReplaceAll(swiftLiteral, "\n", `\n`)
		want := service.LooksLikeURLForTest(c)
		// The assertion NAME is an index, not the URL: the URL is arbitrary text
		// that may contain characters which would end the Swift string literal,
		// and a name that needs escaping is a name that can silently break the
		// generated program instead of reporting a failure.
		b.WriteString("check(\"UI-28 swift agrees with go, case ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(", go=")
		if want {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		b.WriteString("\", SubscriptionURLInput.looksValid(\"")
		b.WriteString(swiftLiteral)
		b.WriteString("\") == ")
		if want {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		b.WriteString(")\n")
	}

	// The regression itself, stated plainly: an uppercase scheme must be usable.
	b.WriteString(`check("UI-28 uppercase scheme is accepted", SubscriptionURLInput.looksValid("HTTPS://example.com/sub"))` + "\n")
	b.WriteString(`check("UI-28 mixed-case scheme is accepted", SubscriptionURLInput.looksValid("HtTpS://example.com/sub"))` + "\n")
	// And the rule must not become so loose that it accepts nonsense.
	b.WriteString(`check("UI-28 a bare host is rejected", !SubscriptionURLInput.looksValid("example.com/sub"))` + "\n")
	b.WriteString(`check("UI-28 ftp is rejected", !SubscriptionURLInput.looksValid("ftp://example.com/sub"))` + "\n")
	b.WriteString(`check("UI-28 empty is rejected", !SubscriptionURLInput.looksValid(""))` + "\n")
	b.WriteString(`check("UI-28 whitespace only is rejected", !SubscriptionURLInput.looksValid("   "))` + "\n")
	b.WriteString(`check("UI-28 a scheme with no host is rejected", !SubscriptionURLInput.looksValid("https://"))` + "\n")

	runSwift(t, b.String())
}
