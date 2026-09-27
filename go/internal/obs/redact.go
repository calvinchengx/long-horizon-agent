// Package obs is observability: secret redaction and the trace-event recorder
// (python/src/lha/obs). Every meaningful step emits a TraceEvent; event data is redacted first,
// so secret-looking keys and values never reach logs or the collected trace.
package obs

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// Redacted replaces every masked value.
const Redacted = "***"

// Secret is a string that is always masked when redacted (pydantic's SecretStr). Its String and
// GoString methods also mask it, so it does not leak through fmt.
type Secret string

func (Secret) String() string   { return Redacted }
func (Secret) GoString() string { return Redacted }

// Value returns the secret itself.
func (s Secret) Value() string { return string(s) }

// The patterns below are hand-written matchers for the Python reference's regular expressions
// (python/src/lha/obs/redact.py). Go's regexp has no look-behind, and Python's re gives \s, \w,
// \b and IGNORECASE their Unicode meaning, so each pattern is matched explicitly with CPython's
// semantics: \s is str.isspace, \w is str.isalnum or "_", and a case-insensitive ASCII letter
// also matches the extra forms sre accepts (ı / İ for i, ſ for s, the Kelvin sign for k).

// foldsTo reports whether r matches pattern character c, case-insensitively when fold is set.
func foldsTo(r rune, c byte, fold bool) bool {
	if fold && (c|0x20) >= 'a' && (c|0x20) <= 'z' {
		return pystr.FoldsToASCIILetter(r, c|0x20)
	}
	return r == rune(c)
}

// literalAt matches lit at rs[i:], returning the index after it.
func literalAt(rs []rune, i int, lit string, fold bool) (int, bool) {
	if i+len(lit) > len(rs) {
		return i, false
	}
	for k := 0; k < len(lit); k++ {
		if !foldsTo(rs[i+k], lit[k], fold) {
			return i, false
		}
	}
	return i + len(lit), true
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// spanOf returns the end of the longest run of runes from i satisfying in.
func spanOf(rs []rune, i int, in func(rune) bool) int {
	for i < len(rs) && in(rs[i]) {
		i++
	}
	return i
}

// wordStart is \b before a word character at i.
func wordStart(rs []rune, i int) bool { return i == 0 || !pystr.IsWord(rs[i-1]) }

// matcher tries to match at rs[i]; it returns the match end and the replacement.
type matcher func(rs []rune, i int) (end int, repl string, ok bool)

// sub is re.sub: leftmost non-overlapping matches, scanning the input left to right.
func sub(text string, m matcher) string {
	rs := []rune(text)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if end, repl, ok := m(rs, i); ok {
			b.WriteString(repl)
			i = end
			continue
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

// tokenPrefix builds (?<![A-Za-z0-9])(<prefix><one of variants?><sep>[class]{min,}).
func tokenPrefix(prefix, variants, sep string, class func(rune) bool, min int) matcher {
	return func(rs []rune, i int) (int, string, bool) {
		if i > 0 && isASCIIAlnum(rs[i-1]) {
			return 0, "", false
		}
		j, ok := literalAt(rs, i, prefix, false)
		if !ok {
			return 0, "", false
		}
		if variants != "" {
			if j >= len(rs) || !strings.ContainsRune(variants, rs[j]) {
				return 0, "", false
			}
			j++
		}
		if j, ok = literalAt(rs, j, sep, false); !ok {
			return 0, "", false
		}
		end := spanOf(rs, j, class)
		if end-j < min {
			return 0, "", false
		}
		return end, Redacted, true
	}
}

func classAlnumUnderscoreDash(r rune) bool { return isASCIIAlnum(r) || r == '_' || r == '-' }
func classAlnumUnderscore(r rune) bool     { return isASCIIAlnum(r) || r == '_' }
func classAlnumDash(r rune) bool           { return isASCIIAlnum(r) || r == '-' }
func classDigitUpper(r rune) bool          { return r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' }

// awsKey is (?<![A-Za-z0-9])(AKIA[0-9A-Z]{16})\b.
func awsKey(rs []rune, i int) (int, string, bool) {
	if i > 0 && isASCIIAlnum(rs[i-1]) {
		return 0, "", false
	}
	j, ok := literalAt(rs, i, "AKIA", false)
	if !ok || j+16 > len(rs) {
		return 0, "", false
	}
	for k := j; k < j+16; k++ {
		if !classDigitUpper(rs[k]) {
			return 0, "", false
		}
	}
	end := j + 16
	if end < len(rs) && pystr.IsWord(rs[end]) {
		return 0, "", false // \b
	}
	return end, Redacted, true
}

func notSpaceQuoteCommaSemicolon(r rune) bool {
	return !pystr.IsSpace(r) && r != '"' && r != '\'' && r != ',' && r != ';'
}

// authorizationHeader is
// (?i)\b(authorization\s*[:=]\s*["']?(?:(?:basic|bearer|digest|token)\s+)?)[^\s"',;]+ -> \1***.
func authorizationHeader(rs []rune, i int) (int, string, bool) {
	if !wordStart(rs, i) {
		return 0, "", false
	}
	j, ok := literalAt(rs, i, "authorization", true)
	if !ok {
		return 0, "", false
	}
	j = spanOf(rs, j, pystr.IsSpace)
	if j >= len(rs) || (rs[j] != ':' && rs[j] != '=') {
		return 0, "", false
	}
	j = spanOf(rs, j+1, pystr.IsSpace)
	if j < len(rs) && (rs[j] == '"' || rs[j] == '\'') {
		j++
	}
	for _, scheme := range []string{"basic", "bearer", "digest", "token"} {
		k, ok := literalAt(rs, j, scheme, true)
		if !ok {
			continue
		}
		v := spanOf(rs, k, pystr.IsSpace)
		if v == k {
			break // \s+ needs one space; fall back to no scheme
		}
		if end := spanOf(rs, v, notSpaceQuoteCommaSemicolon); end > v {
			return end, string(rs[i:v]) + Redacted, true
		}
		break
	}
	end := spanOf(rs, j, notSpaceQuoteCommaSemicolon)
	if end == j {
		return 0, "", false
	}
	return end, string(rs[i:j]) + Redacted, true
}

// bearerToken is (?i)\b(bearer)\s+[A-Za-z0-9._~+/\-]+=* -> "\1 ***".
func bearerToken(rs []rune, i int) (int, string, bool) {
	if !wordStart(rs, i) {
		return 0, "", false
	}
	j, ok := literalAt(rs, i, "bearer", true)
	if !ok {
		return 0, "", false
	}
	k := spanOf(rs, j, pystr.IsSpace)
	if k == j {
		return 0, "", false
	}
	end := spanOf(rs, k, func(r rune) bool {
		switch r {
		case '.', '_', '~', '+', '/', '-', 0x0130, 0x0131, 0x017F, 0x212A: // IGNORECASE extras
			return true
		}
		return isASCIIAlnum(r)
	})
	if end == k {
		return 0, "", false
	}
	end = spanOf(rs, end, func(r rune) bool { return r == '=' })
	return end, string(rs[i:j]) + " " + Redacted, true
}

// urlPassword is (\b[a-z][a-z0-9+.\-]*://[^/\s:@]*:)[^\s/?#]*@ -> \1***@. The password may
// contain "@" (the match runs to the LAST "@" before the host's path / query).
func urlPassword(rs []rune, i int) (int, string, bool) {
	if !(rs[i] >= 'a' && rs[i] <= 'z') || !wordStart(rs, i) {
		return 0, "", false
	}
	j := spanOf(rs, i+1, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '.' || r == '-'
	})
	j, ok := literalAt(rs, j, "://", false)
	if !ok {
		return 0, "", false
	}
	j = spanOf(rs, j, func(r rune) bool { return r != '/' && r != ':' && r != '@' && !pystr.IsSpace(r) })
	if j >= len(rs) || rs[j] != ':' {
		return 0, "", false
	}
	userEnd := j + 1
	run := spanOf(rs, userEnd, func(r rune) bool { return r != '/' && r != '?' && r != '#' && !pystr.IsSpace(r) })
	for at := run - 1; at >= userEnd; at-- {
		if rs[at] == '@' {
			return at + 1, string(rs[i:userEnd]) + Redacted + "@", true
		}
	}
	return 0, "", false
}

var secretValuePatterns = []matcher{
	// Provider keys: Anthropic/OpenAI (sk-...), GitHub, Slack, AWS access key ids, Google.
	tokenPrefix("sk-", "", "", classAlnumUnderscoreDash, 16),
	tokenPrefix("gh", "pousr", "_", isASCIIAlnum, 20),
	tokenPrefix("github_pat_", "", "", classAlnumUnderscore, 20),
	tokenPrefix("xox", "abposr", "-", classAlnumDash, 10),
	awsKey,
	tokenPrefix("AIza", "", "", classAlnumUnderscoreDash, 30),
	// Voyage AI API keys (pa-...).
	tokenPrefix("pa-", "", "", classAlnumUnderscoreDash, 32),
	// "Authorization: <scheme> <credentials>" (Basic/Bearer/Digest/Token, or a bare value).
	authorizationHeader,
	// "Bearer <token>" elsewhere in headers or messages.
	bearerToken,
	// scheme://user:password@host -> scheme://user:***@host (the user may be empty).
	urlPassword,
}

// RedactText masks secret-looking substrings inside free text.
func RedactText(text string) string {
	for _, m := range secretValuePatterns {
		text = sub(text, m)
	}
	return text
}

// secretKeyWords are the unanchored alternatives of the reference's _SECRET_KEY pattern.
var secretKeyWords = []struct{ a, sep, b string }{
	{"api", "_-", "key"},
	{"apikey", "", ""},
	{"secret", "", ""},
	{"passw", "", "d"}, // passw(or)?d
	{"authorization", "", ""},
	{"credential", "", ""},
	{"private", "_-", "key"},
	{"cookie", "", ""},
	{"dsn", "", ""},
}

// secretKeySearch is re.search of
// (?i)api[_-]?key|apikey|secret|passw(or)?d|authorization|credential|private[_-]?key|cookie|dsn|(^|[_-])(token|auth)$
func secretKeySearch(key string) bool {
	rs := []rune(key)
	for i := 0; i <= len(rs); i++ {
		for _, w := range secretKeyWords {
			j, ok := literalAt(rs, i, w.a, true)
			if !ok {
				continue
			}
			if w.a == "passw" {
				if k, ok := literalAt(rs, j, "or", true); ok {
					if _, ok := literalAt(rs, k, "d", true); ok {
						return true
					}
				}
				if _, ok := literalAt(rs, j, "d", true); ok {
					return true
				}
				continue
			}
			if w.b == "" {
				return true
			}
			if j < len(rs) && strings.ContainsRune(w.sep, rs[j]) {
				if _, ok := literalAt(rs, j+1, w.b, true); ok {
					return true
				}
			}
			if _, ok := literalAt(rs, j, w.b, true); ok {
				return true
			}
		}
		// (^|[_-])(token|auth)$ -- $ also matches before a trailing newline.
		if i == 0 && atEnd(rs, 0) {
			return true
		}
		if i < len(rs) && (rs[i] == '_' || rs[i] == '-') && atEnd(rs, i+1) {
			return true
		}
	}
	return false
}

// atEnd reports (token|auth)$ at i.
func atEnd(rs []rune, i int) bool {
	for _, w := range []string{"token", "auth"} {
		if j, ok := literalAt(rs, i, w, true); ok && (j == len(rs) || (j == len(rs)-1 && rs[j] == '\n')) {
			return true
		}
	}
	return false
}

// camelBoundary inserts "_" at each lower/digit -> upper boundary (accessToken -> access_Token).
func camelBoundary(key string) string {
	rs := []rune(key)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && r >= 'A' && r <= 'Z' {
			p := rs[i-1]
			if p >= 'a' && p <= 'z' || p >= '0' && p <= '9' {
				b.WriteByte('_')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsSecretKey reports keys whose values are always masked. "token" only counts as a whole word
// or suffix, so usage counters like input_tokens / max_tokens stay visible.
func IsSecretKey(key string) bool {
	return secretKeySearch(key) || secretKeySearch(camelBoundary(key))
}

// RedactValue recursively redacts value: maps (as with RedactMapping, keys rendered with
// fmt.Sprint when they are not strings), slices and arrays (as []any), strings (RedactText) and
// Secret values (always masked). Other values are returned
// unchanged; []byte is treated as an opaque value, like Python bytes.
func RedactValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case Secret:
		return Redacted
	case string:
		return RedactText(v)
	case []byte:
		return v
	case map[string]any:
		return RedactMapping(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RedactValue(item)
		}
		return out
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.String: // a named string type (a str subclass in Python)
		return RedactText(rv.String())
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := keyString(iter.Key())
			out[key] = redactEntry(key, iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = RedactValue(rv.Index(i).Interface())
		}
		return out
	}
	return value
}

func keyString(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprint(k.Interface())
}

func redactEntry(key string, value any) any {
	if IsSecretKey(key) && value != nil && value != any("") {
		return Redacted
	}
	return RedactValue(value)
}

// RedactMapping returns a copy of data with secret-keyed values masked (unless nil or "") and
// all other values redacted deeply.
func RedactMapping(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = redactEntry(k, v)
	}
	return out
}
