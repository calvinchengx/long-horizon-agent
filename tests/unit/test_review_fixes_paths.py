"""Regression tests for the FIFO file-tool review findings."""

from __future__ import annotations

import os
import threading
from pathlib import Path
from typing import Any

import pytest

from lha.execution.paths import read_text_within, write_text_within

# --- 7. FIFOs never block file tools ---------------------------------------------------------


def _in_thread(fn: Any) -> BaseException | None:
    """Run ``fn`` in a daemon thread; fail the test if it blocks."""
    outcome: list[BaseException | None] = []

    def target() -> None:
        try:
            fn()
            outcome.append(None)
        except BaseException as exc:
            outcome.append(exc)

    thread = threading.Thread(target=target, daemon=True)
    thread.start()
    thread.join(timeout=5)
    assert not thread.is_alive(), "file tool blocked on a FIFO"
    return outcome[0]


@pytest.mark.skipif(not hasattr(os, "mkfifo"), reason="needs POSIX FIFOs")
def test_read_file_rejects_fifo_without_blocking(tmp_path: Path) -> None:
    os.mkfifo(tmp_path / "pipe")
    exc = _in_thread(lambda: read_text_within(tmp_path, "pipe"))
    assert isinstance(exc, OSError)


@pytest.mark.skipif(not hasattr(os, "mkfifo"), reason="needs POSIX FIFOs")
def test_write_file_rejects_fifo_without_blocking(tmp_path: Path) -> None:
    os.mkfifo(tmp_path / "pipe")
    exc = _in_thread(lambda: write_text_within(tmp_path, "pipe", "x"))
    assert isinstance(exc, OSError)


def test_regular_files_still_read_and_write(tmp_path: Path) -> None:
    write_text_within(tmp_path, "a/b.txt", "hello")
    assert read_text_within(tmp_path, "a/b.txt") == "hello"
