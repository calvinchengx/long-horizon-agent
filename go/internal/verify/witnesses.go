package verify

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Witnesses: an item's own acceptance checks, written as short strings on the checklist
// (python: lha.verify.witnesses). Each witness names a deterministic check that must pass before
// the item can flip to done:
//
//   - go:TestName / go:TestName@<pkgpattern> — a Go test (default pattern ./...); the check also
//     requires a "--- PASS: TestName" line, so a missing or skipped test FAILS.
//   - pytest:<node id> — uv run pytest -q <node id>.
//   - cmd:<shell command> — sh -c <command>, authored by the operator.
//   - trusted:<name> (alias ci:<name>) — an operator-defined command from LHA_TRUSTED_CHECKS,
//     run OUTSIDE the sandbox by a TrustedRunner.

// WitnessSchemes are the known witness kinds, in the order error messages list them.
var WitnessSchemes = []string{"go", "pytest", "cmd", "trusted", "ci"}

// DefaultGoPackages is the package pattern of a go: witness without "@".
const DefaultGoPackages = "./..."

var (
	goSegmentRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	goPackagesRE  = regexp.MustCompile(`^[A-Za-z0-9_./~+][A-Za-z0-9_./~+-]*$`)
	pytestNodeRE  = regexp.MustCompile(`^[A-Za-z0-9_./:\[\]=,+][A-Za-z0-9_./:\[\]=,+-]*$`)
	trustedNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	whitespaceRE  = regexp.MustCompile(`\s+`)
)

const maxWitnessNameChars = 80

// pyMatch mirrors Python's re.match with a trailing "$": it also matches before one final "\n".
func pyMatch(re *regexp.Regexp, s string) bool {
	return re.MatchString(s) || (strings.HasSuffix(s, "\n") && re.MatchString(s[:len(s)-1]))
}

// UnknownTrustedCheckError is a trusted:/ci: witness naming a command the operator never defined.
type UnknownTrustedCheckError struct{ Message string }

func (e *UnknownTrustedCheckError) Error() string { return e.Message }

// WitnessError is a witness with invalid syntax (python: ValueError).
type WitnessError struct{ Message string }

func (e *WitnessError) Error() string { return e.Message }

func witnessErr(format string, args ...any) error {
	return &WitnessError{Message: fmt.Sprintf(format, args...)}
}

func splitWitness(witness string) (string, string, error) {
	text := pyStrip(witness)
	scheme, rest, ok := strings.Cut(text, ":")
	known := false
	for _, s := range WitnessSchemes {
		known = known || s == scheme
	}
	if !ok || !known {
		listed := make([]string, len(WitnessSchemes))
		for i, s := range WitnessSchemes {
			listed[i] = s + ":"
		}
		return "", "", witnessErr("witness %s: unknown kind; expected one of %s",
			contracts.PyRepr(witness), strings.Join(listed, ", "))
	}
	return scheme, pyStrip(rest), nil
}

func goParts(witness, rest string) (string, string, error) {
	test, packages, _ := strings.Cut(rest, "@")
	test, packages = pyStrip(test), pyStrip(packages)
	if packages == "" {
		packages = DefaultGoPackages
	}
	valid := test != ""
	for _, seg := range strings.Split(test, "/") {
		valid = valid && goSegmentRE.MatchString(seg)
	}
	if !valid {
		return "", "", witnessErr("witness %s: Go test name must be an identifier like TestName or "+
			"TestName/subtest", contracts.PyRepr(witness))
	}
	if !pyMatch(goPackagesRE, packages) {
		return "", "", witnessErr("witness %s: package pattern %s contains characters that are "+
			"not allowed (use an import path or a pattern like ./internal/...)",
			contracts.PyRepr(witness), contracts.PyRepr(packages))
	}
	return test, packages, nil
}

// ValidateWitness checks a witness string's syntax (no trusted map needed).
func ValidateWitness(witness string) error {
	scheme, rest, err := splitWitness(witness)
	if err != nil {
		return err
	}
	switch scheme {
	case "go":
		_, _, err = goParts(witness, rest)
		return err
	case "pytest":
		if !pyMatch(pytestNodeRE, rest) {
			return witnessErr("witness %s: pytest node id must be a path or node id without "+
				"whitespace, shell metacharacters or a leading '-'", contracts.PyRepr(witness))
		}
	case "cmd":
		if rest == "" {
			return witnessErr("witness %s: cmd: needs a shell command", contracts.PyRepr(witness))
		}
	default:
		if !pyMatch(trustedNameRE, rest) {
			return witnessErr("witness %s: %s: needs the name of an operator-defined trusted check",
				contracts.PyRepr(witness), scheme)
		}
	}
	return nil
}

// ShellQuote is Python's shlex.quote.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("@%+=:,./-_", r))) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// GoTestCommand is argv that passes only if `go test` exits 0 AND reports "--- PASS: <test>".
func GoTestCommand(test, packages string) []string {
	if packages == "" {
		packages = DefaultGoPackages
	}
	segs := strings.Split(test, "/")
	for i, s := range segs {
		segs[i] = "^" + s + "$"
	}
	runRegex := strings.Join(segs, "/")
	passRegex := "^[[:space:]]*--- PASS: " + test + "( |$)"
	goCmd := "go test -count=1 -run " + ShellQuote(runRegex) + " -v " + ShellQuote(packages)
	missing := "witness go:" + test + ": the test did not run and pass (missing, skipped or filtered)"
	script := "out=$(" + goCmd + " 2>&1); rc=$?; " +
		`printf "%s\n" "$out"; ` +
		`if [ "$rc" -ne 0 ]; then exit "$rc"; fi; ` +
		`if printf "%s\n" "$out" | grep -Eq ` + ShellQuote(passRegex) + "; then exit 0; fi; " +
		"echo " + ShellQuote(missing) + " >&2; exit 1"
	return []string{"sh", "-c", script}
}

func witnessCheckName(witness string) string {
	name := whitespaceRE.ReplaceAllString(pyStrip(witness), " ")
	if r := []rune(name); len(r) > maxWitnessNameChars {
		return string(r[:maxWitnessNameChars-3]) + "..."
	}
	return name
}

// ParseWitness turns one witness string into a gating Check named after the witness. It returns
// a *WitnessError on bad syntax and an *UnknownTrustedCheckError for an undefined trusted name.
func ParseWitness(witness string, trusted map[string][]string) (contracts.Check, error) {
	if err := ValidateWitness(witness); err != nil {
		return contracts.Check{}, err
	}
	scheme, rest, _ := splitWitness(witness)
	name := witnessCheckName(witness)
	check := contracts.Check{Name: name, Gating: true, Where: "sandbox"}
	switch scheme {
	case "go":
		test, packages, _ := goParts(witness, rest)
		check.Command = GoTestCommand(test, packages)
	case "pytest":
		check.Command = []string{"uv", "run", "pytest", "-q", rest}
	case "cmd":
		check.Command = []string{"sh", "-c", rest}
	default:
		argv := trusted[rest]
		if len(argv) == 0 {
			names := make([]string, 0, len(trusted))
			for k := range trusted {
				names = append(names, k)
			}
			sort.Strings(names)
			known := strings.Join(names, ", ")
			if known == "" {
				known = "(none defined; set LHA_TRUSTED_CHECKS)"
			}
			return contracts.Check{}, &UnknownTrustedCheckError{Message: fmt.Sprintf(
				"witness %s: no trusted check named %s; known: %s",
				contracts.PyRepr(witness), contracts.PyRepr(rest), known)}
		}
		check.Command = append([]string{}, argv...)
		check.Where = "trusted"
	}
	return check, nil
}

// ItemChecks are the item's witnesses as uniquely-named checks, in declaration order.
func ItemChecks(item contracts.ChecklistItem, trusted map[string][]string) ([]contracts.Check, error) {
	checks := []contracts.Check{}
	for _, w := range item.Witnesses {
		c, err := ParseWitness(w, trusted)
		if err != nil {
			return nil, err
		}
		checks = append(checks, c)
	}
	return contracts.EnsureUniqueCheckNames(checks), nil
}

// pyStrip is Python's str.strip() (Unicode whitespace plus \x1c-\x1f).
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }
