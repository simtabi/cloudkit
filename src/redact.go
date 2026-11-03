package cloudkit

import (
	"regexp"
	"unicode/utf8"
)

// MaxBodyBytes is how much of a provider's raw response body an error may keep.
const MaxBodyBytes = 512

var (
	// Authorization and similar headers, to the end of the line.
	reAuthHeader = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-auth-token|x-api-key|api-key)(\s*[:=]\s*)[^\r\n]*`)
	// A bearer or basic scheme followed by a credential, wherever it appears.
	reScheme = regexp.MustCompile(`(?i)\b(bearer|basic|token)(\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// Credentials in a query string or form body.
	reQuery = regexp.MustCompile(`(?i)([?&;](?:access_token|token|api_key|apikey|key|secret|password|passwd|client_secret|signature|x-amz-signature|x-amz-credential|x-amz-security-token|sig)=)[^&\s"'#]*`)
	// Userinfo in a URL: scheme://user:password@host.
	reUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
)

// Redact removes credentials from s: Authorization-style headers, bearer and
// basic credentials, credential query parameters and URL userinfo. Every error
// leaves the module through it.
func Redact(s string) string {
	s = reAuthHeader.ReplaceAllString(s, "${1}${2}"+redacted)
	s = reScheme.ReplaceAllString(s, "${1}${2}"+redacted)
	s = reQuery.ReplaceAllString(s, "${1}"+redacted)
	s = reUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	return s
}

// RedactBody redacts a provider's raw response body and keeps at most
// [MaxBodyBytes] of it, cut on a rune boundary.
func RedactBody(b []byte) string {
	s := Redact(string(b))
	if len(s) <= MaxBodyBytes {
		return s
	}
	cut := MaxBodyBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
