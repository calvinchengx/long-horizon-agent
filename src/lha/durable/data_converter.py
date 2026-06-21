"""Data converter that installs the ClaimCheck payload codec.

Wiring the codec here means every workflow/activity payload is automatically offloaded to the
object store when large — keeping Temporal history bounded over a weeks-long run — transparently to
all workflow/activity code. The client, every worker AND the replay harness must use the same
converter (same object store), or large payloads cannot be decoded.
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

from temporalio.converter import DataConverter, default

from lha.durable.codec import ClaimCheckCodec


def build_data_converter(*, object_store_root: str | Path) -> DataConverter:
    """Return the default DataConverter augmented with the ClaimCheck payload codec.

    ``object_store_root`` is resolved to an absolute path (pass ``settings.object_store_root``).
    """
    return dataclasses.replace(default(), payload_codec=ClaimCheckCodec(root=object_store_root))
