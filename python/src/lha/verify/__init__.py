"""The verification plane: the deterministic gate that decides what "done" means."""

from lha.contracts.verify import checks_from_commands, derive_check_name, ensure_unique_check_names
from lha.verify.flaky_quarantine import FlakeEvidenceError, FlakyQuarantine, FlakyRetryVerifier
from lha.verify.harness_integrity import harness_violations, snapshot_harness
from lha.verify.trusted import (
    CommandTrustedRunner,
    TrustedAwareVerifier,
    TrustedRunner,
    candidate_commit,
)
from lha.verify.verifier import DeterministicVerifier, clip_output_tail, default_python_checks
from lha.verify.witnesses import (
    UnknownTrustedCheck,
    item_checks,
    parse_witness,
    validate_witness,
)

__all__ = [
    "CommandTrustedRunner",
    "DeterministicVerifier",
    "FlakeEvidenceError",
    "FlakyQuarantine",
    "FlakyRetryVerifier",
    "TrustedAwareVerifier",
    "TrustedRunner",
    "UnknownTrustedCheck",
    "candidate_commit",
    "checks_from_commands",
    "clip_output_tail",
    "default_python_checks",
    "derive_check_name",
    "ensure_unique_check_names",
    "harness_violations",
    "item_checks",
    "parse_witness",
    "snapshot_harness",
    "validate_witness",
]
