"""ClaimCheck payload codec for Temporal.

Over a weeks-long run the workflow journals many activity inputs/results; large payloads would
saturate Temporal's history. This codec offloads any payload above a threshold to the object store
and journals only a small pointer (the content-addressed key), reconstituting it on decode. Wire it
into a Temporal client/worker via ``DataConverter(payload_codec=ClaimCheckCodec(...))``.

The WHOLE original ``Payload`` (data + every metadata entry) is serialized into the store, so
decode restores it byte-for-byte. This codec does not encrypt: blobs are stored in plaintext.
"""

from __future__ import annotations

from collections.abc import Sequence
from pathlib import Path

from temporalio.api.common.v1 import Payload
from temporalio.converter import PayloadCodec

from lha.persistence.object_store import LocalFileObjectStore, ObjectStore

_CLAIMCHECK_ENCODING = b"lha/claimcheck/v2"
# v1 pointers stored only the raw data and the original ``encoding``; still decodable.
_LEGACY_CLAIMCHECK_ENCODING = b"lha/claimcheck"
_DEFAULT_THRESHOLD = 32 * 1024  # 32 KiB


class ClaimCheckCodec(PayloadCodec):
    """Offloads large payloads to an object store, journaling only a pointer."""

    def __init__(
        self,
        store: ObjectStore | None = None,
        *,
        threshold_bytes: int = _DEFAULT_THRESHOLD,
        root: str | Path | None = None,
    ) -> None:
        if store is None:
            if root is None:
                raise ValueError("ClaimCheckCodec needs an object store or an explicit root")
            store = LocalFileObjectStore(root)
        self._store = store
        self._threshold = threshold_bytes

    async def encode(self, payloads: Sequence[Payload]) -> list[Payload]:
        encoded: list[Payload] = []
        for payload in payloads:
            if len(payload.data) > self._threshold:
                key = await self._store.put(payload.SerializeToString(deterministic=True))
                encoded.append(
                    Payload(metadata={"encoding": _CLAIMCHECK_ENCODING}, data=key.encode("ascii"))
                )
            else:
                encoded.append(payload)
        return encoded

    async def decode(self, payloads: Sequence[Payload]) -> list[Payload]:
        decoded: list[Payload] = []
        for payload in payloads:
            encoding = payload.metadata.get("encoding")
            if encoding == _CLAIMCHECK_ENCODING:
                blob = await self._store.get(payload.data.decode("ascii"))
                original = Payload()
                original.ParseFromString(blob)
                decoded.append(original)
            elif encoding == _LEGACY_CLAIMCHECK_ENCODING:
                data = await self._store.get(payload.data.decode("ascii"))
                orig_encoding = payload.metadata.get("lha-orig-encoding", b"")
                decoded.append(Payload(metadata={"encoding": orig_encoding}, data=data))
            else:
                decoded.append(payload)
        return decoded
