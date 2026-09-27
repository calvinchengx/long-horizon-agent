"""The real E2B SDK (the ``e2b`` extra) still has every call the adapter makes.

The adapter's behaviour is tested against a fake SDK (``test_sandbox_e2b.py``); this pins the fake
to the real one, so an SDK upgrade that renames or drops something fails here, not at runtime.
Skipped when the extra is not installed.
"""

from __future__ import annotations

import inspect

import pytest

e2b = pytest.importorskip("e2b")
e2b_code_interpreter = pytest.importorskip("e2b_code_interpreter")


def _params(fn: object) -> set[str]:
    return set(inspect.signature(fn).parameters)  # type: ignore[arg-type]


def test_async_sandbox_lifecycle() -> None:
    sandbox = e2b_code_interpreter.AsyncSandbox
    create = inspect.signature(sandbox.create)
    assert next(iter(create.parameters)) == "template"  # create(template) positionally
    assert callable(sandbox.kill)


def test_commands_and_files_signatures() -> None:
    from e2b.sandbox_async.commands.command import Commands
    from e2b.sandbox_async.filesystem.filesystem import Filesystem

    assert {"cmd", "cwd", "timeout", "envs"} <= _params(Commands.run)
    assert {"path", "format"} <= _params(Filesystem.read)
    assert "bytes" in str(inspect.signature(Filesystem.read).parameters["format"].annotation)
    assert {"path", "data"} <= _params(Filesystem.write)


def test_exception_names_the_adapter_matches() -> None:
    # sandbox_e2b.py maps these by class name (TimeoutException) or attribute (exit_code).
    assert e2b.TimeoutException.__name__ == "TimeoutException"
    assert "exit_code" in _params(e2b.CommandExitException.__init__)
