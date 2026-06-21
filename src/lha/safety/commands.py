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

_WRAPPERS_SIMPLE = frozenset({"nohup", "nice", "time", "command", "exec", "stdbuf", "ionice"})
_RUNNERS = {  # launcher -> subcommands meaning "run the command that follows"
    "uv": ("run",),
    "poetry": ("run",),
    "pipenv": ("run",),
    "pdm": ("run",),
    "hatch": ("run",),
    "pnpm": ("exec", "dlx"),
    "yarn": ("exec", "dlx"),
    "bundle": ("exec",),
}
_DIRECT_RUNNERS = frozenset({"npx", "bunx", "uvx", "pipx"})
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


def _strip_wrappers(argv: list[str]) -> list[str]:
    """Peel launchers (env/nohup/timeout/uv run/npx/python -m ...) off the front of ``argv``."""
    while argv:
        head = _base(argv[0])
        rest = argv[1:]
        if head == "env":
            while rest and (rest[0].startswith("-") or "=" in rest[0]):
                rest = rest[1:]
        elif head in _WRAPPERS_SIMPLE:
            while rest and rest[0].startswith("-"):
                rest = rest[1:]
        elif head == "timeout":
            while rest and rest[0].startswith("-"):
                rest = rest[2:] if rest[0] in ("-s", "-k", "--signal", "--kill-after") else rest[1:]
            rest = rest[1:]  # the duration
        elif head == "xargs" or head in _DIRECT_RUNNERS:
            while rest and rest[0].startswith("-"):
                rest = rest[1:]
        elif head in _RUNNERS and rest and rest[0] in _RUNNERS[head]:
            rest = rest[1:]
            while rest and rest[0].startswith("-"):
                rest = rest[1:]
        elif head.startswith("python") and len(rest) >= 2 and rest[0] == "-m":
            rest = rest[1:]
        else:
            return argv
        argv = rest
    return argv


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
    # Skip global options such as `-C dir` / `-c k=v`.
    while args and args[0].startswith("-"):
        args = args[2:] if args[0] in ("-C", "-c", "--git-dir", "--work-tree") else args[1:]
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


def _classify_http(tool: str, args: list[str]) -> str | None:
    if tool == "curl":
        for i, arg in enumerate(args):
            flag = arg.split("=", 1)[0]
            if flag in _CURL_UPLOAD_FLAGS or (
                arg.startswith("-") and not arg.startswith("--") and arg[1:2] in ("d", "F", "T")
            ):
                return f"curl upload ({flag})"
            method = ""
            if arg in ("-X", "--request") and i + 1 < len(args):
                method = args[i + 1]
            elif arg.startswith("--request="):
                method = arg.split("=", 1)[1]
            elif arg.startswith("-X") and len(arg) > 2:
                method = arg[2:]
            if method.upper() in _WRITE_METHODS:
                return f"curl {method.upper()} request"
    if tool == "wget":
        for arg in args:
            flag = arg.split("=", 1)[0]
            if flag in ("--post-data", "--post-file", "--body-data", "--body-file"):
                return f"wget upload ({flag})"
            if flag == "--method" and arg.split("=", 1)[-1].upper() in _WRITE_METHODS:
                return "wget write request"
    return None


def _classify_simple(argv: list[str], depth: int) -> str | None:
    argv = _strip_wrappers(list(argv))
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
        if tool == "sed" and not any(a.startswith("-i") or a == "--in-place" for a in args):
            return None
        for arg in args:
            if not arg.startswith("-") and _touches_protected(arg):
                return f"{tool} mutates a harness-owned path ({arg!r})"
    return None


def _classify_script(script: str, depth: int) -> str | None:
    """Classify each simple command of a ``sh -c`` script string."""
    if depth > 3:
        return "deeply nested shell invocation"
    try:
        lexer = shlex.shlex(script, posix=True, punctuation_chars=";&|()")
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return "unparseable shell script"
    segment: list[str] = []
    for token in [*tokens, ";"]:
        if token in _SHELL_SEPARATORS or set(token) <= set(";&|()"):
            reason = _classify_simple(segment, depth) if segment else None
            if reason:
                return reason
            segment = []
        else:
            segment.append(token)
    return None


def classify_command(argv: Sequence[str]) -> str | None:
    """Return why ``argv`` needs a human decision (irreversible / outward-facing), else ``None``."""
    return _classify_simple([str(a) for a in argv], 0)
