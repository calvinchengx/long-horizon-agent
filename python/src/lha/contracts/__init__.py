"""Plane-crossing contracts.

These ``Protocol`` definitions are the seams that keep the system swappable: the durable
spine depends only on these interfaces, never on a concrete model/sandbox/store. That is
what lets the model layer flip between stub / Ollama / free-tier / Claude with one config
line, and what makes the same architecture work across domains via a custom ``Verifier``.
"""

from lha.contracts.mission import Mission, MissionSpec
from lha.contracts.model import (
    ModelMessage,
    ModelProvider,
    ToolCall,
    TurnResult,
    Usage,
)
from lha.contracts.sandbox import ExecResult, Sandbox, SandboxSession, Snapshot
from lha.contracts.state import (
    Checklist,
    ChecklistItem,
    Checkpoint,
    DecisionRecord,
    DurableState,
    EventRecord,
    SituationSnapshot,
)
from lha.contracts.verify import Check, CheckResult, VerificationResult, Verifier

__all__ = [
    "Check",
    "CheckResult",
    "Checklist",
    "ChecklistItem",
    "Checkpoint",
    "DecisionRecord",
    "DurableState",
    "EventRecord",
    "ExecResult",
    "Mission",
    "MissionSpec",
    "ModelMessage",
    "ModelProvider",
    "Sandbox",
    "SandboxSession",
    "SituationSnapshot",
    "Snapshot",
    "ToolCall",
    "TurnResult",
    "Usage",
    "VerificationResult",
    "Verifier",
]
