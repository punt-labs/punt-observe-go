package logging

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxRecordBytes = 64 * 1024

var (
	credentialKey = regexp.MustCompile(`(?i)(?:^|[-_.])(?:authorization|proxy[-_]?authorization|(?:api[-_]?|access[-_]?|refresh[-_]?|session[-_]?)token|api[-_]?key|password|passwd|secret|credentials?|cookie|set-cookie)(?:$|[-_.])`)
	keyValue      = regexp.MustCompile(`(?i)(\b(?:[a-z0-9]+[-_]){0,5}(?:authorization|(?:api[-_]?|access[-_]?|refresh[-_]?|session[-_]?)token|api[-_]?key|password|passwd|secret|credentials?|cookie)\b["']?\s*[:=]\s*)(?:\[redacted\]|"[^"\r\n]*"|'[^'\r\n]*'|(?:basic|bearer)\s+[^\s,;]+|[^\s,;]+)`)
	cookieHeader  = regexp.MustCompile(`(?i)(\b(?:set-cookie|cookie):[ \t]*)[^\r\n]*`)
	keyShape      = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[a-z0-9]{16,}|github_pat_[a-z0-9_]{16,}|sk-[a-z0-9_-]{16,})\b`)
	authScheme    = regexp.MustCompile(`(?i)(\b(?:bearer|basic)(?:[\s\x0b\x{85}]+|\\(?:t|x09|x20))+)(?:\[redacted\]|["']?[a-z0-9._~+/%=-]+["']?)`)
	urlToken      = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s\x{85}\x{2028}\x{2029}]*`)
	userInfo      = regexp.MustCompile(`[^\s"'<>/@:]+:[^\s/@]+@[^\s]*`)
	absolutePath  = regexp.MustCompile(`(^|[\s=(:"'])(?:/|~/)[^\s"'<>]+`)
	email         = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	opaqueToken   = regexp.MustCompile(`\S+`)
)

func cleanRecord(text, home string) ([]byte, bool) {
	if text == "" || len(text) >= maxRecordBytes || !utf8.ValidString(text) {
		return nil, false
	}
	for range 8 {
		// Remove URL tokens before inspecting generic credentials; do not expose the host.
		masked := urlToken.ReplaceAllString(text, "[url]")
		masked = cookieHeader.ReplaceAllString(masked, "${1}[redacted]")
		masked = keyValue.ReplaceAllString(masked, "${1}[redacted]")
		masked = authScheme.ReplaceAllString(masked, "${1}[redacted]")
		masked = keyShape.ReplaceAllString(masked, "[redacted]")
		masked = userInfo.ReplaceAllString(masked, "[userinfo]")
		masked = email.ReplaceAllString(masked, "[email]")
		if home != "" && home != "/" {
			masked = strings.ReplaceAll(masked, home, "~")
		}
		masked = absolutePath.ReplaceAllString(masked, "${1}[path]")
		masked = opaqueToken.ReplaceAllStringFunc(masked, func(token string) string {
			if len(token) > 2048 {
				return "[opaque]"
			}
			return token
		})
		if masked == text {
			escaped := Escape(masked)
			if len(escaped)+1 > maxRecordBytes {
				return nil, false
			}
			return append([]byte(escaped), '\n'), true
		}
		text = masked
	}
	return nil, false
}

// Escape encodes control and formatting runes before inserting untrusted metadata into a record.
func Escape(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r < 0x20 || r >= 0x7f && r <= 0x9f:
			_, _ = fmt.Fprintf(&out, "\\x%02x", r)
		case r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r):
			_, _ = fmt.Fprintf(&out, "\\u%04x", r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
