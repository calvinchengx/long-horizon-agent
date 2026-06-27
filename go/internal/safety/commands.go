package safety

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// This file is the Go port of python/src/lha/safety/commands.py. The classifier is the security
// boundary, so the port is deliberately literal: every helper mirrors one Python function, string
// operations go through pystr (CPython's str semantics), and the reason strings are byte-identical
// (spec/safety/classify_command.json pins them).
//
// ClassifyCommand returns a short reason when a command must not run without a human decision
// (it is routed through a HITL gate by the dispatcher). It covers publishing / pushing,
// history-destroying git operations and git config that defines what git runs or where it pushes,
// deploy / infra CLIs, network uploads and remote shells, deletes outside the workspace and any
// mutation of .git / .lha (including shell redirections and find -delete), and privilege
// escalation. It unwraps common launchers (env, nohup, timeout, setsid, flock, watch, uv run,
// npx, python -m, ...), find -exec and sh -c / bash -c script strings, and fails closed on a
// command name that comes from a shell expansion. It is a guardrail, not a sandbox.

// ClassifyCommand reports why argv needs a human decision (irreversible / outward-facing).
// gated is false (and reason "") when the command may run without approval.
func ClassifyCommand(argv []string) (reason string, gated bool) {
	reason = classifySimple(append([]string(nil), argv...), 0)
	return reason, reason != ""
}

type stringSet map[string]struct{}

func setOf(items ...string) stringSet {
	s := make(stringSet, len(items))
	for _, it := range items {
		s[it] = struct{}{}
	}
	return s
}

func (s stringSet) has(v string) bool { _, ok := s[v]; return ok }

var protectedDirs = setOf(".git", ".lha")

var runners = map[string][]string{ // launcher -> subcommands meaning "run the command that follows"
	"uv":     {"run"},
	"poetry": {"run"},
	"pipenv": {"run"},
	"pdm":    {"run"},
	"hatch":  {"run"},
	"pnpm":   {"exec", "dlx"},
	"yarn":   {"exec", "dlx"},
	"bundle": {"exec"},
	"pipx":   {"run"},
}

// optSpec describes how a launcher parses its own options before the wrapped command.
//
// flags / values: short option letters without / with a value; longFlags / longValues: the same
// for --long options; attached: short letters whose (optional) value may only be attached
// (xargs -i{}); scripts: options whose value is itself a shell command line (env -S, npx -c).
// Unknown options are treated as "maybe takes a value" and BOTH readings are classified.
type optSpec struct {
	flags, values         string
	longFlags, longValues stringSet
	attached              string
	scripts               stringSet
	assignments           bool // env: NAME=value tokens before the command
	numeric               bool // nice: legacy -10
	positional            int  // timeout: the duration
}

var wrappers = map[string]*optSpec{
	"env": {
		flags: "i0v", values: "uCS",
		longFlags:   setOf("--ignore-environment", "--null", "--debug"),
		longValues:  setOf("--unset", "--chdir", "--split-string"),
		scripts:     setOf("-S", "--split-string"),
		assignments: true,
	},
	"nohup": {},
	"nice":  {values: "n", longValues: setOf("--adjustment"), numeric: true},
	"time": {
		flags: "pvaq", values: "fo",
		longFlags:  setOf("--portability", "--verbose", "--append", "--quiet"),
		longValues: setOf("--format", "--output"),
	},
	"command": {flags: "pvV"},
	"exec":    {flags: "cl", values: "a"},
	"stdbuf":  {values: "ioe", longValues: setOf("--input", "--output", "--error")},
	"ionice": {
		flags: "t", values: "cnpPu",
		longFlags:  setOf("--ignore"),
		longValues: setOf("--class", "--classdata", "--pid", "--pgid", "--uid"),
	},
	"timeout": {
		flags: "v", values: "sk",
		longFlags:  setOf("--preserve-status", "--foreground", "--verbose"),
		longValues: setOf("--signal", "--kill-after"),
		positional: 1,
	},
	"xargs": {
		flags: "0rtpxo", values: "IanLPdEs",
		longFlags: setOf("--null", "--no-run-if-empty", "--verbose", "--interactive", "--exit",
			"--open-tty", "--show-limits", "--replace", "--eof", "--max-lines"),
		longValues: setOf("--arg-file", "--delimiter", "--max-args", "--max-procs", "--max-chars",
			"--process-slot-var"),
		attached: "eil",
	},
	"npx": {
		flags: "yq", values: "pc",
		longFlags:  setOf("--yes", "--no", "--quiet", "--no-install", "--ignore-existing"),
		longValues: setOf("--package", "--call"),
		scripts:    setOf("-c", "--call"),
	},
	"bunx":   {values: "p", longFlags: setOf("--bun"), longValues: setOf("--package")},
	"setsid": {flags: "cfw", longFlags: setOf("--ctty", "--fork", "--wait")},
	// flock [opts] FILE CMD... (or flock FILE -c CMD, handled in unwrap).
	"flock": {
		flags: "sxnoFu", values: "wEc",
		longFlags:  setOf("--shared", "--exclusive", "--nonblock", "--close", "--no-fork", "--unlock"),
		longValues: setOf("--wait", "--timeout", "--conflict-exit-code", "--command"),
		scripts:    setOf("-c", "--command"),
		positional: 1,
	},
	"taskset": {flags: "acp", longFlags: setOf("--all-tasks", "--cpu-list", "--pid"), positional: 1},
	"chrt": {
		flags: "abdfiomprRe", values: "TPD",
		longFlags: setOf("--all-tasks", "--batch", "--deadline", "--fifo", "--idle", "--other",
			"--rr"),
		longValues: setOf("--sched-runtime", "--sched-period", "--sched-deadline"),
		positional: 1,
	},
	"unbuffer":   {flags: "p"},
	"caffeinate": {flags: "dimsu", values: "tw"},
	// watch joins its arguments into a sh -c string (see unwrap).
	"watch": {
		flags: "bcdegtwx", values: "nq",
		longFlags: setOf("--beep", "--color", "--differences", "--errexit", "--chgexit",
			"--no-title"),
		longValues: setOf("--interval", "--equexit"),
	},
	"script": {
		flags: "aefqk", values: "cEIOBTm",
		longFlags:  setOf("--append", "--flush", "--quiet"),
		longValues: setOf("--command"),
		scripts:    setOf("-c", "--command"),
	},
	"uvx": {
		flags: "qvn", values: "pf",
		longFlags: setOf("--quiet", "--verbose", "--offline", "--isolated", "--no-cache"),
		longValues: setOf("--from", "--with", "--with-editable", "--with-requirements", "--python",
			"--index", "--index-url", "--extra-index-url", "--find-links", "--directory", "--project",
			"--cache-dir", "--config-file", "--color"),
	},
}

// Options of the "<runner> run" style launchers (unknown ones are still handled fail-closed).
var runnerOpts = &optSpec{
	flags: "qv", values: "pf",
	longFlags: setOf("--quiet", "--verbose", "--offline", "--isolated", "--frozen", "--locked",
		"--no-sync", "--all-extras", "--no-dev", "--all-packages", "--all-groups", "--exact",
		"--no-progress", "--refresh", "--reinstall", "--compile-bytecode", "--no-editable",
		"--native-tls"),
	longValues: setOf("--with", "--with-editable", "--with-requirements", "--python", "--package",
		"--extra", "--group", "--only-group", "--no-group", "--env-file", "--directory", "--project",
		"--index", "--index-url", "--extra-index-url", "--find-links", "--cache-dir", "--config-file",
		"--color", "--spec", "--pip-args", "--filter", "--dir", "--cwd", "--gemfile"),
}

const maxCandidates = 64

var (
	shells = setOf("sh", "bash", "zsh", "dash", "ksh", "fish")
	priv   = setOf("sudo", "doas", "su", "pkexec")
)

var publish = map[string]stringSet{
	"npm":        setOf("publish", "unpublish", "deprecate", "dist-tag", "owner", "access"),
	"pnpm":       setOf("publish"),
	"yarn":       setOf("publish"),
	"bun":        setOf("publish"),
	"twine":      setOf("upload", "register"),
	"uv":         setOf("publish"),
	"poetry":     setOf("publish"),
	"hatch":      setOf("publish"),
	"flit":       setOf("publish"),
	"pdm":        setOf("publish"),
	"cargo":      setOf("publish", "yank", "owner"),
	"gem":        setOf("push", "yank", "owner"),
	"docker":     setOf("push", "login"),
	"podman":     setOf("push", "login"),
	"mvn":        setOf("deploy", "release:perform"),
	"gradle":     setOf("publish"),
	"dotnet":     setOf("nuget"),
	"helm":       setOf("push", "install", "upgrade", "uninstall", "delete", "rollback"),
	"kubectl":    setOf("apply", "create", "delete", "replace", "patch", "scale", "rollout", "edit", "set", "label", "annotate", "drain", "cordon", "taint", "exec"),
	"terraform":  setOf("apply", "destroy", "import", "taint", "state"),
	"tofu":       setOf("apply", "destroy", "import", "taint", "state"),
	"pulumi":     setOf("up", "update", "destroy", "import", "refresh", "cancel"),
	"fly":        setOf("deploy", "launch", "destroy", "secrets", "scale"),
	"flyctl":     setOf("deploy", "launch", "destroy", "secrets", "scale"),
	"vercel":     setOf("deploy", "--prod", "remove", "rm", "env", "promote"),
	"netlify":    setOf("deploy"),
	"firebase":   setOf("deploy"),
	"serverless": setOf("deploy", "remove"),
	"sls":        setOf("deploy", "remove"),
	"wrangler":   setOf("deploy", "publish", "delete", "secret"),
}

// Tools that are outward-facing for (almost) every subcommand.
var alwaysGated = setOf("aws", "gcloud", "gsutil", "az", "heroku", "gh", "glab", "ansible",
	"ansible-playbook", "ssh", "scp", "sftp", "ftp", "telnet", "nc", "ncat", "netcat", "socat",
	"sendmail", "mail", "mailx", "shutdown", "reboot", "halt", "mkfs", "dd")

var gitAlways = setOf("push", "send-pack", "filter-branch", "filter-repo", "update-ref", "send-email")

// git config keys that define commands git will run, or where it pushes. Setting one (with
// git config or git -c) is gated: an alias can rename any subcommand (alias.p=push).
var gitExecConfig = []string{
	"alias.", "include.", "includeif.", "core.hookspath", "core.fsmonitor", "core.sshcommand",
	"core.gitproxy", "core.pager", "core.editor", "credential.", "protocol.", "remote.", "url.",
	"pushurl", "filter.", "diff.", "merge.", "uploadpack.", "receivepack.",
}

var gitConfigReads = setOf("--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l",
	"get", "list")

var (
	httpie           = setOf("http", "https", "xh", "xhs")
	findExec         = setOf("-exec", "-execdir", "-ok", "-okdir")
	curlUploadFlags  = setOf("-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--data-ascii", "--json", "-F", "--form", "--form-string", "-T", "--upload-file")
	writeMethods     = setOf("POST", "PUT", "PATCH", "DELETE")
	fsMutators       = setOf("rm", "rmdir", "mv", "cp", "ln", "chmod", "chown", "touch", "tee", "truncate", "shred", "sed")
	shellSeparators  = setOf(";", "&&", "||", "|", "&", "\n", "(", ")")
	reservedPrefix   = setOf("{", "}", "!", "if", "then", "else", "elif", "do", "while", "until", "time", "fi", "done")
	curlShortValues  = "AbcCdDeEFHKmoPQrtTuUwxXYyz" // curl short options that consume a value
	curlUploadShort  = "dFT"
	separatorChars   = ";&|()\n"
	publishScriptHit = setOf("publish", "deploy", "release")
)

// errAmbiguous: a launcher command line could not be unwrapped unambiguously (caller fails closed).
type errAmbiguous struct{ msg string }

func (e *errAmbiguous) Error() string { return e.msg }

func base(token string) string {
	t := strings.ReplaceAll(token, "\\", "/")
	if i := strings.LastIndexByte(t, '/'); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSuffix(pystr.Lower(t), ".exe")
}

func tail(xs []string, i int) []string {
	if i >= len(xs) {
		return []string{}
	}
	return xs[i:]
}

func plus(xs []string, v string) []string {
	out := make([]string, len(xs), len(xs)+1)
	copy(out, xs)
	return append(out, v)
}

// partition is Python's str.partition(sep).
func partition(s, sep string) (before string, found bool, after string) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], true, s[i+len(sep):]
	}
	return s, false, ""
}

type reading struct{ scripts, command []string }

type frame struct {
	i       int
	scripts []string
}

// parseOpts splits rest into (scripts, command) readings, branching on unknown options.
func parseOpts(spec *optSpec, rest []string) ([]reading, error) {
	var readings []reading
	stack := []frame{{0, []string{}}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i, scripts := top.i, top.scripts
		for {
			if len(readings)+len(stack) > maxCandidates {
				return nil, &errAmbiguous{"too many ways to read the launcher options"}
			}
			if i >= len(rest) {
				readings = append(readings, reading{scripts, []string{}})
				break
			}
			token := rest[i]
			if token == "--" {
				readings = append(readings, reading{scripts, tail(rest, i+1+spec.positional)})
				break
			}
			if spec.assignments && !strings.HasPrefix(token, "-") && strings.Contains(token, "=") {
				i++
				continue
			}
			if token == "-" && spec.assignments { // env - == env -i
				i++
				continue
			}
			if !strings.HasPrefix(token, "-") || token == "-" {
				readings = append(readings, reading{scripts, tail(rest, i+spec.positional)})
				break
			}
			if spec.numeric && pystr.IsDigitString(token[1:]) {
				i++
				continue
			}
			if strings.HasPrefix(token, "--") {
				name, hasEq, value := partition(token, "=")
				switch {
				case hasEq:
					if spec.scripts.has(name) {
						scripts = plus(scripts, value)
					}
					i++
				case spec.longFlags.has(name):
					i++
				case spec.longValues.has(name):
					if spec.scripts.has(name) && i+1 < len(rest) {
						scripts = plus(scripts, rest[i+1])
					}
					i += 2
				default: // unknown: maybe a flag, maybe takes the next token
					stack = append(stack, frame{i + 2, scripts})
					i++
				}
				continue
			}
			// short option bundle, e.g. -iu FOO / -n10 / -sd
			step := 1
			rs := []rune(token)
			for pos := 1; pos < len(rs); pos++ {
				ch := rs[pos]
				attachedValue := string(rs[pos+1:])
				if strings.ContainsRune(spec.attached, ch) {
					break
				}
				if strings.ContainsRune(spec.values, ch) {
					if spec.scripts.has("-" + string(ch)) {
						if attachedValue != "" {
							scripts = plus(scripts, attachedValue)
						} else if i+1 < len(rest) {
							scripts = plus(scripts, rest[i+1])
						}
					}
					if attachedValue != "" {
						step = 1
					} else {
						step = 2
					}
					break
				}
				if strings.ContainsRune(spec.flags, ch) {
					continue
				}
				// unknown letter: fail closed by also reading it as value-taking
				next := i + 2
				if attachedValue != "" {
					next = i + 1
				}
				stack = append(stack, frame{next, scripts})
				break
			}
			i += step
		}
	}
	return readings, nil
}

// unwrap returns every plausible "real command" hidden behind launchers.
func unwrap(argv []string, depth int) ([][]string, error) {
	if depth > 8 {
		return nil, &errAmbiguous{"too many nested launchers"}
	}
	if len(argv) == 0 {
		return [][]string{argv}, nil
	}
	head := base(argv[0])
	rest := argv[1:]
	spec := wrappers[head]
	if spec == nil {
		if subs, ok := runners[head]; ok && len(rest) > 0 && contains(subs, rest[0]) {
			spec, rest = runnerOpts, rest[1:]
		}
	}
	if spec == nil && strings.HasPrefix(head, "python") && len(rest) >= 2 && rest[0] == "-m" {
		return unwrap(rest[1:], depth+1)
	}
	if spec == nil {
		return [][]string{argv}, nil
	}
	readings, err := parseOpts(spec, rest)
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, rd := range readings {
		for _, script := range rd.scripts {
			out = append(out, []string{"sh", "-c", script})
		}
		command := rd.command
		if head == "flock" && len(command) > 0 && (command[0] == "-c" || command[0] == "--command") {
			if len(command) > 1 {
				out = append(out, []string{"sh", "-c", command[1]})
			}
			continue
		}
		if head == "watch" && len(command) > 0 { // run as sh -c "<args joined>"
			out = append(out, []string{"sh", "-c", strings.Join(command, " ")})
		}
		inner, err := unwrap(command, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, inner...)
		if len(out) > maxCandidates {
			return nil, &errAmbiguous{"too many ways to read the launcher options"}
		}
	}
	return out, nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// normpath is posixpath.normpath.
func normpath(path string) string {
	if path == "" {
		return "."
	}
	initial := ""
	if strings.HasPrefix(path, "/") {
		initial = "/"
		if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
			initial = "//"
		}
	}
	var comps []string
	for _, comp := range strings.Split(path, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if comp != ".." || (initial == "" && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, comp)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := initial + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// escapes: absolute, home-relative, or parent-escaping paths (and bare globs of cwd).
func escapes(path string) bool {
	unified := strings.ReplaceAll(path, "\\", "/")
	rs := []rune(unified)
	if strings.HasPrefix(unified, "/") || strings.HasPrefix(unified, "~") || (len(rs) > 1 && rs[1] == ':') {
		return true
	}
	n := normpath(unified)
	return n == ".." || strings.HasPrefix(n, "../") || n == "." || n == "*"
}

func touchesProtected(path string) bool {
	n := normpath(strings.ReplaceAll(path, "\\", "/"))
	first, _, _ := partition(n, "/")
	return protectedDirs.has(pystr.Casefold(first))
}

func classifyRm(args []string) string {
	var flags strings.Builder
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			flags.WriteString(strings.TrimLeft(a, "-"))
		}
	}
	recursive := strings.Contains(pystr.Lower(flags.String()), "r") || contains(args, "--recursive")
	for _, target := range args {
		if strings.HasPrefix(target, "-") {
			continue
		}
		if escapes(target) {
			kind := "delete"
			if recursive {
				kind = "recursive delete"
			}
			return fmt.Sprintf("%s outside the workspace: %s", kind, contracts.PyRepr(target))
		}
	}
	return ""
}

func classifyGit(args []string) string {
	// Skip global options such as -C dir / -c k=v; an inline alias could rename any subcommand
	// (git -c alias.p=push p), so defining one is gated (fail closed).
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		opt := args[0]
		config := ""
		switch {
		case (opt == "-c" || opt == "--config-env") && len(args) > 1:
			config = args[1]
		case strings.HasPrefix(opt, "--config-env="):
			_, _, config = partition(opt, "=")
		}
		key, _, _ := partition(config, "=")
		if gitExecConfigKey(key) {
			return fmt.Sprintf("git -c %s (defines what git runs or where it pushes)", key)
		}
		switch opt {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env":
			args = tail(args, 2)
		default:
			args = args[1:]
		}
	}
	if len(args) == 0 {
		return ""
	}
	sub, rest := args[0], args[1:]
	if strings.Contains(sub, "$") {
		return "git subcommand comes from a shell expansion (cannot classify)"
	}
	if gitAlways.has(sub) || strings.HasPrefix(sub, "remote-") {
		return fmt.Sprintf("git %s (outward-facing / rewrites history)", sub)
	}
	if sub == "config" && !anyIn(rest, gitConfigReads) {
		key := ""
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && a != "set" && a != "add" {
				key = a
				break
			}
		}
		if gitExecConfigKey(key) {
			return fmt.Sprintf("git config %s (defines what git runs or where it pushes)", key)
		}
	}
	for _, arg := range rest {
		if strings.HasPrefix(arg, "--upload-pack") || strings.HasPrefix(arg, "--receive-pack") ||
			strings.HasPrefix(arg, "--exec=") || strings.Contains(arg, "ext::") {
			name, _, _ := partition(arg, "=")
			return fmt.Sprintf("git %s %s runs an arbitrary transport command", sub, name)
		}
	}
	if (sub == "clone" || sub == "ls-remote") && contains(rest, "-u") {
		return fmt.Sprintf("git %s -u runs an arbitrary upload-pack command", sub)
	}
	if sub == "reset" && contains(rest, "--hard") {
		return "git reset --hard discards work"
	}
	if sub == "clean" {
		for _, a := range rest {
			if strings.HasPrefix(a, "-") && strings.Contains(strings.TrimLeft(a, "-"), "f") {
				return "git clean -f deletes untracked files"
			}
		}
	}
	if sub == "branch" && gitBranchForceDelete(rest) {
		return "git branch force-delete (discards unmerged work)"
	}
	if sub == "remote" && len(rest) > 0 {
		switch rest[0] {
		case "add", "set-url", "remove", "rm":
			return "git remote reconfiguration"
		}
	}
	if sub == "reflog" && len(rest) > 0 && rest[0] == "expire" {
		return "git reflog expire destroys recovery points"
	}
	if (sub == "rebase" || sub == "commit") && (contains(rest, "--amend") || contains(rest, "--root")) {
		return fmt.Sprintf("git %s rewrites history", sub)
	}
	if (sub == "checkout" || sub == "switch" || sub == "restore") && (contains(rest, "-f") || contains(rest, "--force")) {
		return fmt.Sprintf("git %s --force discards work", sub)
	}
	return ""
}

func anyIn(xs []string, set stringSet) bool {
	for _, x := range xs {
		if set.has(x) {
			return true
		}
	}
	return false
}

func gitExecConfigKey(key string) bool {
	key = pystr.Lower(pystr.Strip(key))
	if key == "" {
		return false
	}
	for _, p := range gitExecConfig {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return strings.HasSuffix(key, ".pushurl")
}

// gitBranchForceDelete: -D, -df/-Df bundles, or -d/--delete together with -f/--force. A plain
// -d only deletes branches that are already merged, so it is not gated.
func gitBranchForceDelete(rest []string) bool {
	var shorts strings.Builder
	for _, a := range rest {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			shorts.WriteString(a[1:])
		}
	}
	s := shorts.String()
	del := strings.Contains(s, "d") || contains(rest, "--delete")
	force := strings.Contains(s, "f") || contains(rest, "--force")
	return strings.Contains(s, "D") || (del && force)
}

// gitConfigEnvName matches ^GIT_CONFIG(?:_PARAMETERS|_COUNT|_KEY_\d+|_VALUE_\d+|_GLOBAL|_SYSTEM)?=
// with re.IGNORECASE (CPython semantics: \d is any Unicode decimal digit, and the letters also
// match their extra case-insensitive forms such as the Kelvin sign or dotless i).
func gitConfigEnvMatch(token string) bool {
	rs := []rune(token)
	pos := 0
	// literal matches pattern text lit (ASCII) case-insensitively at pos.
	literal := func(at int, lit string) (int, bool) {
		for k := 0; k < len(lit); k++ {
			if at >= len(rs) {
				return at, false
			}
			c := lit[k]
			r := rs[at]
			if c >= 'A' && c <= 'Z' {
				if !pystr.FoldsToASCIILetter(r, c|0x20) {
					return at, false
				}
			} else if r != rune(c) {
				return at, false
			}
			at++
		}
		return at, true
	}
	var ok bool
	if pos, ok = literal(pos, "GIT_CONFIG"); !ok {
		return false
	}
	isEq := func(at int) bool { return at < len(rs) && rs[at] == '=' }
	for _, alt := range []string{"_PARAMETERS", "_COUNT", "_KEY_", "_VALUE_", "_GLOBAL", "_SYSTEM"} {
		end, ok := literal(pos, alt)
		if !ok {
			continue
		}
		if alt == "_KEY_" || alt == "_VALUE_" {
			digits := end
			for digits < len(rs) && pystr.IsDecimal(rs[digits]) {
				digits++
			}
			if digits == end {
				continue
			}
			end = digits
		}
		if isEq(end) {
			return true
		}
	}
	return isEq(pos)
}

func injectsGitConfig(tokens []string) string {
	for _, t := range tokens {
		if gitConfigEnvMatch(t) {
			name, _, _ := partition(t, "=")
			return fmt.Sprintf("git config injected via the environment (%s)", name)
		}
	}
	return ""
}

// redirectEnd finds the first match of (?:\d*|&)(?:>>?\|?|<>) in token (re.search) and returns
// the index (in runes) just past it, or -1.
func redirectEnd(rs []rune) int {
	op := func(at int) int {
		if at < len(rs) && rs[at] == '>' {
			at++
			if at < len(rs) && rs[at] == '>' {
				at++
			}
			if at < len(rs) && rs[at] == '|' {
				at++
			}
			return at
		}
		if at+1 < len(rs) && rs[at] == '<' && rs[at+1] == '>' {
			return at + 2
		}
		return -1
	}
	for p := 0; p <= len(rs); p++ {
		d := p
		for d < len(rs) && pystr.IsDecimal(rs[d]) {
			d++
		}
		if end := op(d); end >= 0 {
			return end
		}
		if p < len(rs) && rs[p] == '&' {
			if end := op(p + 1); end >= 0 {
				return end
			}
		}
	}
	return -1
}

// redirectIntoProtected: a shell redirection (> / >> / &> / 2> ...) whose target is .git / .lha.
func redirectIntoProtected(tokens []string) string {
	for i, token := range tokens {
		rs := []rune(token)
		end := redirectEnd(rs)
		if end < 0 {
			continue
		}
		target := string(rs[end:])
		if target == "" && i+1 < len(tokens) {
			target = tokens[i+1]
		}
		if target != "" && touchesProtected(target) {
			return fmt.Sprintf("redirection writes a harness-owned path (%s)", contracts.PyRepr(target))
		}
	}
	return ""
}

// findExecCommands: the commands a find runs via -exec / -execdir / -ok / -okdir.
func findExecCommands(args []string) [][]string {
	var commands [][]string
	for i := 0; i < len(args); i++ {
		if findExec.has(args[i]) {
			end := i + 1
			for end < len(args) && args[end] != ";" && args[end] != "+" && args[end] != "\\;" {
				end++
			}
			commands = append(commands, append([]string{}, args[i+1:end]...))
			i = end
		}
	}
	return commands
}

// httpieDataItem matches ^[A-Za-z0-9_.\[\]-]+(?::=@|:=|=@|=(?!=)|@).
func httpieDataItem(arg string) bool {
	n := 0
	for n < len(arg) {
		c := arg[n]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == '[' || c == ']' || c == '-' {
			n++
			continue
		}
		break
	}
	if n == 0 {
		return false
	}
	r := arg[n:]
	return strings.HasPrefix(r, ":=") || strings.HasPrefix(r, "@") ||
		(strings.HasPrefix(r, "=") && !strings.HasPrefix(r, "=="))
}

func classifyHTTPie(args []string) string {
	for _, a := range args {
		if a == "-f" || a == "--form" || a == "--multipart" || strings.HasPrefix(a, "--raw") {
			return "httpie request with a body"
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if up := pystr.Upper(arg); writeMethods.has(up) {
			return fmt.Sprintf("httpie %s request", up)
		}
		if !strings.Contains(arg, "://") && httpieDataItem(arg) {
			name, _, _ := partition(arg, "=")
			return fmt.Sprintf("httpie request with a body (%s)", name)
		}
	}
	return ""
}

func classifyCurl(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		nxt := ""
		if i+1 < len(args) {
			nxt = args[i+1]
		}
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--") {
			flag, hasEq, value := partition(arg, "=")
			if curlUploadFlags.has(flag) || strings.HasPrefix(flag, "--data") || strings.HasPrefix(flag, "--form") {
				return fmt.Sprintf("curl upload (%s)", flag)
			}
			if flag == "--config" {
				return "curl config file (may contain uploads)"
			}
			if flag == "--request" {
				method := nxt
				if hasEq {
					method = value
				}
				if up := pystr.Upper(method); writeMethods.has(up) {
					return fmt.Sprintf("curl %s request", up)
				}
			}
		} else if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			rs := []rune(arg)
			for pos := 1; pos < len(rs); pos++ {
				ch := rs[pos]
				attached := string(rs[pos+1:])
				if strings.ContainsRune(curlUploadShort, ch) {
					return fmt.Sprintf("curl upload (-%c)", ch)
				}
				if ch == 'K' {
					return "curl config file (may contain uploads)"
				}
				if ch == 'X' {
					method := attached
					if method == "" {
						method = nxt
					}
					if up := pystr.Upper(method); writeMethods.has(up) {
						return fmt.Sprintf("curl %s request", up)
					}
				}
				if strings.ContainsRune(curlShortValues, ch) {
					if attached == "" {
						i++ // the value is the next argument
					}
					break
				}
			}
		}
	}
	return ""
}

func classifyWget(args []string) string {
	for i, arg := range args {
		nxt := ""
		if i+1 < len(args) {
			nxt = args[i+1]
		}
		flag, hasEq, value := partition(arg, "=")
		switch flag {
		case "--post-data", "--post-file", "--body-data", "--body-file":
			return fmt.Sprintf("wget upload (%s)", flag)
		}
		if flag == "--method" {
			method := nxt
			if hasEq {
				method = value
			}
			if writeMethods.has(pystr.Upper(method)) {
				return "wget write request"
			}
		}
		if flag == "-e" || flag == "--execute" {
			command := nxt
			if hasEq {
				command = value
			}
			command = strings.ReplaceAll(pystr.Lower(command), "_", "")
			for _, word := range []string{"post", "body", "method"} {
				if strings.Contains(command, word) {
					return "wget --execute with an upload setting"
				}
			}
		}
	}
	return ""
}

// sedInPlace: -i, -i.bak, bundles like -Ei / -ni, and --in-place[=SUF].
func sedInPlace(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
			for _, ch := range arg[1:] {
				if ch == 'i' {
					return true
				}
				if ch == 'e' || ch == 'f' || ch == 'l' { // value-taking: the rest is the value
					break
				}
			}
		}
	}
	return false
}

func classifySimple(argv []string, depth int) string {
	if injected := injectsGitConfig(argv); injected != "" {
		return injected
	}
	candidates, err := unwrap(append([]string{}, argv...), 0)
	if err != nil {
		var amb *errAmbiguous
		if errors.As(err, &amb) {
			return fmt.Sprintf("ambiguous command line (%s)", amb.msg)
		}
		return fmt.Sprintf("ambiguous command line (%s)", err)
	}
	for _, candidate := range candidates {
		if reason := classifyOne(candidate, depth); reason != "" {
			return reason
		}
	}
	return ""
}

func classifyOne(argv []string, depth int) string {
	if len(argv) == 0 {
		return ""
	}
	tool, args := base(argv[0]), argv[1:]

	if strings.Contains(argv[0], "$") {
		return "command name comes from a shell expansion (cannot classify)"
	}
	if priv.has(tool) {
		return fmt.Sprintf("privilege escalation (%s)", tool)
	}
	if shells.has(tool) {
		for i, arg := range args {
			if arg == "-c" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c")) {
				if i+1 < len(args) {
					return classifyScript(args[i+1], depth+1)
				}
				return ""
			}
		}
		return ""
	}
	if tool == "eval" {
		if len(args) == 0 {
			return ""
		}
		return classifyScript(strings.Join(args, " "), depth+1)
	}
	if alwaysGated.has(tool) {
		return fmt.Sprintf("%s is an outward-facing / destructive tool", tool)
	}
	if tool == "git" {
		return classifyGit(args)
	}
	if subs, ok := publish[tool]; ok {
		for _, a := range args {
			if subs.has(a) {
				return fmt.Sprintf("%s %s (publish / deploy)", tool, a)
			}
		}
		var positional []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				positional = append(positional, a)
			}
		}
		if (tool == "npm" || tool == "yarn" || tool == "pnpm") && len(positional) > 1 && positional[0] == "run" {
			parts := strings.FieldsFunc(positional[1], func(r rune) bool {
				return r == ':' || r == '.' || r == '_' || r == '-'
			})
			if anyIn(parts, publishScriptHit) {
				return fmt.Sprintf("%s run %s (publish / deploy script)", tool, positional[1])
			}
		}
		return ""
	}
	if tool == "curl" {
		return classifyCurl(args)
	}
	if tool == "wget" {
		return classifyWget(args)
	}
	if httpie.has(tool) {
		return classifyHTTPie(args)
	}
	if tool == "find" {
		for _, command := range findExecCommands(args) {
			if reason := classifySimple(command, depth); reason != "" {
				return reason
			}
		}
		if contains(args, "-delete") {
			for _, a := range args {
				if !strings.HasPrefix(a, "-") && touchesProtected(a) {
					return "find -delete removes a harness-owned path"
				}
			}
		}
		return ""
	}
	if tool == "rsync" {
		for _, t := range args {
			if strings.HasPrefix(t, "-") {
				continue
			}
			host, _, _ := partition(t, "/")
			if strings.Contains(host, ":") {
				return "rsync to/from a remote host"
			}
		}
	}
	if tool == "rm" {
		if found := classifyRm(args); found != "" {
			return found
		}
	}
	if fsMutators.has(tool) {
		if tool == "sed" && !sedInPlace(args) {
			return ""
		}
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") && touchesProtected(arg) {
				return fmt.Sprintf("%s mutates a harness-owned path (%s)", tool, contracts.PyRepr(arg))
			}
		}
	}
	return ""
}

func isAssignment(token string) bool {
	name, eq, _ := partition(token, "=")
	if !eq || !pystr.IsAlnumString(strings.ReplaceAll(name, "_", "a")) {
		return false
	}
	first := []rune(name)[0]
	return !pystr.IsDigit(first)
}

var (
	errUnterminatedBacktick = errors.New("unterminated backtick substitution")
	errUnterminatedDollar   = errors.New("unterminated $( substitution")
	errUnterminatedQuote    = errors.New("unterminated quote")
)

// substitutions returns the bodies of command substitutions (backticks, $(...), <(...) / >(...)).
// It honours single quotes and backslash escapes (substitutions still run inside double quotes)
// and fails if a substitution or quote is unterminated.
func substitutions(script string) ([]string, error) {
	var bodies []string
	s := []rune(script)
	n := len(s)
	inSingle, inDouble := false, false
	i := 0
	for i < n {
		ch := s[i]
		if ch == '\\' && !inSingle {
			i += 2
			continue
		}
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && ch == '`':
			end := i + 1
			for end < n && s[end] != '`' {
				if s[end] == '\\' {
					end += 2
				} else {
					end++
				}
			}
			if end >= n {
				return nil, errUnterminatedBacktick
			}
			bodies = append(bodies, string(s[i+1:end]))
			i = end
		case !inSingle && (ch == '$' || ch == '<' || ch == '>') && i+1 < n && s[i+1] == '(':
			depth, end := 1, i+2
			for end < n && depth != 0 {
				if s[end] == '\\' {
					end += 2
					continue
				}
				switch s[end] {
				case '(':
					depth++
				case ')':
					depth--
				}
				end++
			}
			if depth != 0 {
				return nil, errUnterminatedDollar
			}
			body := string(s[i+2 : end-1])
			if ch == '$' && strings.HasPrefix(body, "(") && strings.HasSuffix(body, ")") {
				body = "" // $(( arithmetic ))
			}
			if body != "" {
				bodies = append(bodies, body)
			}
			i = end
			continue
		}
		i++
	}
	if inSingle || inDouble {
		return nil, errUnterminatedQuote
	}
	return bodies, nil
}

func isSeparatorToken(token string) bool {
	if shellSeparators.has(token) {
		return true
	}
	for _, r := range token { // set(token) <= set(";&|()\n") (true for "")
		if !strings.ContainsRune(separatorChars, r) {
			return false
		}
	}
	return true
}

// classifyScript classifies each simple command of a sh -c script string (and its substitutions).
// scriptRedirect finds a redirection and its target in RAW script text (python: _SCRIPT_REDIRECT;
// the tokenizer splits ">|" into ">" and a pipe, so the target is checked before tokenizing).
var scriptRedirect = regexp.MustCompile(`(?:[0-9]+|&)?(?:>>?\|?|<>)[ \t]*("[^"]*"|'[^']*'|[^ \t\n\r\f\v;&|()<>]+)`)

func classifyScript(script string, depth int) string {
	if depth > 3 {
		return "deeply nested shell invocation"
	}
	nested, err := substitutions(script)
	var tokens []string
	if err == nil {
		tokens, err = shellLex(script)
	}
	if err != nil {
		return "unparseable shell script"
	}
	for _, m := range scriptRedirect.FindAllStringSubmatch(script, -1) {
		target := strings.Trim(m[1], "'\"")
		if target != "" && touchesProtected(target) {
			return "redirection writes a harness-owned path (" + contracts.PyRepr(target) + ")"
		}
	}
	for _, body := range nested {
		if reason := classifyScript(body, depth+1); reason != "" {
			return reason
		}
	}
	var segment []string
	for _, token := range append(tokens, ";") {
		if isSeparatorToken(token) {
			if reason := classifySegment(segment, depth); reason != "" {
				return reason
			}
			segment = nil
		} else {
			segment = append(segment, token)
		}
	}
	return ""
}

// classifySegment drops leading reserved words ({, if, do...) and VAR=value prefixes.
func classifySegment(segment []string, depth int) string {
	if blocked := injectsGitConfig(segment); blocked != "" {
		return blocked
	}
	if blocked := redirectIntoProtected(segment); blocked != "" {
		return blocked
	}
	for len(segment) > 0 && (reservedPrefix.has(segment[0]) || isAssignment(segment[0])) {
		segment = segment[1:]
	}
	// Tokens glued to substitutions ($(git / `git) were classified via substitutions.
	if len(segment) == 0 {
		return ""
	}
	return classifySimple(segment, depth)
}
