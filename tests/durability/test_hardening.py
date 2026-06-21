"""Tests for durable-hardening: object store, ClaimCheck codec, saga, CAN reconciliation."""

from __future__ import annotations

from pathlib import Path

import pytest
from temporalio.api.common.v1 import Payload

from lha.durable.codec import ClaimCheckCodec
from lha.durable.reconcile import reconcile_in_flight
from lha.durable.saga import Saga
from lha.persistence.object_store import LocalFileObjectStore
from lha.state import git_ops


@pytest.mark.asyncio
async def test_object_store_roundtrip_and_dedup(tmp_path: Path) -> None:
    store = LocalFileObjectStore(str(tmp_path / "obj"))
    k1 = await store.put(b"hello")
    k2 = await store.put(b"hello")
    assert k1 == k2  # content-addressed dedup
    assert await store.get(k1) == b"hello"


@pytest.mark.asyncio
async def test_claimcheck_offloads_large_payloads(tmp_path: Path) -> None:
    codec = ClaimCheckCodec(LocalFileObjectStore(str(tmp_path / "obj")), threshold_bytes=10)
    big = Payload(metadata={"encoding": b"json/plain"}, data=b"x" * 100)
    small = Payload(metadata={"encoding": b"json/plain"}, data=b"tiny")

    encoded = await codec.encode([big, small])
    assert encoded[0].metadata["encoding"] == b"lha/claimcheck"
    assert len(encoded[0].data) < 100  # replaced by a pointer key
    assert encoded[1].data == b"tiny"  # small payload untouched

    decoded = await codec.decode(encoded)
    assert decoded[0].data == b"x" * 100
    assert decoded[0].metadata["encoding"] == b"json/plain"
    assert decoded[1].data == b"tiny"


@pytest.mark.asyncio
async def test_saga_runs_compensations_lifo() -> None:
    order: list[str] = []
    saga = Saga()

    async def undo_a() -> None:
        order.append("a")

    async def undo_b() -> None:
        order.append("b")

    saga.add("a", undo_a)
    saga.add("b", undo_b)
    ran = await saga.compensate()
    assert ran == ["b", "a"]
    assert order == ["b", "a"]


@pytest.mark.asyncio
async def test_saga_continues_on_undo_failure() -> None:
    saga = Saga()

    async def ok() -> None:
        return None

    async def boom() -> None:
        raise RuntimeError("nope")

    saga.add("ok", ok)
    saga.add("boom", boom)
    ran = await saga.compensate()
    assert "ok" in ran
    assert "boom" not in ran
    assert saga.failures


def test_reconcile_adopts_existing_respawns_missing(tmp_path: Path) -> None:
    git_ops.init_repo(tmp_path)
    (tmp_path / "f.txt").write_text("x", encoding="utf-8")
    git_ops.commit_all(tmp_path, "init")
    git_ops.run_git(tmp_path, "branch", "feature/t1")

    actions = reconcile_in_flight(
        str(tmp_path),
        [("t1", "feature/t1"), ("t2", "feature/missing"), ("t3", None)],
    )
    by_id = {a.ticket_id: a.action for a in actions}
    assert by_id["t1"] == "adopt"
    assert by_id["t2"] == "respawn"
    assert by_id["t3"] == "respawn"
