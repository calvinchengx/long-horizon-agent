"""Classifier for irreversible / outward-facing shell commands.

``classify_command(argv)`` returns a short reason string when a command must not run without a
human decision (it is routed through a ``HITLGate`` by the dispatcher), else ``None``. It covers:

- publishing / pushing: ``git push``, package publish (npm/pnpm/yarn/twine/uv/poetry/cargo/gem...),
  ``docker push``;
- history-destroying git ops (``reset --hard``, ``clean -f``, ``branch -D``, ``filter-branch``...);
- deploy / infra CLIs (kubectl, helm, terraform, pulumi, aws, gcloud, az, fly, vercel, gh, ...);
- network uploads and remote shells (``curl -d/-F/-T/-X POST``, ``wget --post-*``, scp, rsync to a
  remote, ssh, nc, ...);
- ``rm -r``/``rm`` of absolute, home or parent paths, and any mutation of ``.git``/``.lha``;
- privilege escalation (sudo/doas/su).

It unwraps common launchers (``env``, ``nohup``, ``timeout``, ``uv run``, ``npx``,
``python -m``...) and ``sh -c`` / ``bash -c`` script strings. This is a guardrail, not a sandbox:
an arbitrary program (``python -c ...``) can still do anything its sandbox allows, which is why
network isolation is enforced at the sandbox level (Docker ``network_mode="none"``).
"""

from __future__ import annotations

import posixpath
import shlex
from collections.abc import Sequence

_PROTECTED = frozenset({".git", ".lha"})

_RUNNERS = {  # launcher -> subcommands meaning "run the command that follows"
    "uv": ("run",),
    "poetry": ("run",),
    "pipenv": ("run",),
    "pdm": ("run",),
    "hatch": ("run",),
    "pnpm": ("exec", "dlx"),
    "yarn": ("exec", "dlx"),
    "bundle": ("exec",),
    "pipx": ("run",),
}


class _OptSpec:
    """How a launcher parses its own options before the wrapped command.

    ``flags`` / ``values``: short option letters without / with a value; ``long_flags`` /
    ``long_values``: the same for ``--long`` options; ``attached``: short letters whose (optional)
    value may only be attached (``xargs -i{}``); ``scripts``: options whose value is itself a
    shell command line (``env -S``, ``npx -c``). Unknown options are treated as "maybe takes a
    value" and BOTH readings are classified (fail closed).
    """

    def __init__(
        self,
        flags: str = "",
        values: str = "",
        long_flags: frozenset[str] = frozenset(),
        long_values: frozenset[str] = frozenset(),
        *,
        attached: str = "",
        scripts: frozenset[str] = frozenset(),
        assignments: bool = False,
        numeric: bool = False,
        positional: int = 0,
    ) -> None:
        self.flags = flags
        self.values = values
        self.long_flags = long_flags
        self.long_values = long_values
        self.attached = attached
        self.scripts = scripts
        self.assignments = assignments  # env: NAME=value tokens before the command
        self.numeric = numeric  # nice: legacy ``-10``
        self.positional = positional  # timeout: the duration


_F = frozenset
_WRAPPERS: dict[str, _OptSpec] = {
    "env": _OptSpec(
        "i0v",
        "uCS",
        _F({"--ignore-environment", "--null", "--debug"}),
        _F({"--unset", "--chdir", "--split-string"}),
        scripts=_F({"-S", "--split-string"}),
        assignments=True,
    ),
    "nohup": _OptSpec(),
    "nice": _OptSpec("", "n", _F(), _F({"--adjustment"}), numeric=True),
    "time": _OptSpec(
        "pvaq",
        "fo",
        _F({"--portability", "--verbose", "--append", "--quiet"}),
        _F({"--format", "--output"}),
    ),
    "command": _OptSpec("pvV"),
    "exec": _OptSpec("cl", "a"),
    "stdbuf": _OptSpec("", "ioe", _F(), _F({"--input", "--output", "--error"})),
    "ionice": _OptSpec(
        "t", "cnpPu", _F({"--ignore"}), _F({"--class", "--classdata", "--pid", "--pgid", "--uid"})
    ),
    "timeout": _OptSpec(
        "v",
        "sk",
        _F({"--preserve-status", "--foreground", "--verbose"}),
        _F({"--signal", "--kill-after"}),
        positional=1,
    ),
    "xargs": _OptSpec(
        "0rtpxo",
        "IanLPdEs",
        _F(
            {
                "--null",
                "--no-run-if-empty",
                "--verbose",
                "--interactive",
                "--exit",
                "--open-tty",
                "--show-limits",
                "--replace",
                "--eof",
                "--max-lines",
            }
        ),
        _F(
            {
                "--arg-file",
                "--delimiter",
                "--max-args",
                "--max-procs",
                "--max-chars",
                "--process-slot-var",
            }
        ),
        attached="eil",
    ),
    "npx": _OptSpec(
        "yq",
        "pc",
        _F({"--yes", "--no", "--quiet", "--no-install", "--ignore-existing"}),
        _F({"--package", "--call"}),
        scripts=_F({"-c", "--call"}),
    ),
    "bunx": _OptSpec("", "p", _F({"--bun"}), _F({"--package"})),
    "uvx": _OptSpec(
        "qvn",
        "pf",
        _F({"--quiet", "--verbose", "--offline", "--isolated", "--no-cache"}),
        _F(
            {
                "--from",
                "--with",
                "--with-editable",
                "--with-requirements",
                "--python",
                "--index",
                "--index-url",
                "--extra-index-url",
                "--find-links",
                "--directory",
                "--project",
                "--cache-dir",
                "--config-file",
                "--color",
            }
        ),
    ),
}
# Options of the ``<runner> run`` style launchers (unknown ones are still handled fail-closed).
_RUNNER_OPTS = _OptSpec(
    "qv",
    "pf",
    _F({"--quiet", "--verbose", "--offline", "--isolated", "--frozen", "--locked", "--no-sync"}),
    _F(
        {
            "--with",
            "--with-editable",
            "--with-requirements",
            "--python",
            "--package",
            "--extra",
            "--group",
            "--only-group",
            "--no-group",
            "--env-file",
            "--directory",
            "--project",
            "--index",
            "--index-url",
            "--extra-index-url",
            "--find-links",
            "--cache-dir",
            "--config-file",
            "--color",
            "--spec",
            "--pip-args",
            "--filter",
            "--dir",
            "--cwd",
            "--gemfile",
        }
    ),
)
_MAX_CANDIDATES = 64
_SHELLS = frozenset({"sh", "bash", "zsh", "dash", "ksh", "fish"})
_PRIV = frozenset({"sudo", "doas", "su", "pkexec"})

_PUBLISH: dict[str, frozenset[str]] = {
    "npm": frozenset({"publish", "unpublish", "deprecate", "dist-tag", "owner", "access"}),
    "pnpm": frozenset({"publish"}),
    "yarn": frozenset({"publish"}),
    "bun": frozenset({"publish"}),
    "twine": frozenset({"upload", "register"}),
    "uv": frozenset({"publish"}),
    "poetry": frozenset({"publish"}),
    "hatch": frozenset({"publish"}),
    "flit": frozenset({"publish"}),
    "pdm": frozenset({"publish"}),
    "cargo": frozenset({"publish", "yank", "owner"}),
    "gem": frozenset({"push", "yank", "owner"}),
    "docker": frozenset({"push", "login"}),
    "podman": frozenset({"push", "login"}),
    "mvn": frozenset({"deploy", "release:perform"}),
    "gradle": frozenset({"publish"}),
    "dotnet": frozenset({"nuget"}),
    "helm": frozenset({"push", "install", "upgrade", "uninstall", "delete", "rollback"}),
    "kubectl": frozenset(
        {
            "apply",
            "create",
            "delete",
            "replace",
            "patch",
            "scale",
            "rollout",
            "edit",
            "set",
            "label",
            "annotate",
            "drain",
            "cordon",
            "taint",
            "exec",
        }
    ),
    "terraform": frozenset({"apply", "destroy", "import", "taint", "state"}),
    "tofu": frozenset({"apply", "destroy", "import", "taint", "state"}),
    "pulumi": frozenset({"up", "update", "destroy", "import", "refresh", "cancel"}),
    "fly": frozenset({"deploy", "launch", "destroy", "secrets", "scale"}),
    "flyctl": frozenset({"deploy", "launch", "destroy", "secrets", "scale"}),
    "vercel": frozenset({"deploy", "--prod", "remove", "rm", "env", "promote"}),
    "netlify": frozenset({"deploy"}),
    "firebase": frozenset({"deploy"}),
    "serverless": frozenset({"deploy", "remove"}),
    "sls": frozenset({"deploy", "remove"}),
    "wrangler": frozenset({"deploy", "publish", "delete", "secret"}),
}
# Tools that are outward-facing for (almost) every subcommand.
_ALWAYS_GATED = frozenset(
    {
        "aws",
        "gcloud",
        "gsutil",
        "az",
        "heroku",
        "gh",
        "glab",
        "ansible",
        "ansible-playbook",
        "ssh",
        "scp",
        "sftp",
        "ftp",
        "telnet",
        "nc",
        "ncat",
        "netcat",
        "socat",
        "sendmail",
        "mail",
        "mailx",
        "shutdown",
        "reboot",
        "halt",
        "mkfs",
        "dd",
    }
)
_GIT_ALWAYS = frozenset({"push", "filter-branch", "filter-repo", "update-ref", "send-email"})
_CURL_UPLOAD_FLAGS = frozenset(
    {
        "-d",
        "--data",
        "--data-raw",
        "--data-binary",
        "--data-urlencode",
        "--data-ascii",
        "--json",
        "-F",
        "--form",
        "--form-string",
        "-T",
        "--upload-file",
    }
)
_WRITE_METHODS = frozenset({"POST", "PUT", "PATCH", "DELETE"})
_FS_MUTATORS = frozenset(
    {"rm", "rmdir", "mv", "cp", "ln", "chmod", "chown", "touch", "tee", "truncate", "shred", "sed"}
)
_SHELL_SEPARATORS = frozenset({";", "&&", "||", "|", "&", "\n", "(", ")"})


def _base(token: str) -> str:
    return posixpath.basename(token.replace("\\", "/")).lower().removesuffix(".exe")


class _Ambiguous(Exception):
    """A launcher command line could not be unwrapped unambiguously (caller fails closed)."""


def _parse_opts(spec: _OptSpec, rest: list[str]) -> list[tuple[list[str], list[str]]]:
    """Split ``rest`` into (scripts, command) readings, branching on unknown options."""
    readings: list[tuple[list[str], list[str]]] = []
    # (index, scripts seen so far)
    stack: list[tuple[int, list[str]]] = [(0, [])]
    while stack:
        i, scripts = stack.pop()
        while True:
            if len(readings) + len(stack) > _MAX_CANDIDATES:
                raise _Ambiguous("too many ways to read the launcher options")
            if i >= len(rest):
                readings.append((scripts, []))
                break
            token = rest[i]
            if token == "--":
                readings.append((scripts, rest[i + 1 + spec.positional :]))
                break
            if spec.assignments and not token.startswith("-") and "=" in token:
                i += 1
                continue
            if token == "-" and spec.assignments:  # ``env -`` == ``env -i``
                i += 1
                continue
            if not token.startswith("-") or token == "-":
                readings.append((scripts, rest[i + spec.positional :]))
                break
            if spec.numeric and token[1:].isdigit():
                i += 1
                continue
            if token.startswith("--"):
                name, has_eq, value = token.partition("=")
                if has_eq:
                    if name in spec.scripts:
                        scripts = [*scripts, value]
                    i += 1
                elif name in spec.long_flags:
                    i += 1
                elif name in spec.long_values:
                    if name in spec.scripts and i + 1 < len(rest):
                        scripts = [*scripts, rest[i + 1]]
                    i += 2
                else:  # unknown: maybe a flag, maybe takes the next token
                    stack.append((i + 2, scripts))
                    i += 1
                continue
            # short option bundle, e.g. ``-iu FOO`` / ``-n10`` / ``-sd``
            step = 1
            for pos, ch in enumerate(token[1:], start=1):
                attached_value = token[pos + 1 :]
                if ch in spec.attached:
                    break
                if ch in spec.values:
                    if f"-{ch}" in spec.scripts:
                        if attached_value:
                            scripts = [*scripts, attached_value]
                        elif i + 1 < len(rest):
                            scripts = [*scripts, rest[i + 1]]
                    step = 1 if attached_value else 2
                    break
                if ch in spec.flags:
                    continue
                # unknown letter: fail closed by also reading it as value-taking
                stack.append((i + (1 if attached_value else 2), scripts))
                break
            i += step
    return readings


def _unwrap(argv: list[str], depth: int = 0) -> list[list[str]]:
    """Every plausible "real command" hidden behind launchers (env/nice/timeout/uv run/npx...).

    Launcher options that may take a value are read both ways when unknown, and script-valued
    options (``env -S``, ``npx -c``) are returned as their own candidates, so the classifier can
    fail closed. Raises ``_Ambiguous`` when the readings explode.
    """
    if depth > 8:
        raise _Ambiguous("too many nested launchers")
    if not argv:
        return [argv]
    head = _base(argv[0])
    rest = argv[1:]
    spec = _WRAPPERS.get(head)
    if spec is None and head in _RUNNERS and rest and rest[0] in _RUNNERS[head]:
        spec, rest = _RUNNER_OPTS, rest[1:]
    if spec is None and head.startswith("python") and len(rest) >= 2 and rest[0] == "-m":
        return _unwrap(rest[1:], depth + 1)
    if spec is None:
        return [argv]
    out: list[list[str]] = []
    for scripts, command in _parse_opts(spec, rest):
        out.extend(["sh", "-c", script] for script in scripts)
        out.extend(_unwrap(command, depth + 1))
        if len(out) > _MAX_CANDIDATES:
            raise _Ambiguous("too many ways to read the launcher options")
    return out


def _escapes(path: str) -> bool:
    """True for absolute, home-relative, or parent-escaping paths (and bare globs of cwd)."""
    unified = path.replace("\\", "/")
    if unified.startswith(("/", "~")) or (len(unified) > 1 and unified[1] == ":"):
        return True
    normalized = posixpath.normpath(unified)
    return normalized == ".." or normalized.startswith("../") or normalized in (".", "*")


def _touches_protected(path: str) -> bool:
    normalized = posixpath.normpath(path.replace("\\", "/"))
    first = normalized.split("/", 1)[0].casefold()
    return first in _PROTECTED


def _classify_rm(args: list[str]) -> str | None:
    flags = "".join(a.lstrip("-") for a in args if a.startswith("-") and not a.startswith("--"))
    recursive = "r" in flags.lower() or "--recursive" in args
    targets = [a for a in args if not a.startswith("-")]
    for target in targets:
        if _escapes(target):
            kind = "recursive delete" if recursive else "delete"
            return f"{kind} outside the workspace: {target!r}"
    return None


def _classify_git(args: list[str]) -> str | None:
    # Skip global options such as `-C dir` / `-c k=v`; an inline alias could rename any
    # subcommand (``git -c alias.p=push p``), so defining one is gated (fail closed).
    while args and args[0].startswith("-"):
        opt = args[0]
        if opt in ("-c", "--config-env") and len(args) > 1:
            config = args[1]
        elif opt.startswith("--config-env="):
            config = opt.split("=", 1)[1]
        else:
            config = ""
        if config.strip().lower().startswith("alias."):
            return "git alias defined on the command line (could rename any subcommand)"
        takes_value = opt in ("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env")
        args = args[2:] if takes_value else args[1:]
    if not args:
        return None
    sub, rest = args[0], args[1:]
    if sub in _GIT_ALWAYS:
        return f"git {sub} (outward-facing / rewrites history)"
    if sub == "reset" and "--hard" in rest:
        return "git reset --hard discards work"
    if sub == "clean" and any(a.startswith("-") and "f" in a.lstrip("-") for a in rest):
        return "git clean -f deletes untracked files"
    if sub == "branch" and any(a in ("-D", "--delete", "-d") for a in rest):
        return "git branch delete"
    if sub == "remote" and rest[:1] and rest[0] in ("add", "set-url", "remove", "rm"):
        return "git remote reconfiguration"
    if sub == "reflog" and rest[:1] == ["expire"]:
        return "git reflog expire destroys recovery points"
    if sub in ("rebase", "commit") and any(a in ("--amend", "--root") for a in rest):
        return f"git {sub} rewrites history"
    if sub in ("checkout", "switch", "restore") and ("-f" in rest or "--force" in rest):
        return f"git {sub} --force discards work"
    return None


# curl short options that consume a value (the rest of a bundle, or the next argument).
_CURL_SHORT_VALUES = frozenset("AbcCdDeEFHKmoPQrtTuUwxXYyz")
_CURL_UPLOAD_SHORT = frozenset("dFT")


def _classify_curl(args: list[str]) -> str | None:
    i = 0
    while i < len(args):
        arg = args[i]
        nxt = args[i + 1] if i + 1 < len(args) else ""
        if arg == "--":
            break
        if arg.startswith("--"):
            flag, has_eq, value = arg.partition("=")
            if flag in _CURL_UPLOAD_FLAGS or flag.startswith(("--data", "--form")):
                return f"curl upload ({flag})"
            if flag in ("--config",):
                return "curl config file (may contain uploads)"
            if flag == "--request":
                method = value if has_eq else nxt
                if method.upper() in _WRITE_METHODS:
                    return f"curl {method.upper()} request"
        elif arg.startswith("-") and len(arg) > 1:
            for pos, ch in enumerate(arg[1:], start=1):
                attached = arg[pos + 1 :]
                if ch in _CURL_UPLOAD_SHORT:
                    return f"curl upload (-{ch})"
                if ch == "K":
                    return "curl config file (may contain uploads)"
                if ch == "X":
                    method = attached or nxt
                    if method.upper() in _WRITE_METHODS:
                        return f"curl {method.upper()} request"
                if ch in _CURL_SHORT_VALUES:
                    if not attached:
                        i += 1  # the value is the next argument
                    break
        i += 1
    return None


def _classify_wget(args: list[str]) -> str | None:
    for i, arg in enumerate(args):
        nxt = args[i + 1] if i + 1 < len(args) else ""
        flag, has_eq, value = arg.partition("=")
        if flag in ("--post-data", "--post-file", "--body-data", "--body-file"):
            return f"wget upload ({flag})"
        if flag == "--method":
            method = value if has_eq else nxt
            if method.upper() in _WRITE_METHODS:
                return "wget write request"
        if flag in ("-e", "--execute"):
            command = (value if has_eq else nxt).lower().replace("_", "")
            if any(word in command for word in ("post", "body", "method")):
                return "wget --execute with an upload setting"
    return None


def _classify_http(tool: str, args: list[str]) -> str | None:
    if tool == "curl":
        return _classify_curl(args)
    if tool == "wget":
        return _classify_wget(args)
    return None


def _sed_in_place(args: list[str]) -> bool:
    """True for ``-i``, ``-i.bak``, bundles like ``-Ei`` / ``-ni``, and ``--in-place[=SUF]``."""
    for arg in args:
        if arg == "--":
            return False
        if arg == "--in-place" or arg.startswith("--in-place="):
            return True
        if arg.startswith("-") and not arg.startswith("--"):
            for ch in arg[1:]:
                if ch == "i":
                    return True
                if ch in "efl":  # value-taking: the rest of the bundle is the value
                    break
    return False


def _classify_simple(argv: list[str], depth: int) -> str | None:
    try:
        candidates = _unwrap(list(argv))
    except _Ambiguous as exc:
        return f"ambiguous command line ({exc})"
    for candidate in candidates:
        reason = _classify_one(candidate, depth)
        if reason:
            return reason
    return None


def _classify_one(argv: list[str], depth: int) -> str | None:
    if not argv:
        return None
    tool, args = _base(argv[0]), argv[1:]

    if tool in _PRIV:
        return f"privilege escalation ({tool})"
    if tool in _SHELLS:
        for i, arg in enumerate(args):
            if arg == "-c" or (arg.startswith("-") and not arg.startswith("--") and "c" in arg):
                if i + 1 < len(args):
                    return _classify_script(args[i + 1], depth + 1)
                return None
        return None
    if tool == "eval":
        return _classify_script(" ".join(args), depth + 1) if args else None
    if tool in _ALWAYS_GATED:
        return f"{tool} is an outward-facing / destructive tool"
    if tool == "git":
        return _classify_git(args)
    if tool in _PUBLISH:
        hit = next((a for a in args if a in _PUBLISH[tool]), None)
        if hit is not None:
            return f"{tool} {hit} (publish / deploy)"
        subs = [a for a in args if not a.startswith("-")]
        if (
            tool in ("npm", "yarn", "pnpm")
            and subs[:1] == ["run"]
            and len(subs) > 1
            and any(word in subs[1] for word in ("publish", "deploy", "release"))
        ):
            return f"{tool} run {subs[1]} (publish / deploy script)"
        return None
    if tool in ("curl", "wget"):
        return _classify_http(tool, args)
    if tool == "rsync":
        targets = [a for a in args if not a.startswith("-")]
        if any(":" in t.split("/", 1)[0] for t in targets):
            return "rsync to/from a remote host"
    if tool == "rm":
        found = _classify_rm(args)
        if found:
            return found
    if tool in _FS_MUTATORS:
        if tool == "sed" and not _sed_in_place(args):
            return None
        for arg in args:
            if not arg.startswith("-") and _touches_protected(arg):
                return f"{tool} mutates a harness-owned path ({arg!r})"
    return None


_RESERVED_PREFIX = frozenset(
    {"{", "}", "!", "if", "then", "else", "elif", "do", "while", "until", "time", "fi", "done"}
)


def _is_assignment(token: str) -> bool:
    name, eq, _ = token.partition("=")
    return bool(eq) and name.replace("_", "a").isalnum() and not name[:1].isdigit()


def _substitutions(script: str) -> list[str]:
    """Bodies of command substitutions (backticks, ``$(...)``, ``<(...)``/``>(...)``).

    Honours single quotes and backslash escapes (substitutions still run inside double quotes).
    Raises ``ValueError`` if a substitution is unterminated.
    """
    bodies: list[str] = []
    i, n = 0, len(script)
    in_single = in_double = False
    while i < n:
        ch = script[i]
        if ch == "\\" and not in_single:
            i += 2
            continue
        if ch == "'" and not in_double:
            in_single = not in_single
        elif ch == '"' and not in_single:
            in_double = not in_double
        elif not in_single and ch == "`":
            end = i + 1
            while end < n and script[end] != "`":
                end += 2 if script[end] == "\\" else 1
            if end >= n:
                raise ValueError("unterminated backtick substitution")
            bodies.append(script[i + 1 : end])
            i = end
        elif not in_single and ch in "$<>" and script[i + 1 : i + 2] == "(":
            depth, end = 1, i + 2
            while end < n and depth:
                if script[end] == "\\":
                    end += 2
                    continue
                depth += {"(": 1, ")": -1}.get(script[end], 0)
                end += 1
            if depth:
                raise ValueError("unterminated $( substitution")
            body = script[i + 2 : end - 1]
            if ch == "$" and body.startswith("(") and body.endswith(")"):
                body = ""  # $(( arithmetic ))
            if body:
                bodies.append(body)
            i = end
            continue
        i += 1
    if in_single or in_double:
        raise ValueError("unterminated quote")
    return bodies


def _classify_script(script: str, depth: int) -> str | None:
    """Classify each simple command of a ``sh -c`` script string (and its substitutions)."""
    if depth > 3:
        return "deeply nested shell invocation"
    try:
        nested = _substitutions(script)
        lexer = shlex.shlex(script, posix=True, punctuation_chars=";&|()\n")
        lexer.whitespace = " \t\r"
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return "unparseable shell script"
    for body in nested:
        reason = _classify_script(body, depth + 1)
        if reason:
            return reason
    segment: list[str] = []
    for token in [*tokens, ";"]:
        if token in _SHELL_SEPARATORS or set(token) <= set(";&|()\n"):
            reason = _classify_segment(segment, depth)
            if reason:
                return reason
            segment = []
        else:
            segment.append(token)
    return None


def _classify_segment(segment: list[str], depth: int) -> str | None:
    """Drop leading reserved words (``{``, ``if``, ``do``...) and ``VAR=value`` prefixes."""
    while segment and (segment[0] in _RESERVED_PREFIX or _is_assignment(segment[0])):
        segment = segment[1:]
    # Tokens glued to substitutions (``$(git``/```git``) were classified via ``_substitutions``.
    return _classify_simple(segment, depth) if segment else None


def classify_command(argv: Sequence[str]) -> str | None:
    """Return why ``argv`` needs a human decision (irreversible / outward-facing), else ``None``."""
    return _classify_simple([str(a) for a in argv], 0)
