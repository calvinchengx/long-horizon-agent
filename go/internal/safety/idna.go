package safety

import (
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// Host encoding for the egress policy.
//
// The Python reference encodes non-ASCII hosts with idna.encode(host, uts46=True): the UTS #46
// mapping (non-transitional, STD3 rules off) and NFC, then per label the IDNA 2008 checks of
// RFC 5891/5892/5893 (PVALID / CONTEXTJ / CONTEXTO code points, hyphen rules, no leading
// combining mark, the Bidi Rule) and Punycode. It detects IDNA 2003/2008 ambiguity by comparing
// that with the stdlib "idna" codec (RFC 3490 ToASCII: nameprep over Unicode 3.2).
//
// This file re-implements both encoders step by step. The per-character UTS #46 mapping and NFC
// come from golang.org/x/net/idna; where the reference's tables differ from x/net/idna's
// (Unicode version drift, nameprep vs transitional UTS #46), the generated idna_overrides.go
// supplies the reference's mapping. The IDNA 2008 label checks, the nameprep prohibited / bidi
// tables and Punycode are ported directly, over tables generated from the reference's data
// (idna_tables.go).

var (
	// uts46Map maps one character (non-transitional, STD3 off); label checks are done here.
	uts46Map = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false),
		idna.CheckHyphens(false), idna.CheckJoiners(false))
	// uts46TransitionalMap maps one character the way IDNA 2003 nameprep does (ß -> ss, ...).
	uts46TransitionalMap = idna.New(idna.MapForLookup(), idna.Transitional(true),
		idna.StrictDomainName(false), idna.CheckHyphens(false), idna.CheckJoiners(false))
	// nfcOnly only normalizes (ValidateLabels installs NFC as the mapping when there is none).
	nfcOnly = idna.New(idna.ValidateLabels(true), idna.CheckHyphens(false), idna.CheckJoiners(false))

	errIDNA = errors.New("idna: invalid domain name")
)

type runeMapping struct {
	out string
	ok  bool
}

type idnaRange struct{ lo, hi rune }

func inRanges(t []idnaRange, r rune) bool {
	lo, hi := 0, len(t)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case r < t[mid].lo:
			hi = mid
		case r > t[mid].hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

type bidiClass uint8

type bidiRange struct {
	lo, hi rune
	class  bidiClass
}

func bidiOf(r rune) bidiClass {
	lo, hi := 0, len(bidiTable)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case r < bidiTable[mid].lo:
			hi = mid
		case r > bidiTable[mid].hi:
			lo = mid + 1
		default:
			return bidiTable[mid].class
		}
	}
	return bidiNone
}

// The x/net/idna calls below are given "0" + text: a leading "0" keeps the text from being read
// as an ACE ("xn--") label, and it neither maps nor composes, so it is simply removed again.
// idna_overrides_gen.go must use the same calls.

// mapRune2008 is idna.uts46_remap for one character (ok=false: disallowed).
func mapRune2008(r rune) (string, bool) {
	if m, ok := idna2008Overrides[r]; ok {
		return m.out, m.ok
	}
	out, err := uts46Map.ToUnicode("0" + string(r))
	if err != nil || !strings.HasPrefix(out, "0") {
		return "", false
	}
	return out[1:], true
}

// mapRune2003 is nameprep's per-character step (B.1, B.2, NFKC) for a Unicode 3.2 character.
// ToUnicode always maps non-transitionally, so the transitional mapping goes through ToASCII and
// back through Punycode. Characters the transitional mapping disallows are left as they are;
// the prohibited-output tables decide about them.
func mapRune2003(r rune) string {
	if m, ok := idna2003Overrides[r]; ok {
		return m
	}
	ascii, err := uts46TransitionalMap.ToASCII("0" + string(r))
	if err != nil {
		return string(r)
	}
	out, err := idna.Punycode.ToUnicode(ascii)
	if err != nil || !strings.HasPrefix(out, "0") {
		return string(r)
	}
	return out[1:]
}

// nfc is unicodedata.normalize("NFC", s). "." never composes, so each dot-separated piece is
// normalized on its own.
func nfc(s string) string {
	if pystr.IsASCII(s) {
		return s
	}
	pieces := strings.Split(s, ".")
	for i, p := range pieces {
		if pystr.IsASCII(p) {
			continue
		}
		out, err := nfcOnly.ToUnicode("0" + p)
		if err == nil && strings.HasPrefix(out, "0") {
			pieces[i] = out[1:]
		}
	}
	return strings.Join(pieces, ".")
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// encodeIDNA2008 is idna.encode(host, uts46=True) for a lower-cased, non-empty host.
func encodeIDNA2008(host string) (string, error) {
	var b strings.Builder
	for _, r := range host {
		out, ok := mapRune2008(r)
		if !ok {
			return "", errIDNA
		}
		b.WriteString(out)
	}
	s := nfc(b.String())
	if runeLen(s) > 254 {
		return "", errIDNA // Domain too long
	}
	labels := splitFunc(s, isIDNA2003Dot)
	if len(labels) == 1 && labels[0] == "" {
		return "", errIDNA // Empty domain
	}
	trailingDot := false
	if labels[len(labels)-1] == "" {
		labels = labels[:len(labels)-1]
		trailingDot = true
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		a, err := alabel(label)
		if err != nil {
			return "", err
		}
		out = append(out, a)
	}
	result := strings.Join(out, ".")
	limit := 253
	if trailingDot {
		result += "."
		limit = 254
	}
	if len(result) > limit {
		return "", errIDNA // Domain too long
	}
	return result, nil
}

// alabel is idna.core.alabel.
func alabel(label string) (string, error) {
	if pystr.IsASCII(label) {
		if err := ulabelASCII(label); err != nil {
			return "", err
		}
		if len(label) > 63 {
			return "", errIDNA
		}
		return label, nil
	}
	if err := checkLabel2008(label); err != nil {
		return "", err
	}
	ace := "xn--" + punycodeEncode(label)
	if len(ace) > 63 {
		return "", errIDNA
	}
	return ace, nil
}

// ulabelASCII is idna.core.ulabel for an ASCII label (only its validation matters here).
func ulabelASCII(label string) error {
	label = strings.ToLower(label)
	if !strings.HasPrefix(label, "xn--") {
		return checkLabel2008(label)
	}
	rest := label[len("xn--"):]
	if rest == "" || strings.HasSuffix(rest, "-") {
		return errIDNA
	}
	decoded, ok := punycodeDecode(rest)
	if !ok {
		return errIDNA
	}
	if nfc(decoded) != decoded { // check_nfc
		return errIDNA
	}
	return checkLabel2008(decoded)
}

// checkLabel2008 is idna.core.check_label; labels reaching it are already NFC (the remap output
// is normalized, decoded A-labels are checked by ulabelASCII).
func checkLabel2008(label string) error {
	rs := []rune(label)
	if len(rs) == 0 {
		return errIDNA // Empty Label
	}
	// check_hyphen_ok
	if len(rs) >= 4 && rs[2] == '-' && rs[3] == '-' {
		return errIDNA
	}
	if rs[0] == '-' || rs[len(rs)-1] == '-' {
		return errIDNA
	}
	// check_initial_combiner
	if inRanges(unicodeMark, rs[0]) {
		return errIDNA
	}
	for pos, r := range rs {
		switch {
		case inRanges(idnaPVALID, r):
		case inRanges(idnaCONTEXTJ, r):
			ok, err := validContextJ(rs, pos)
			if err != nil || !ok {
				return errIDNA
			}
		case inRanges(idnaCONTEXTO, r):
			if !validContextO(rs, pos) {
				return errIDNA
			}
		default:
			return errIDNA
		}
	}
	return checkBidi(rs)
}

func joiningType(r rune) byte {
	switch {
	case inRanges(joiningTypeL, r):
		return 'L'
	case inRanges(joiningTypeD, r):
		return 'D'
	case inRanges(joiningTypeR, r):
		return 'R'
	case inRanges(joiningTypeT, r):
		return 'T'
	}
	return 0
}

var errUnknownCharacter = errors.New("Unknown character in unicodedata")

func isVirama(r rune) (bool, error) {
	if inRanges(viramaCombining, r) {
		return true, nil
	}
	if inRanges(unicodeDataUnnamed, r) {
		return false, errUnknownCharacter
	}
	return false, nil
}

// validContextJ is idna.core.valid_contextj (RFC 5892 Appendix A.1 / A.2).
func validContextJ(rs []rune, pos int) (bool, error) {
	switch rs[pos] {
	case 0x200C:
		if pos > 0 {
			v, err := isVirama(rs[pos-1])
			if err != nil {
				return false, err
			}
			if v {
				return true, nil
			}
		}
		ok := false
		for i := pos - 1; i >= 0; i-- {
			jt := joiningType(rs[i])
			if jt == 'T' {
				continue
			}
			ok = jt == 'L' || jt == 'D'
			break
		}
		if !ok {
			return false, nil
		}
		for i := pos + 1; i < len(rs); i++ {
			jt := joiningType(rs[i])
			if jt == 'T' {
				continue
			}
			return jt == 'R' || jt == 'D', nil
		}
		return false, nil
	case 0x200D:
		if pos > 0 {
			return isVirama(rs[pos-1])
		}
		return false, nil
	}
	return false, nil
}

// validContextO is idna.core.valid_contexto (RFC 5892 Appendix A.3 - A.9).
func validContextO(rs []rune, pos int) bool {
	r := rs[pos]
	switch {
	case r == 0x00B7:
		return pos > 0 && pos < len(rs)-1 && rs[pos-1] == 0x006C && rs[pos+1] == 0x006C
	case r == 0x0375:
		if pos < len(rs)-1 && len(rs) > 1 {
			return inRanges(scriptGreek, rs[pos+1])
		}
		return false
	case r == 0x05F3 || r == 0x05F4:
		if pos > 0 {
			return inRanges(scriptHebrew, rs[pos-1])
		}
		return false
	case r == 0x30FB:
		for _, c := range rs {
			if c == 0x30FB {
				continue
			}
			if inRanges(scriptHiragana, c) || inRanges(scriptKatakana, c) || inRanges(scriptHan, c) {
				return true
			}
		}
		return false
	case 0x660 <= r && r <= 0x669:
		for _, c := range rs {
			if 0x6F0 <= c && c <= 0x6F9 {
				return false
			}
		}
		return true
	case 0x6F0 <= r && r <= 0x6F9:
		for _, c := range rs {
			if 0x660 <= c && c <= 0x669 {
				return false
			}
		}
		return true
	}
	return false
}

func bidiIn(c bidiClass, set ...bidiClass) bool {
	for _, s := range set {
		if c == s {
			return true
		}
	}
	return false
}

// checkBidi is idna.core.check_bidi (RFC 5893), applied only to labels with RTL characters.
func checkBidi(rs []rune) error {
	bidiLabel := false
	for _, r := range rs {
		d := bidiOf(r)
		if d == bidiNone {
			return errIDNA // unknown directionality
		}
		if bidiIn(d, bidiR, bidiAL, bidiAN) {
			bidiLabel = true
		}
	}
	if !bidiLabel {
		return nil
	}
	var rtl bool
	switch d := bidiOf(rs[0]); {
	case d == bidiR || d == bidiAL:
		rtl = true
	case d == bidiL:
		rtl = false
	default:
		return errIDNA
	}
	validEnding := false
	var numberType bidiClass
	haveNumber := false
	for _, r := range rs {
		d := bidiOf(r)
		if rtl {
			if !bidiIn(d, bidiR, bidiAL, bidiAN, bidiEN, bidiES, bidiCS, bidiET, bidiON, bidiBN, bidiNSM) {
				return errIDNA
			}
			if bidiIn(d, bidiR, bidiAL, bidiEN, bidiAN) {
				validEnding = true
			} else if d != bidiNSM {
				validEnding = false
			}
			if bidiIn(d, bidiAN, bidiEN) {
				if !haveNumber {
					numberType, haveNumber = d, true
				} else if numberType != d {
					return errIDNA
				}
			}
		} else {
			if !bidiIn(d, bidiL, bidiEN, bidiES, bidiCS, bidiET, bidiON, bidiBN, bidiNSM) {
				return errIDNA
			}
			if bidiIn(d, bidiL, bidiEN) {
				validEnding = true
			} else if d != bidiNSM {
				validEnding = false
			}
		}
	}
	if !validEnding {
		return errIDNA
	}
	return nil
}

func isIDNA2003Dot(r rune) bool { return r == '.' || r == 0x3002 || r == 0xFF0E || r == 0xFF61 }

// encodeIDNA2003 is host.encode("idna") (the stdlib codec: RFC 3490 ToASCII per label, with
// unassigned code points allowed and without STD3 rules) for a lower-cased, non-ASCII host.
func encodeIDNA2003(host string) (string, bool) {
	labels := splitFunc(host, isIDNA2003Dot)
	trailingDot := false
	if len(labels) > 0 && labels[len(labels)-1] == "" {
		trailingDot = true
		labels = labels[:len(labels)-1]
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		ace, ok := toASCII2003(label)
		if !ok {
			return "", false
		}
		out = append(out, ace)
	}
	result := strings.Join(out, ".")
	if trailingDot {
		result += "."
	}
	return result, true
}

// splitFunc is re.split on a single-character class: it keeps empty fields.
func splitFunc(s string, sep func(rune) bool) []string {
	var out []string
	start := 0
	for i, r := range s {
		if sep(r) {
			out = append(out, s[start:i])
			start = i + utf8.RuneLen(r)
		}
	}
	return append(out, s[start:])
}

func labelLengthOK(n int) bool { return n > 0 && n < 64 }

// toASCII2003 is encodings.idna.ToASCII.
func toASCII2003(label string) (string, bool) {
	if pystr.IsASCII(label) {
		return label, labelLengthOK(len(label))
	}
	prepped, ok := nameprep(label)
	if !ok {
		return "", false
	}
	if pystr.IsASCII(prepped) {
		return prepped, labelLengthOK(len(prepped))
	}
	if strings.HasPrefix(prepped, "xn--") {
		return "", false
	}
	ace := "xn--" + punycodeEncode(prepped)
	return ace, labelLengthOK(len(ace))
}

// nameprep is encodings.idna.nameprep: B.1 / B.2 mapping and NFKC "over Unicode 3.2", then the
// prohibited-output tables and the RFC 3454 bidi requirements. CPython's ucd_3_2_0.normalize
// only suppresses the decomposition of code points unassigned in 3.2; canonical ordering and
// composition use the current tables. So such code points are copied unmapped into the run that
// is normalized, except the few whose own NFC form differs (they would be decomposed), which
// are kept verbatim between runs.
func nameprep(label string) (string, bool) {
	var b, run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(nfc(run.String()))
			run.Reset()
		}
	}
	for _, r := range label {
		switch {
		case inRanges(assignedUnicode32, r):
			run.WriteString(mapRune2003(r))
		case nfc(string(r)) == string(r):
			run.WriteRune(r)
		default:
			flush()
			b.WriteRune(r)
		}
	}
	flush()
	out := []rune(b.String())
	randAL, lcat := false, false
	for _, r := range out {
		if inRanges(stringprepProhibited, r) {
			return "", false
		}
		if inRanges(stringprepRandAL, r) {
			randAL = true
		}
		if inRanges(stringprepL, r) {
			lcat = true
		}
	}
	if randAL {
		if lcat || !inRanges(stringprepRandAL, out[0]) || !inRanges(stringprepRandAL, out[len(out)-1]) {
			return "", false
		}
	}
	return string(out), true
}

// punycodeDecode is Python's punycode codec decode (strict) of an ASCII label.
func punycodeDecode(text string) (string, bool) {
	var base []rune
	extended := text
	if pos := strings.LastIndexByte(text, '-'); pos >= 0 {
		base = []rune(text[:pos])
		extended = text[pos+1:]
	}
	extended = strings.ToUpper(extended)
	threshold := func(j, bias int) int {
		res := 36*(j+1) - bias
		if res < 1 {
			return 1
		}
		if res > 26 {
			return 26
		}
		return res
	}
	const huge = 1 << 40 // any delta this large can only end in an error
	char, pos, bias, extpos := 0x80, -1, 72, 0
	for extpos < len(extended) {
		first := extpos == 0
		// decode_generalized_number
		result, w, j := 0, 1, 0
		for {
			if extpos >= len(extended) {
				return "", false // incomplete punicode string
			}
			c := extended[extpos]
			extpos++
			var digit int
			switch {
			case c >= 'A' && c <= 'Z':
				digit = int(c - 'A')
			case c >= '0' && c <= '9':
				digit = int(c) - 22
			default:
				return "", false // invalid extended code point
			}
			t := threshold(j, bias)
			result += digit * w
			if result > huge {
				return "", false
			}
			if digit < t {
				break
			}
			w *= 36 - t
			if w > huge {
				return "", false
			}
			j++
		}
		delta := result
		pos += delta + 1
		char += pos / (len(base) + 1)
		if char > 0x10FFFF {
			return "", false
		}
		pos %= len(base) + 1
		base = append(base[:pos], append([]rune{rune(char)}, base[pos:]...)...)
		bias = punycodeAdapt(delta, first, len(base))
	}
	return string(base), true
}

// punycodeAdapt is the RFC 3492 bias adaptation (encodings.punycode.adapt).
func punycodeAdapt(delta int, first bool, numPoints int) int {
	if first {
		delta /= 700
	} else {
		delta /= 2
	}
	delta += delta / numPoints
	divisions := 0
	for delta > 455 {
		delta /= 35
		divisions += 36
	}
	return divisions + 36*delta/(delta+38)
}

// punycodeEncode is RFC 3492 encoding (Python's "punycode" codec).
func punycodeEncode(s string) string {
	const (
		base        = 36
		tmin        = 1
		tmax        = 26
		initialBias = 72
		initialN    = 128
	)
	rs := []rune(s)
	var out []byte
	for _, r := range rs {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	h := b
	if b > 0 {
		out = append(out, '-')
	}
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	n, delta, bias := initialN, 0, initialBias
	for h < len(rs) {
		m := int(^uint(0) >> 1)
		for _, r := range rs {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range rs {
			if int(r) < n {
				delta++
			}
			if int(r) == n {
				q := delta
				for k := base; ; k += base {
					t := k - bias
					if t < tmin {
						t = tmin
					} else if t > tmax {
						t = tmax
					}
					if q < t {
						break
					}
					out = append(out, digit(t+(q-t)%(base-t)))
					q = (q - t) / (base - t)
				}
				out = append(out, digit(q))
				bias = punycodeAdapt(delta, h == b, h+1)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return string(out)
}
