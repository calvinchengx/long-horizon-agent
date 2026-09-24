"""Tests for durable-hardening: object store and ClaimCheck codec."""

from __future__ import annotations

from pathlib import Path

import pytest
from temporalio.api.common.v1 import Payload

from lha.durable.codec import ClaimCheckCodec
from lha.persistence.object_store import (
    InvalidObjectKeyError,
    LocalFileObjectStore,
    ObjectCorruptError,
)


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
