"""Persistence adapters: object store (blob spillover), and (later) Postgres repositories."""

from lha.persistence.object_store import LocalFileObjectStore, ObjectStore

__all__ = ["LocalFileObjectStore", "ObjectStore"]
