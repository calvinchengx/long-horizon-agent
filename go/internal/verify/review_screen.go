package verify

// The pre-review diff screen: deterministic rules that spot weakened tests in a diff
// (python: lha.verify.review_screen). The reviewer judges a verified item's base..head diff;
// before it does, this screen reads the same diff for the ways a change can pass the gate by
// weakening it rather than by doing the work: a deleted test file, a removed test, a newly
// skipped test, assertions taken out of a test, or a lowered coverage floor. The findings never
// make an item green or skip the reviewer; they only force the full review when the organization
// would have skipped it, and go to the reviewer as criteria. spec/verify/review_screen.json pins
// the findings for both implementations.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// MaxFindings is how many findings a screen keeps.
const MaxFindings = 20

// ReviewScreenEvent is the anchor event each screened diff is recorded as.
const ReviewScreenEvent = "review_screen"

var (
	testDirs         = map[string]bool{"tests": true, "test": true, "__tests__": true, "spec": true, "testdata": true}
	screenTestFileRE = regexp.MustCompile(`^(test_.*\.py|.*_test\.py|.*_test\.go|.*\.(test|spec)\.[cm]?[jt]sx?|conftest\.py)$`)
	testDefRE        = regexp.MustCompile(`^(?:async\s+)?def\s+(test_\w+)\s*\(|^func\s+(Test\w+)\s*\(|^\s*(?:it|test)\(\s*['"](.+?)['"]`)
	skipRE           = regexp.MustCompile(`@pytest\.mark\.(skip|xfail)|pytest\.(skip|xfail)\(|unittest\.(skip|expectedFailure)|@skip\b` +
		`|\bt\.Skip(?:f|Now)?\(|testing\.Short\(\)|\b(?:it|test|describe)\.skip\(|\bx(?:it|test|describe)\(`)
	assertRE = regexp.MustCompile(`^\s*assert\b|self\.assert\w*\(|\bt\.(Fatal|Error|Fail)\w*\(|\b(require|assert)\.\w+\(|\bexpect\(`)
	floorRE  = regexp.MustCompile(`(fail[_-]under)\s*[=:]\s*(\d+(?:\.\d+)?)`)
)

// IsTestPath reports whether path (repo-relative, /-separated) is a test file.
func IsTestPath(path string) bool {
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		if testDirs[part] {
			return true
		}
	}
	return screenTestFileRE.MatchString(parts[len(parts)-1])
}

func headerPath(header string) string {
	rest := header[4:]
	if i := strings.Index(rest, "\t"); i >= 0 {
		rest = rest[:i]
	}
	rest = pyfmt.PyStrip(rest)
	if rest == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(rest, "a/") || strings.HasPrefix(rest, "b/") {
		return rest[2:]
	}
	return rest
}

type screenedFile struct {
	old, new       string
	removedTests   []string
	addedTests     map[string]bool
	skips          []string
	assertsRemoved int
	assertsAdded   int
	floorOld       *string
	floorNew       *string
	floorKey       string
}

func (f *screenedFile) path() string {
	if f.new != "" {
		return f.new
	}
	return f.old
}

func (f *screenedFile) findings() []string {
	out := []string{}
	if f.old != "" && f.new == "" && IsTestPath(f.old) {
		return []string{"deleted test file " + f.old}
	}
	if f.old == "" && f.new != "" && IsHarnessConfig(f.new) {
		out = append(out, "added harness file "+f.new)
	}
	if IsTestPath(f.path()) {
		for _, name := range f.removedTests {
			if !f.addedTests[name] {
				out = append(out, "removed test "+name+" from "+f.path())
			}
		}
		for _, line := range f.skips {
			out = append(out, "added skip to "+f.path()+": "+line)
		}
		if net := f.assertsRemoved - f.assertsAdded; net > 0 {
			plural := "s"
			if net == 1 {
				plural = ""
			}
			out = append(out, fmt.Sprintf("%d assertion%s removed from %s", net, plural, f.path()))
		}
	}
	if f.floorOld != nil && f.floorNew != nil {
		oldV, err1 := strconv.ParseFloat(*f.floorOld, 64)
		newV, err2 := strconv.ParseFloat(*f.floorNew, 64)
		if err1 == nil && err2 == nil && newV < oldV {
			out = append(out, "lowered "+f.floorKey+" from "+*f.floorOld+" to "+*f.floorNew+" in "+f.path())
		}
	}
	return out
}

func firstGroup(groups []string) string {
	for _, g := range groups[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}

// ScreenDiff is the weakened-test findings in a unified diff (empty when it looks clean).
func ScreenDiff(diff string) []string {
	files := []*screenedFile{}
	var current *screenedFile
	old := ""
	for _, raw := range pySplitLines(diff) {
		if strings.HasPrefix(raw, "--- ") && !strings.HasPrefix(raw, "---  ") {
			old = headerPath(raw)
			continue
		}
		if strings.HasPrefix(raw, "+++ ") {
			current = &screenedFile{old: old, new: headerPath(raw), addedTests: map[string]bool{}}
			files = append(files, current)
			old = ""
			continue
		}
		if current == nil || len(raw) < 2 || (raw[0] != '+' && raw[0] != '-') || strings.HasPrefix(raw, "+++") || strings.HasPrefix(raw, "---") {
			continue
		}
		sign, line := raw[0], raw[1:]
		stripped := pyfmt.PyStrip(line)
		if m := testDefRE.FindStringSubmatch(line); m != nil {
			name := firstGroup(m)
			if sign == '-' {
				current.removedTests = append(current.removedTests, name)
			} else {
				current.addedTests[name] = true
			}
		}
		if sign == '+' && skipRE.MatchString(line) {
			current.skips = append(current.skips, pyfmt.Head(stripped, 120))
		}
		if assertRE.MatchString(line) {
			if sign == '-' {
				current.assertsRemoved++
			} else {
				current.assertsAdded++
			}
		}
		if m := floorRE.FindStringSubmatch(line); m != nil {
			current.floorKey = m[1]
			value := m[2]
			if sign == '-' && current.floorOld == nil {
				current.floorOld = &value
			} else if sign == '+' && current.floorNew == nil {
				current.floorNew = &value
			}
		}
	}
	out := []string{}
	for _, f := range files {
		out = append(out, f.findings()...)
	}
	if len(out) > MaxFindings {
		out = out[:MaxFindings]
	}
	return out
}

// ScreenCriteria is the reviewer's criteria with the screen's findings in front (unchanged when
// there are none).
func ScreenCriteria(findings []string, criteria string) string {
	if len(findings) == 0 {
		return criteria
	}
	lines := make([]string, len(findings))
	for i, f := range findings {
		lines[i] = "- " + f
	}
	return "Pre-review screen: this diff weakens or removes tests. Each of these is BLOCKING " +
		"unless the item itself requires it:\n" + strings.Join(lines, "\n") + "\n\n" + criteria
}

// ReviewScreenEventRecord is the review_screen event: what the screen found and whether it
// forced the review.
func ReviewScreenEventRecord(itemID, base, head string, findings []string, forced bool, cycleID string) contracts.EventRecord {
	list := make([]any, len(findings))
	for i, f := range findings {
		list[i] = f
	}
	return contracts.EventRecord{Kind: ReviewScreenEvent, CycleID: cycleID, Payload: contracts.Payload(
		"item_id", itemID, "base", base, "head", head, "findings", list, "forced", forced,
	)}
}

// pySplitLines mirrors Python's str.splitlines() (the state package keeps the same helper).
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		default:
			i += size
			continue
		}
		end := i + size
		if r == '\r' && end < len(s) && s[end] == '\n' {
			end++
		}
		out = append(out, s[start:i])
		start, i = end, end
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
