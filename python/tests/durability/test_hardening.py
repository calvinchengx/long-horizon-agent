"""Tests for durable-hardening: object store, ClaimCheck codec, saga, CAN reconciliation."""

from __future__ import annotations

from pathlib import Path

import pytest
from temporalio.api.common.v1 import Payload

from lha.durable.codec import ClaimCheckCodec
from lha.durable.reconcile import reconcile_in_flight
from lha.durable.saga import Saga
from lha.persistence.object_store import (
    InvalidObjectKeyError,
    LocalFileObjectStore,
    ObjectCorruptError,
)
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
    assert encoded[0].metadata["encoding"] == b"lha/claimcheck/v2"
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
    git_ops.run_git(tmp_path, "checkout", "-q", "-b", "feature/t1")
    (tmp_path / "work.txt").write_text("real work", encoding="utf-8")
    git_ops.commit_all(tmp_path, "t1 work")
    git_ops.run_git(tmp_path, "checkout", "-q", "main")

    actions = reconcile_in_flight(
        str(tmp_path),
        [("t1", "feature/t1"), ("t2", "feature/missing"), ("t3", None)],
    )
    by_id = {a.ticket_id: a.action for a in actions}
    assert by_id["t1"] == "adopt"
    assert by_id["t2"] == "respawn"
    assert by_id["t3"] == "respawn"


def _repo_with_remote(tmp_path: Path) -> Path:
    remote = tmp_path / "remote.git"
    git_ops.run_git(tmp_path, "init", "-q", "--bare", str(remote))
    work = tmp_path / "work"
    git_ops.init_repo(work)
    (work / "f.txt").write_text("x", encoding="utf-8")
    git_ops.commit_all(work, "init")
    git_ops.run_git(work, "remote", "add", "origin", str(remote))
    git_ops.run_git(work, "push", "-q", "origin", "main")
    return work


def test_reconcile_adopts_remote_tracking_branch(tmp_path: Path) -> None:
    work = _repo_with_remote(tmp_path)
    git_ops.run_git(work, "checkout", "-q", "-b", "feature/t1")
    (work / "t1.txt").write_text("pushed work", encoding="utf-8")
    git_ops.commit_all(work, "t1 work")
    git_ops.run_git(work, "push", "-q", "origin", "feature/t1")
    git_ops.run_git(work, "checkout", "-q", "main")
    git_ops.run_git(work, "branch", "-q", "-D", "feature/t1")  # only origin/feature/t1 remains

    [action] = reconcile_in_flight(str(work), [("t1", "feature/t1")])
    assert action.action == "adopt"
    assert "origin/feature/t1" in action.detail


def test_reconcile_respawns_branch_without_commits(tmp_path: Path) -> None:
    git_ops.init_repo(tmp_path)
    (tmp_path / "f.txt").write_text("x", encoding="utf-8")
    git_ops.commit_all(tmp_path, "init")
    git_ops.run_git(tmp_path, "branch", "feature/empty")  # created, never committed to

    [action] = reconcile_in_flight(str(tmp_path), [("t1", "feature/empty")])
    assert action.action == "respawn"
    assert "no commits" in action.detail


@pytest.mark.asyncio
async def test_saga_compensate_is_idempotent() -> None:
    calls: list[str] = []
    saga = Saga()

    async def undo() -> None:
        calls.append("undo")

    saga.add("a", undo)
    assert await saga.compensate() == ["a"]
    assert await saga.compensate() == []  # nothing left: no double compensation
    assert calls == ["undo"]
    assert len(saga) == 0


@pytest.mark.asyncio
async def test_claimcheck_preserves_all_metadata(tmp_path: Path) -> None:
    codec = ClaimCheckCodec(LocalFileObjectStore(tmp_path / "obj"), threshold_bytes=10)
    big = Payload(
        metadata={"encoding": b"json/protobuf", "messageType": b"temporal.X", "x-extra": b"1"},
        data=b"y" * 200,
    )
    [encoded] = await codec.encode([big])
    assert set(encoded.metadata) == {"encoding"}  # only the pointer is journaled
    [decoded] = await codec.decode([encoded])
    assert decoded == big  # data AND every metadata entry restored


@pytest.mark.asyncio
async def test_claimcheck_decodes_legacy_v1_pointers(tmp_path: Path) -> None:
    store = LocalFileObjectStore(tmp_path / "obj")
    key = await store.put(b"legacy-bytes")
    legacy = Payload(
        metadata={"encoding": b"lha/claimcheck", "lha-orig-encoding": b"json/plain"},
        data=key.encode("ascii"),
    )
    [decoded] = await ClaimCheckCodec(store).decode([legacy])
    assert decoded.data == b"legacy-bytes"
    assert decoded.metadata["encoding"] == b"json/plain"


def test_claimcheck_requires_explicit_store_or_root() -> None:
    with pytest.raises(ValueError):
        ClaimCheckCodec()


@pytest.mark.asyncio
async def test_object_store_rejects_bad_keys_and_corruption(tmp_path: Path) -> None:
    store = LocalFileObjectStore(tmp_path / "obj")
    key = await store.put(b"payload")
    for bad in ["../../etc/passwd", "ABC", key.upper(), key + "0", ""]:
        with pytest.raises(InvalidObjectKeyError):
            await store.get(bad)
    (tmp_path / "obj" / key).write_bytes(b"tampered")
    with pytest.raises(ObjectCorruptError):
        await store.get(key)


@pytest.mark.asyncio
async def test_object_store_root_is_absolute_and_put_is_atomic(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.chdir(tmp_path)
    store = LocalFileObjectStore("rel/objects")
    assert store.root == (tmp_path / "rel" / "objects").resolve()
    monkeypatch.chdir("/")  # a later chdir must not move the store
    key = await store.put(b"data")
    assert (tmp_path / "rel" / "objects" / key).read_bytes() == b"data"
    assert [p.name for p in store.root.iterdir()] == [key]  # no temp files left behind


def test_object_store_requires_root() -> None:
    with pytest.raises(ValueError):
        LocalFileObjectStore("")
